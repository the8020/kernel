package daemon

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"the8020/kernel/logging/records"
)

func testPolicy() records.Policy {
	return records.Policy{Enabled: true, Level: "info", SplitBy: "none", SplitPeriod: "none", MaxFileSize: 64 * 1024, MaxTotalSize: 70 * 1024, MaxAge: time.Hour}
}
func testStore(t *testing.T, p records.Policy) *store {
	t.Helper()
	s, err := openStore(t.TempDir(), p, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func logLine(source string, n int) []byte {
	line, _ := records.Encode(records.Record{Time: time.Now().UTC(), Level: "INFO", Source: source, Component: "test", NodeID: "nod-0123456789", Message: strings.Repeat("x", n)})
	return line
}
func appendLines(t *testing.T, s *store, source string, now time.Time, lines ...[]byte) {
	t.Helper()
	n, err := s.appendBatch(source, lines, now)
	if err != nil || n != len(lines) {
		t.Fatalf("append: %d/%d %v", n, len(lines), err)
	}
}
func actualTotal(t *testing.T, s *store) int64 {
	t.Helper()
	var total int64
	for _, seg := range s.segments {
		stat, err := os.Stat(seg.path)
		if err != nil {
			t.Fatal(err)
		}
		if stat.Size() != seg.size {
			t.Fatalf("segment accounting: %d != %d", stat.Size(), seg.size)
		}
		total += stat.Size()
	}
	if total != s.total {
		t.Fatalf("total accounting: %d != %d", total, s.total)
	}
	return total
}

func TestTotalRetentionDuringOrdinaryWrites(t *testing.T) {
	s := testStore(t, testPolicy())
	now := time.Now().UTC()
	old := s.active["all"].path
	for i := 0; i < 4; i++ {
		appendLines(t, s, "kernel", now, logLine("kernel", 14800))
	}
	appendLines(t, s, "kernel", now, logLine("kernel", 9900))
	current := s.active["all"].id
	if _, err := os.Stat(old); err != nil {
		t.Fatal("premature retention", err)
	}
	appendLines(t, s, "kernel", now, logLine("kernel", 9900))
	if s.active["all"].id != current {
		t.Fatal("ordinary retention unnecessarily rotated current file")
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("total limit did not remove closed segment", err)
	}
	if actualTotal(t, s) > s.policy.MaxTotalSize {
		t.Fatal("exceeded total")
	}
	// A file introduced outside the writer's ownership index is not discovered
	// by an ordinary append or retention tick; no directory scan is on that path.
	external := filepath.Join(s.directory, "segment-00000000000000999999-all.log")
	if err := os.WriteFile(external, bytes.Repeat([]byte{'x'}, 100000), 0600); err != nil {
		t.Fatal(err)
	}
	appendLines(t, s, "kernel", now, logLine("kernel", 10))
	if err := s.maintain(now, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatal("ordinary path rescanned directory", err)
	}
}

func TestGlobalRetentionIncludesEveryActiveSource(t *testing.T) {
	p := testPolicy()
	p.SplitBy = "source"
	p.MaxFileSize = records.MaxRecord
	p.MaxTotalSize = 22 * 1024
	s := testStore(t, p)
	now := time.Now().UTC()
	old := s.active["kernel"].path
	for _, source := range []string{"kernel", "deno", "logd"} {
		appendLines(t, s, source, now, logLine(source, 9000))
		if actualTotal(t, s) > p.MaxTotalSize {
			t.Fatal("active files exceeded global budget")
		}
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old active source not safely rotated for retention", err)
	}
	for _, seg := range s.active {
		if _, err := os.Stat(seg.path); err != nil {
			t.Fatal("deleted active file", err)
		}
	}
}

func TestIdleAgeExpiryAndDisabledRetention(t *testing.T) {
	p := testPolicy()
	p.MaxAge = time.Minute
	s := testStore(t, p)
	now := time.Now().UTC()
	appendLines(t, s, "kernel", now, logLine("kernel", 100))
	old := s.active["all"].path
	if err := s.maintain(now.Add(time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("idle active segment did not expire", err)
	}
	if s.total != 0 {
		t.Fatal("expiry accounting")
	}
	counter := s.counter
	if err := s.maintain(now.Add(10*time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	if s.counter != counter {
		t.Fatal("empty idle output rotated repeatedly")
	}
	appendLines(t, s, "kernel", now, logLine("kernel", 100))
	old = s.active["all"].path
	p.Enabled = false
	prepared, err := s.prepare(p, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = prepared.commit(now); err != nil {
		t.Fatal(err)
	}
	if len(s.active) != 0 {
		t.Fatal("disabled outputs remain open")
	}
	if _, err = os.Stat(old); err != nil {
		t.Fatal("disable removed still-retained records", err)
	}
	if err = s.maintain(now.Add(2*time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disabled retention stopped", err)
	}
}

func TestFailedRotationPreservesCurrentOutputAndRecovers(t *testing.T) {
	p := testPolicy()
	p.SplitPeriod = "minute"
	s := testStore(t, p)
	now := time.Now().UTC()
	appendLines(t, s, "kernel", now, logLine("kernel", 50))
	old := s.active["all"]
	originalOpen := s.openFile
	s.openFile = func(string, int, os.FileMode) (outputFile, error) { return nil, os.ErrPermission }
	if n, err := s.appendBatch("kernel", [][]byte{logLine("kernel", 50)}, now.Add(time.Minute)); n != 0 || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("rotation failure: %d %v", n, err)
	}
	if s.active["all"] != old || old.file == nil {
		t.Fatal("failed replacement abandoned usable output")
	}
	if err := s.sync(); err != nil {
		t.Fatal("current output no longer usable", err)
	}
	s.openFile = originalOpen
	appendLines(t, s, "kernel", now.Add(time.Minute), logLine("kernel", 50))
	if s.active["all"] == old || old.file != nil {
		t.Fatal("rotation did not recover")
	}
	actualTotal(t, s)
}

type faultFile struct {
	outputFile
	failWrite, failTruncate, failSync bool
}

func (f *faultFile) Write(p []byte) (int, error) {
	if !f.failWrite {
		return f.outputFile.Write(p)
	}
	n, err := f.outputFile.Write(p[:min(17, len(p))])
	return n, errors.Join(err, syscall.ENOSPC)
}
func (f *faultFile) Truncate(n int64) error {
	if f.failTruncate {
		return syscall.EIO
	}
	return f.outputFile.Truncate(n)
}
func (f *faultFile) Sync() error {
	if f.failSync {
		return syscall.EIO
	}
	return f.outputFile.Sync()
}

func TestPartialBatchRollbackAndRecovery(t *testing.T) {
	for _, rollbackFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "seal"}[rollbackFails], func(t *testing.T) {
			s := testStore(t, testPolicy())
			now := time.Now().UTC()
			first := logLine("kernel", 100)
			appendLines(t, s, "kernel", now, first)
			old := s.active["all"]
			f := &faultFile{outputFile: old.file, failWrite: true, failTruncate: rollbackFails}
			old.file = f
			n, err := s.appendBatch("kernel", [][]byte{logLine("kernel", 200)}, now)
			if n != 0 || !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("partial failure: %d %v", n, err)
			}
			data, err := os.ReadFile(old.path)
			if err != nil {
				t.Fatal(err)
			}
			if !rollbackFails && !bytes.Equal(data, first) {
				t.Fatal("partial record remained after rollback")
			}
			if rollbackFails && old.file != nil {
				t.Fatal("damaged output remained appendable")
			}
			actualTotal(t, s)
			f.failWrite = false
			appendLines(t, s, "kernel", now, logLine("kernel", 100))
			actualTotal(t, s)
		})
	}
}

func TestDirtySyncFailureRemainsRetryable(t *testing.T) {
	s := testStore(t, testPolicy())
	appendLines(t, s, "kernel", time.Now().UTC(), logLine("kernel", 100))
	seg := s.active["all"]
	f := &faultFile{outputFile: seg.file, failSync: true}
	seg.file = f
	if err := s.sync(); !errors.Is(err, syscall.EIO) || !seg.dirty {
		t.Fatal("sync failure lost dirty state", err)
	}
	f.failSync = false
	if err := s.sync(); err != nil || seg.dirty {
		t.Fatal("dirty sync did not recover", err)
	}
}

func TestPolicyPreparationDiscardAndAtomicPublication(t *testing.T) {
	s := testStore(t, testPolicy())
	now := time.Now().UTC()
	appendLines(t, s, "kernel", now, logLine("kernel", 100))
	old := s.active["all"]
	p := s.policy
	p.SplitBy = "source"
	originalOpen := s.openFile
	s.openFile = func(string, int, os.FileMode) (outputFile, error) { return nil, os.ErrPermission }
	if _, err := s.prepare(p, now); !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	if s.policy.SplitBy != "none" || s.active["all"] != old {
		t.Fatal("failed prepare changed live policy")
	}
	s.openFile = originalOpen
	prepared, err := s.prepare(p, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.appendBatch("kernel", [][]byte{logLine("kernel", 100)}, now); !errors.Is(err, errPolicyPrepared) {
		t.Fatal("writer did not hold bounded intake during policy preparation", err)
	}
	paths := make([]string, len(prepared.files))
	for i, seg := range prepared.files {
		paths[i] = seg.path
	}
	if err = prepared.discard(); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("discard left empty output", err)
		}
	}
	appendLines(t, s, "kernel", now, logLine("kernel", 100))
	prepared, err = s.prepare(p, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = prepared.commit(now); err != nil {
		t.Fatal(err)
	}
	if s.policy != p || len(s.active) != 3 || s.active["all"] != nil || old.file != nil {
		t.Fatal("policy did not switch all streams together")
	}
	appendLines(t, s, "deno", now, logLine("deno", 100))
	actualTotal(t, s)
}

func TestOwnedFileRulesAndSegmentCounterSurviveCompleteExpiry(t *testing.T) {
	directory := t.TempDir()
	outside := filepath.Join(t.TempDir(), "keep.log")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"notes.log", "kernel-old.log", "segment-not-owned-all.log"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(directory, "segment-00000000000000000999-all.log")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	p := testPolicy()
	p.MaxAge = time.Second
	now := time.Now().UTC()
	s, err := openStore(directory, p, now)
	if err != nil {
		t.Fatal(err)
	}
	appendLines(t, s, "kernel", now, logLine("kernel", 100))
	counter := s.counter
	p.Enabled = false
	prepared, err := s.prepare(p, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = prepared.commit(now); err != nil {
		t.Fatal(err)
	}
	if err = s.maintain(now.Add(2*time.Second), 0); err != nil {
		t.Fatal(err)
	}
	if len(s.segments) != 0 {
		t.Fatal("closed segments did not fully expire")
	}
	if err = s.close(); err != nil {
		t.Fatal(err)
	}
	p.Enabled = true
	s, err = openStore(directory, p, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if s.counter <= counter || s.counter >= 999 {
		t.Fatal("counter reused or symlink treated as owned", s.counter, counter)
	}
	for _, name := range []string{"notes.log", "kernel-old.log", "segment-not-owned-all.log"} {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil || string(data) != "keep" {
			t.Fatal("unrelated file changed", name, err)
		}
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Fatal("symlink target changed", err)
	}
}
