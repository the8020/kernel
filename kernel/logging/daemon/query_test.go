package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"the8020/kernel/logging/records"
)

func queryLine(source, level, message string, at time.Time) []byte {
	line, err := records.Encode(records.Record{Time: at, Level: level, Source: source, Component: "test", NodeID: "nod-0123456789", SandboxID: "sbx-0123456789", WorkerID: "wrk-0123456789", ContextID: "ctx-0123456789", JobID: "job-0123456789", Object: "program:acme/jobs/test", Message: message})
	if err != nil {
		panic(err)
	}
	return line
}

func queryPage(t *testing.T, s *store, q records.Query) records.Page {
	t.Helper()
	page, err := s.query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(page)
	if len(encoded) > records.MaxQueryBytes || len(page.Records) > records.MaxQueryRecords || page.ScannedBytes > records.MaxQueryScan {
		t.Fatal("query exceeded bounds", len(encoded), len(page.Records), page.ScannedBytes)
	}
	return page
}

func TestExecutionUsernameFilterAndCursorBinding(t *testing.T) {
	s := testStore(t, testPolicy())
	now := time.Now().UTC()
	for _, username := range []string{"alice", "bob", ""} {
		r := records.Record{Time: now, Level: "INFO", Source: "deno", Component: "worker", NodeID: "nod-0123456789", Username: username, Object: "service:acme/app/login", Message: "executing"}
		line, err := records.Encode(r)
		if err != nil {
			t.Fatal(err)
		}
		appendLines(t, s, "deno", now, line)
	}
	q := records.Query{Filter: records.Filter{Username: "alice"}}
	page := queryPage(t, s, q)
	if len(page.Records) != 1 || page.Records[0].Username != "alice" {
		t.Fatal("wrong invocation user", page)
	}
	q.Cursor, q.Username = page.Cursor, "bob"
	if _, err := s.query(context.Background(), q); err == nil {
		t.Fatal("username change reused cursor")
	}
	q.Cursor = ""
	page = queryPage(t, s, q)
	if len(page.Records) != 1 || page.Records[0].Username != "bob" {
		t.Fatal(page)
	}
}

func TestStartingPositionSkipsPriorHistoryAcrossRotationAndRestart(t *testing.T) {
	p := testPolicy()
	p.MaxTotalSize = 1 << 20
	s := testStore(t, p)
	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		appendLines(t, s, "deno", now, queryLine("deno", "INFO", strings.Repeat("older", 1000), now))
	}
	start := s.readPosition()
	first := queryLine("deno", "INFO", "first selected message", now)
	appendLines(t, s, "deno", now, first)
	if _, err := s.rotate("all", now); err != nil {
		t.Fatal(err)
	}
	second := queryLine("deno", "ERROR", "second selected message", now)
	appendLines(t, s, "deno", now, second)
	q := records.Query{Position: start, Filter: records.Filter{JobID: "job-0123456789"}, Limit: 1}
	page := queryPage(t, s, q)
	if len(page.Records) != 1 || page.Records[0].Message != "first selected message" || page.ScannedBytes > len(first)+len(second) {
		t.Fatal("starting reference read unrelated history", page)
	}
	q.Position, q.Cursor = "", page.Cursor
	page = queryPage(t, s, q)
	if len(page.Records) != 1 || page.Records[0].Message != "second selected message" {
		t.Fatal("continuation lost selected logs", page)
	}
	if _, err := s.query(context.Background(), records.Query{Position: page.Cursor}); err == nil {
		t.Fatal("a filter-bound continuation was accepted as a starting reference")
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(s.directory, p, now)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	q = records.Query{Position: start, Filter: records.Filter{Level: "error"}}
	page = queryPage(t, reopened, q)
	if len(page.Records) != 1 || page.Records[0].Message != "second selected message" {
		t.Fatal("restart or a new filter invalidated a starting position", page)
	}
	for _, invalid := range []records.Query{{Position: start, Tail: true}, {Position: start, Cursor: page.Cursor}, {Position: strings.Repeat("x", 4097)}} {
		if _, err := reopened.query(context.Background(), invalid); err == nil {
			t.Fatal("conflicting or oversized starting position was accepted")
		}
	}
}

func TestQueryFiltersLimitsAndContinuationAcrossRotation(t *testing.T) {
	p := testPolicy()
	p.MaxTotalSize = 1024 * 1024
	s := testStore(t, p)
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		source, level := "deno", "ERROR"
		if i == 1 {
			source = "kernel"
		}
		if i == 3 {
			level = "INFO"
		}
		appendLines(t, s, source, now, queryLine(source, level, fmt.Sprint(i), now.Add(time.Duration(i)*time.Millisecond)))
	}
	q := records.Query{Filter: records.Filter{Source: "deno", Level: "error", ContextID: "ctx-0123456789", JobID: "job-0123456789", Object: "program:acme/jobs/test", From: now, Until: now.Add(time.Second)}, Limit: 1}
	page := queryPage(t, s, q)
	if page.State != "ok" || len(page.Records) != 1 || page.Records[0].Message != "0" || !page.More {
		t.Fatalf("first filtered page: %#v", page)
	}
	var messages []string
	for {
		for _, r := range page.Records {
			messages = append(messages, r.Message)
		}
		if !page.More {
			break
		}
		q.Cursor = page.Cursor
		page = queryPage(t, s, q)
	}
	if strings.Join(messages, ",") != "0,2,4" {
		t.Fatal(messages)
	}
	q.Cursor = page.Cursor
	if _, err := s.rotate("all", now); err != nil {
		t.Fatal(err)
	}
	appendLines(t, s, "deno", now, queryLine("deno", "ERROR", "after rotation", now.Add(10*time.Millisecond)))
	page = queryPage(t, s, q)
	if len(page.Records) != 1 || page.Records[0].Message != "after rotation" {
		t.Fatalf("rotation cursor lost continuity: %#v", page)
	}
	q.Cursor = page.Cursor
	page = queryPage(t, s, q)
	if len(page.Records) != 0 || page.More {
		t.Fatal("idle poll repeated records")
	}
	q.Source = "kernel"
	if _, err := s.query(context.Background(), q); err == nil {
		t.Fatal("reused cursor under different filters")
	}
}

func TestSourceSplitCursorsMergeAndTail(t *testing.T) {
	p := testPolicy()
	p.SplitBy = "source"
	p.MaxTotalSize = 1024 * 1024
	s := testStore(t, p)
	now := time.Now().UTC()
	for i := 0; i < 15; i++ {
		source := []string{"kernel", "deno", "logd"}[i%3]
		appendLines(t, s, source, now, queryLine(source, "INFO", fmt.Sprint(i), now.Add(time.Duration(i)*time.Millisecond)))
	}
	page := queryPage(t, s, records.Query{Limit: 4, Tail: true})
	if len(page.Records) != 4 || page.Records[0].Message != "11" || page.Records[3].Message != "14" || page.More {
		t.Fatalf("tail: %#v", page)
	}
	for _, source := range []string{"deno", "kernel"} {
		appendLines(t, s, source, now, queryLine(source, "INFO", source, now.Add(time.Second)))
	}
	page = queryPage(t, s, records.Query{Cursor: page.Cursor, Limit: 10})
	if len(page.Records) != 2 {
		t.Fatalf("split follow repeated or lost records: %#v", page)
	}
	page = queryPage(t, s, records.Query{Filter: records.Filter{Source: "deno"}, Tail: true, Limit: 2})
	if len(page.Records) != 2 || page.Records[1].Message != "deno" {
		t.Fatal("source tail lost newest record")
	}
}

func TestQueryByteBoundWithEscapeExpansion(t *testing.T) {
	p := testPolicy()
	p.MaxTotalSize = 2 * 1024 * 1024
	s := testStore(t, p)
	now := time.Now().UTC()
	line := queryLine("kernel", "ERROR", strings.Repeat("<", records.MaxRecord), now)
	for i := 0; i < 10; i++ {
		appendLines(t, s, "kernel", now, line)
	}
	page := queryPage(t, s, records.Query{Limit: 500})
	if len(page.Records) == 0 || len(page.Records) >= 10 || !page.More {
		t.Fatal("response byte cap not enforced")
	}
	seen := len(page.Records)
	for page.More {
		page = queryPage(t, s, records.Query{Cursor: page.Cursor, Limit: 500})
		seen += len(page.Records)
	}
	if seen != 10 {
		t.Fatal("byte-limited pagination lost records", seen)
	}
}

func TestBoundedScanProgressWithNoMatches(t *testing.T) {
	p := testPolicy()
	p.MaxFileSize = 16 * 1024 * 1024
	p.MaxTotalSize = 32 * 1024 * 1024
	s := testStore(t, p)
	now := time.Now().UTC()
	line := queryLine("kernel", "INFO", strings.Repeat("x", 2000), now)
	lines := make([][]byte, 2500)
	for i := range lines {
		lines[i] = line
	}
	appendLines(t, s, "kernel", now, lines...)
	q := records.Query{Filter: records.Filter{Source: "deno"}}
	page := queryPage(t, s, q)
	if len(page.Records) != 0 || !page.More || page.ScannedBytes == 0 {
		t.Fatal("scan did not stop with progressing empty page")
	}
	q.Cursor = page.Cursor
	page = queryPage(t, s, q)
	if page.More || len(page.Records) != 0 || page.ScannedBytes == 0 {
		t.Fatal("bounded scan did not finish on continuation")
	}
}

func TestCorruptOversizeAndUnterminatedRecordsStayBounded(t *testing.T) {
	directory := t.TempDir()
	now := time.Now().UTC()
	path := filepath.Join(directory, "segment-00000000000000000001-all.log")
	data := strings.Repeat("x", records.MaxQueryScan+10000) + "\n" + string(queryLine("kernel", "INFO", "after corrupt record", now)) + "unterminated"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	p := testPolicy()
	p.MaxFileSize = 16 * 1024 * 1024
	p.MaxTotalSize = 32 * 1024 * 1024
	s, err := openStore(directory, p, now)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	page := queryPage(t, s, records.Query{})
	if page.CorruptRecords != 1 || !page.More || len(page.Records) != 0 {
		t.Fatalf("corrupt scan: %#v", page)
	}
	page = queryPage(t, s, records.Query{Cursor: page.Cursor})
	if len(page.Records) != 1 || page.Records[0].Message != "after corrupt record" || page.CorruptRecords != 1 || page.More {
		t.Fatalf("corrupt continuation: %#v", page)
	}
}

func TestExpiredUnavailableAndCancelledQueries(t *testing.T) {
	p := testPolicy()
	p.MaxAge = time.Minute
	s := testStore(t, p)
	now := time.Now().UTC()
	appendLines(t, s, "kernel", now, queryLine("kernel", "INFO", "one", now))
	page := queryPage(t, s, records.Query{})
	cursor := page.Cursor
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.query(ctx, records.Query{}); !errors.Is(err, context.Canceled) {
		t.Fatal("query ignored cancellation", err)
	}
	if err := s.maintain(now.Add(2*time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	page = queryPage(t, s, records.Query{Cursor: cursor})
	if page.State != "expired" || page.Reason == "" {
		t.Fatal("expired reference silently reset")
	}
	appendLines(t, s, "kernel", now, queryLine("kernel", "INFO", "two", now))
	path := s.active["all"].path
	if err := os.Rename(path, path+".missing"); err != nil {
		t.Fatal(err)
	}
	page = queryPage(t, s, records.Query{})
	if page.State != "unavailable" {
		t.Fatal("missing file silently ignored")
	}
	if err := os.Rename(path+".missing", path); err != nil {
		t.Fatal(err)
	}
}

func TestCursorSurvivesWriterRestart(t *testing.T) {
	dir := t.TempDir()
	p := testPolicy()
	now := time.Now().UTC()
	s, err := openStore(dir, p, now)
	if err != nil {
		t.Fatal(err)
	}
	appendLines(t, s, "kernel", now, queryLine("kernel", "INFO", "before", now))
	page := queryPage(t, s, records.Query{})
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(dir, p, now)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	appendLines(t, s, "kernel", now, queryLine("kernel", "INFO", "after", now))
	page = queryPage(t, s, records.Query{Cursor: page.Cursor})
	if page.State != "ok" || len(page.Records) != 1 || page.Records[0].Message != "after" {
		t.Fatal("restart cursor did not continue", page.State, len(page.Records))
	}
}
