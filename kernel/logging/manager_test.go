package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"the8020/kernel/logging/records"
	"the8020/kernel/settings"
)

var testLogd string

func TestMain(m *testing.M) {
	if mode := os.Getenv("THE8020_LOGGING_CAPTURE_CHILD"); mode != "" {
		captureChild(mode)
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "logd-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testLogd = filepath.Join(dir, "logd")
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", testLogd, "../logd")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func testPolicy() records.Policy {
	return records.Policy{Enabled: true, Level: "info", SplitBy: "none", SplitPeriod: "day", MaxFileSize: 128 << 10, MaxTotalSize: 1 << 20, MaxAge: time.Hour}
}
func testConfig(dir string) Config {
	return Config{Directory: filepath.Join(dir, "logs"), Socket: filepath.Join(dir, "api", "logs.sock"), Executable: testLogd, NodeID: "nod-0123456789", MaxProducers: 8, Policy: testPolicy()}
}
func newTestManager(t *testing.T, change func(*Config)) *Manager {
	t.Helper()
	dir, err := os.MkdirTemp("", "log-parent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	config := testConfig(dir)
	if change != nil {
		change(&config)
	}
	m, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	return m
}
func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out:", what)
}
func waitReady(t *testing.T, m *Manager) {
	t.Helper()
	eventually(t, "logd ready", func() bool { s := m.Status(); return s.Running && s.Available })
}
func findRecord(t *testing.T, m *Manager, message string, filter records.Filter) records.Record {
	t.Helper()
	var found records.Record
	eventually(t, "persisted "+message, func() bool {
		page, err := m.Query(context.Background(), records.Query{Filter: filter, Limit: 100, Tail: true})
		if err != nil {
			return false
		}
		for _, r := range page.Records {
			if r.Message == message {
				found = r.Record
				return true
			}
		}
		return false
	})
	return found
}
func values(policy records.Policy) settings.Values {
	return settings.Values{"logging.enabled": policy.Enabled, "logging.level": policy.Level, "logging.split_by": policy.SplitBy, "logging.split_period": policy.SplitPeriod, "logging.max_file_size": settings.ByteSize(policy.MaxFileSize), "logging.max_total_size": settings.ByteSize(policy.MaxTotalSize), "logging.max_age": settings.Duration(policy.MaxAge)}
}

func TestStartupRecordsAndAtomicPolicy(t *testing.T) {
	m := newTestManager(t, nil)
	m.Logger().Info("early boot", "worker_id", "wrk-0123456789", "context_id", "ctx-0123456789", "program_id", "acme/billing/import", "username", "system")
	waitReady(t, m)
	r := findRecord(t, m, "early boot", records.Filter{ContextID: "ctx-0123456789", Username: "system"})
	if r.WorkerID != "wrk-0123456789" || r.Object != "program:acme/billing/import" || r.NodeID != m.config.NodeID || r.Username != "system" {
		t.Fatal("lost attributed boot record", r)
	}
	p := testPolicy()
	p.Enabled = false
	prepared, err := m.Prepare(context.Background(), values(p))
	if err != nil {
		t.Fatal(err)
	}
	if !m.Enabled() {
		t.Fatal("preparation published early")
	}
	prepared.Discard()
	m.Logger().Info("after discard")
	findRecord(t, m, "after discard", records.Filter{Source: "kernel"})
	prepared, err = m.Prepare(context.Background(), values(p))
	if err != nil {
		t.Fatal(err)
	}
	prepared.Commit()
	if m.Enabled() || m.ActiveFile() != "" {
		t.Fatal("disable was not published atomically")
	}
	findRecord(t, m, "early boot", records.Filter{Source: "kernel"})
	m.Logger().Error("disabled record")
	p.Enabled, p.Level, p.SplitBy = true, "warn", "source"
	prepared, err = m.Prepare(context.Background(), values(p))
	if err != nil {
		t.Fatal(err)
	}
	prepared.Commit()
	m.Logger().Info("filtered info")
	m.Logger().Warn("retained warning")
	findRecord(t, m, "retained warning", records.Filter{Source: "kernel"})
	page, err := m.Query(context.Background(), records.Query{Tail: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Records {
		if r.Message == "disabled record" || r.Message == "filtered info" {
			t.Fatal("filtered record was persisted", r)
		}
	}
	p.MaxTotalSize = p.MaxFileSize - 1
	if _, err := m.Prepare(context.Background(), values(p)); err == nil {
		t.Fatal("accepted invalid replacement")
	}
	m.Logger().Error("working after failed replacement")
	findRecord(t, m, "working after failed replacement", records.Filter{})
}

func TestLoggerRestartReplaysLiveBindingsAndKeepsFIFOInode(t *testing.T) {
	m := newTestManager(t, nil)
	waitReady(t, m)
	id, token := "sbx-0123456789", strings.Repeat("a", 64)
	paths, err := m.RegisterSandbox(context.Background(), id, token)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(paths.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(paths.Stdout, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	_, _ = writer.WriteString("before restart\n")
	findRecord(t, m, "before restart", records.Filter{SandboxID: id})
	pid := m.Status().PID
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "logd replacement", func() bool { s := m.Status(); return s.Running && s.PID != pid && s.Restarts > 0 })
	after, err := os.Stat(paths.Stdout)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("replaced a live FIFO inode", err)
	}
	_, _ = writer.WriteString("after restart\n")
	r := findRecord(t, m, "after restart", records.Filter{SandboxID: id})
	if r.WorkerID != "" || r.ContextID != "" || r.Source != "deno" || r.Stream != "stdout" {
		t.Fatal("raw output gained invented attribution", r)
	}
	conn, err := net.Dial("unix", m.config.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	hello, _ := json.Marshal(records.Hello{Version: records.ProtocolVersion, ID: id, Token: token})
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if err := records.WriteFrame(conn, hello); err != nil {
		t.Fatal(err)
	}
	if _, err := records.ReadFrame(conn); err != nil {
		t.Fatal("registration was not replayed", err)
	}
	_ = conn.Close()
	_, _ = writer.WriteString("final unterminated 💡")
	_ = writer.Close()
	if err := m.UnregisterSandbox(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	r = findRecord(t, m, "final unterminated 💡", records.Filter{SandboxID: id})
	if r.Attributes["unterminated"] != "true" {
		t.Fatal("lost EOF framing")
	}
	if _, err := os.Stat(paths.Stdout); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retired FIFO remains", err)
	}
}

func TestKernelReplacementAdoptsRawBeforeRuntimeAndDoesNotStealOldWriter(t *testing.T) {
	first := newTestManager(t, nil)
	waitReady(t, first)
	id, token := "sbx-0123456789", strings.Repeat("a", 64)
	paths, err := first.RegisterSandbox(context.Background(), id, token)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(paths.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(paths.Stdout, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	second, err := New(first.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	eventually(t, "previous writer detection", func() bool {
		return strings.Contains(second.Status().ProcessError, "previous log writer")
	})
	_, _ = writer.WriteString("old writer still owns raw\n")
	findRecord(t, first, "old writer still owns raw", records.Filter{SandboxID: id})
	if second.Status().RawDroppedBytes != 0 {
		t.Fatal("standby reader stole bytes from the previous writer")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	waitReady(t, second)
	if after, err := os.Stat(paths.Stdout); err != nil || !os.SameFile(before, after) {
		t.Fatal("kernel replacement changed the native FIFO inode", err)
	}
	_, _ = writer.WriteString("raw before runtime recovery\n")
	findRecord(t, second, "raw before runtime recovery", records.Filter{SandboxID: id})

	connect := func(token string) error {
		conn, err := net.Dial("unix", second.config.Socket)
		if err != nil {
			return err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		data, _ := json.Marshal(records.Hello{Version: records.ProtocolVersion, ID: id, Token: token})
		if err := records.WriteFrame(conn, data); err != nil {
			return err
		}
		_, err = records.ReadFrame(conn)
		return err
	}
	if connect("") == nil || connect(token) == nil {
		t.Fatal("raw-only adoption authenticated an unregistered producer")
	}
	if restored, err := second.RegisterSandbox(context.Background(), id, token); err != nil || restored != paths {
		t.Fatal("runtime recovery changed its existing ingress", err)
	}
	if err := connect(token); err != nil {
		t.Fatal("original credential was not restored", err)
	}
	_ = writer.Close()
	if err := second.UnregisterSandbox(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(paths.Stdout); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("confirmed native cleanup retained FIFO", err)
	}
}

func TestRawPreparationFailurePreservesExistingEndpoints(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			dir := t.TempDir()
			m := &Manager{config: Config{Socket: filepath.Join(dir, "logs.sock")}}
			ingress := filepath.Join(dir, "log-ingress")
			if err := os.Mkdir(ingress, 0700); err != nil {
				t.Fatal(err)
			}
			stdout, stderr := filepath.Join(ingress, "sbx-0123456789-stdout"), filepath.Join(ingress, "sbx-0123456789-stderr")
			if existing {
				if err := unix.Mkfifo(stdout, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(stderr, []byte("unrelated"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := m.prepareRaw(records.Binding{ID: "sbx-0123456789"}, false); err == nil {
				t.Fatal("regular file admitted as raw endpoint")
			}
			_, err := os.Lstat(stdout)
			if (existing && err != nil) || (!existing && !errors.Is(err, os.ErrNotExist)) {
				t.Fatal("failed preparation violated FIFO ownership", err)
			}
			if data, err := os.ReadFile(stderr); err != nil || string(data) != "unrelated" {
				t.Fatal("failed preparation changed unrelated data", err)
			}
		})
	}
}

func TestRawRecoveryIsolatesAnInvalidEndpoint(t *testing.T) {
	dir := t.TempDir()
	ingress := filepath.Join(dir, "log-ingress")
	if err := os.Mkdir(ingress, 0700); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(ingress, "sbx-aaaaaaaaaa-stdout")
	if err := os.WriteFile(bad, []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, stream := range []string{"stdout", "stderr"} {
		if err := unix.Mkfifo(filepath.Join(ingress, "sbx-bbbbbbbbbb-"+stream), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manager{config: Config{Socket: filepath.Join(dir, "logs.sock"), MaxProducers: 4}, registrations: map[string]*registration{}}
	t.Cleanup(func() {
		for _, r := range m.registrations {
			r.release()
		}
	})
	if err := m.adoptRaw(); err == nil {
		t.Fatal("invalid endpoint was not reported")
	}
	r := m.registrations["sbx-bbbbbbbbbb"]
	if r == nil || r.binding.Token != "" || len(m.registrations) != 1 {
		t.Fatal("one invalid endpoint prevented unrelated raw recovery")
	}
	if data, err := os.ReadFile(bad); err != nil || string(data) != "unrelated" {
		t.Fatal("raw recovery modified unrelated data", err)
	}
}

func TestCommittedPolicySurvivesLoggerDeathAfterPreparation(t *testing.T) {
	m := newTestManager(t, nil)
	waitReady(t, m)
	p := testPolicy()
	p.Level, p.SplitBy = "error", "source"
	prepared, err := m.Prepare(context.Background(), values(p))
	if err != nil {
		t.Fatal(err)
	}
	oldPID := m.Status().PID
	process, err := os.FindProcess(oldPID)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	prepared.Commit()
	if m.Logger().Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("kernel producer retained stale policy")
	}
	eventually(t, "persisted policy after writer crash", func() bool { s := m.Status(); return s.Running && s.PID != oldPID && !s.PolicyPending && s.Policy == p })
	m.Logger().Warn("warning filtered after recovery")
	m.Logger().Error("error after policy recovery")
	findRecord(t, m, "error after policy recovery", records.Filter{Source: "kernel"})
	page, err := m.Query(context.Background(), records.Query{Tail: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Records {
		if r.Message == "warning filtered after recovery" {
			t.Fatal("restarted writer lost severity policy")
		}
	}
}

func TestUnavailableWriterBoundsBuffersAndDrainsRaw(t *testing.T) {
	m := newTestManager(t, func(c *Config) { c.Executable = filepath.Join(filepath.Dir(c.Directory), "missing-logd") })
	paths, err := m.RegisterSandbox(context.Background(), "sbx-abcdefghij", strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(paths.Stderr, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	_ = writer.SetWriteDeadline(time.Now().Add(2 * time.Second))
	message := strings.Repeat("log flood", 50)
	start := time.Now()
	for range 1000 {
		m.Logger().Info(message)
	}
	if time.Since(start) > time.Second {
		t.Fatal("kernel callers waited for missing logd")
	}
	if _, err := writer.Write(bytes.Repeat([]byte("raw\n"), 256*1024)); err != nil {
		t.Fatal("raw writer blocked during outage", err)
	}
	eventually(t, "outage byte count", func() bool { return m.Status().RawDroppedBytes >= 1<<20 })
	s := m.Status()
	if s.KernelQueue.PendingBytes > producerBytes || s.KernelQueue.Dropped[1] == 0 || s.ProcessError == "" {
		t.Fatal("outage was unbounded or unreported", s)
	}
	page, err := m.Query(context.Background(), records.Query{})
	if err != nil || page.State != "unavailable" {
		t.Fatal(page, err)
	}
	_ = writer.Close()
	if err := m.UnregisterSandbox(context.Background(), "sbx-abcdefghij"); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	closed := m.Status().KernelQueue
	if closed.PendingRecords != 0 || closed.PendingBytes != 0 || closed.Dropped[1] != 1000 {
		t.Fatal("shutdown retained or failed to account for unsent prints", closed)
	}
}

func TestCancelledQueryConsumesReplyWithoutBreakingNextCall(t *testing.T) {
	parent, server := net.Pipe()
	defer parent.Close()
	defer server.Close()
	m := &Manager{commands: make(chan managerCall, 8), stop: make(chan struct{}), done: make(chan struct{}), child: &childProcess{control: parent}, generation: 1}
	firstReceived := make(chan struct{})
	go func() {
		for i := 0; i < 2; i++ {
			data, err := records.ReadFrame(server)
			if err != nil {
				return
			}
			var request records.ControlRequest
			_ = json.Unmarshal(data, &request)
			if i == 0 {
				close(firstReceived)
				time.Sleep(75 * time.Millisecond)
			}
			data, _ = json.Marshal(records.ControlReply{ID: request.ID, OK: true, Page: &records.Page{State: "ok", Records: []records.LocatedRecord{}}})
			if records.WriteControlFrame(server, data) != nil {
				return
			}
		}
	}()
	go func() {
		defer close(m.done)
		for range 2 {
			call := <-m.commands
			call.reply <- m.process(call)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := m.Query(ctx, records.Query{}); done <- err }()
	<-firstReceived
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("query ignored cancellation", err)
	}
	if page, err := m.Query(context.Background(), records.Query{}); err != nil || page.State != "ok" {
		t.Fatal("cancelled query corrupted next reply", page, err)
	}
	<-m.done
}

func captureChild(mode string) {
	config := testConfig(os.Getenv("THE8020_LOGGING_CHILD_DIR"))
	config.Executable = os.Getenv("THE8020_LOGGING_CHILD_BINARY")
	config.CaptureStandard = true
	m, err := New(config)
	if err != nil {
		panic(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !m.Status().Running && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !m.Status().Running {
		panic("test writer never started")
	}
	stdout, _ := m.Console()
	fmt.Fprintln(stdout, "intentional console output")
	fmt.Print("native kernel stdout 💡\n")
	fmt.Fprint(os.Stderr, "native kernel stderr\n")
	m.Logger().Info("structured kernel output")
	for m.producer.queue.Stats().PendingRecords > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if mode == "crash" {
		panic("intentional kernel crash")
	}
	if err := m.Close(); err != nil {
		panic(err)
	}
	fmt.Println("restored console output")
}

func TestManagedKernelDescriptorsAndCrashDrain(t *testing.T) {
	for _, mode := range []string{"graceful", "crash"} {
		t.Run(mode, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "kernel-raw-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), "THE8020_LOGGING_CAPTURE_CHILD="+mode, "THE8020_LOGGING_CHILD_DIR="+dir, "THE8020_LOGGING_CHILD_BINARY="+testLogd, "GORACE=atexit_sleep_ms=0")
			cmd.WaitDelay = 5 * time.Second
			out, err := cmd.CombinedOutput()
			if mode == "graceful" && err != nil || mode == "crash" && err == nil {
				t.Fatal("unexpected kernel outcome", err, string(out))
			}
			if !bytes.Contains(out, []byte("intentional console output")) || bytes.Contains(out, []byte("native kernel")) {
				t.Fatal("intentional console and managed output were mixed", string(out))
			}
			if mode == "graceful" && !bytes.Contains(out, []byte("restored console output")) {
				t.Fatal("stdout was not restored")
			}
			var messages []string
			paths, _ := filepath.Glob(filepath.Join(dir, "logs", "segment-*.log"))
			for _, path := range paths {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				for _, line := range bytes.SplitAfter(data, []byte{'\n'}) {
					if len(line) == 0 {
						continue
					}
					r, err := records.Decode(line)
					if err != nil {
						t.Fatal(err)
					}
					messages = append(messages, r.Message)
				}
			}
			joined := strings.Join(messages, "\n")
			for _, expected := range []string{"native kernel stdout 💡", "native kernel stderr", "structured kernel output"} {
				if !strings.Contains(joined, expected) {
					t.Fatal("lost child output", expected, joined)
				}
			}
			if mode == "crash" && !strings.Contains(joined, "panic: intentional kernel crash") {
				t.Fatal("lost Go runtime panic output", joined)
			}
		})
	}
}

type collectingEmitter struct {
	policy records.Policy
	record records.Record
}

func (e *collectingEmitter) allows(level string) bool   { return e.policy.Allows(level) }
func (e *collectingEmitter) emit(r records.Record) bool { e.record = r; return true }

type panicError struct{}

func (panicError) Error() string { panic("bad error formatter") }

func TestSlogBoundsGroupsRedactionAndTypedMetadata(t *testing.T) {
	emitter := &collectingEmitter{policy: testPolicy()}
	logger := slog.New(&handler{node: "nod-0123456789", emitter: emitter})
	logger.Info("plain", "worker_id", "wrk-0123456789", "context_id", "ctx-0123456789")
	if emitter.record.Attributes != nil || emitter.record.WorkerID != "wrk-0123456789" || emitter.record.ContextID != "ctx-0123456789" {
		t.Fatal("identity-only log allocated attributes or lost metadata", emitter.record)
	}
	cycle := map[string]any{}
	cycle["self"], cycle["password"] = cycle, "must not persist"
	logger.With("worker_id", "wrk-0123456789", slog.Group("auth", "accessToken", "must not persist"), "value", cycle).Warn("bounded", "context_id", "ctx-0123456789", "error", panicError{}, "huge", "START"+strings.Repeat("💡", 100000)+"END")
	r := emitter.record
	data, err := records.Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > records.MaxRecord || bytes.Contains(data, []byte("must not persist")) || r.WorkerID == "" || r.ContextID == "" {
		t.Fatal("broken slog contract", string(data))
	}
	if !strings.Contains(r.Attributes["huge"], "START") || !strings.HasSuffix(r.Attributes["huge"], "END") || r.Attributes["error"] != "[format failed]" {
		t.Fatal("formatter lost boundaries or propagated panic", r.Attributes)
	}
	logger.WithGroup("request").With(slog.Group("credentials", "value", "must not persist")).Error("grouped", slog.Group("headers", "authorization", "must not persist"))
	data, err = records.Encode(emitter.record)
	if err != nil || bytes.Contains(data, []byte("must not persist")) {
		t.Fatal("grouped credentials leaked", err, string(data))
	}
}
