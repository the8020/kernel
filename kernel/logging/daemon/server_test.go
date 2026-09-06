package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"the8020/kernel/identity"
	"the8020/kernel/logging/records"
)

type daemonHarness struct {
	t        *testing.T
	init     records.Initialization
	control  net.Conn
	done     chan error
	cancel   context.CancelFunc
	finished bool
}

func startDaemon(t *testing.T, raw [2]*os.File) *daemonHarness {
	t.Helper()
	dir, err := os.MkdirTemp("", "logd-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	parent, child := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	h := &daemonHarness{t: t, init: records.Initialization{Version: records.ProtocolVersion, Directory: filepath.Join(dir, "logs"), Socket: filepath.Join(dir, "api", "logs.sock"), NodeID: "nod-0123456789", Policy: testPolicy(), MaxProducers: 8}, control: parent, done: make(chan error, 1), cancel: cancel}
	go func() { h.done <- Run(ctx, child, raw) }()
	data, _ := json.Marshal(h.init)
	_ = parent.SetDeadline(time.Now().Add(3 * time.Second))
	if err := records.WriteFrame(parent, data); err != nil {
		t.Fatal(err)
	}
	data, err = records.ReadControlFrame(parent)
	if err != nil {
		t.Fatal(err)
	}
	var ready records.ControlReply
	if json.Unmarshal(data, &ready) != nil || !ready.OK || !ready.Status.Available {
		t.Fatalf("daemon did not initialize: %s", data)
	}
	t.Cleanup(func() {
		h.control.Close()
		h.cancel()
		if !h.finished {
			select {
			case err := <-h.done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(4 * time.Second):
				t.Error("logging daemon did not stop")
			}
		}
	})
	return h
}

func (h *daemonHarness) call(request records.ControlRequest) records.ControlReply {
	h.t.Helper()
	request.ID, _ = identity.New("cor")
	data, _ := json.Marshal(request)
	_ = h.control.SetDeadline(time.Now().Add(3 * time.Second))
	if err := records.WriteFrame(h.control, data); err != nil {
		h.t.Fatal(err)
	}
	data, err := records.ReadControlFrame(h.control)
	if err != nil {
		h.t.Fatal(err)
	}
	var reply records.ControlReply
	if err = json.Unmarshal(data, &reply); err != nil {
		h.t.Fatal(err)
	}
	if reply.ID != request.ID {
		h.t.Fatal("control reply correlation changed")
	}
	return reply
}

func (h *daemonHarness) status() records.Status {
	h.t.Helper()
	r := h.call(records.ControlRequest{Operation: "status"})
	if !r.OK || r.Status == nil {
		h.t.Fatal(r.Error)
	}
	return *r.Status
}
func (h *daemonHarness) wait(predicate func(records.Status) bool) {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if predicate(h.status()) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatal("daemon state did not converge")
}

func (h *daemonHarness) bind(id string) (records.Binding, records.RawPaths) {
	h.t.Helper()
	token, err := identity.NewToken()
	if err != nil {
		h.t.Fatal(err)
	}
	b := records.Binding{ID: id, Token: token}
	if identity.Is(id, "sbx") {
		dir := filepath.Join(filepath.Dir(h.init.Socket), "log-ingress")
		if err := os.MkdirAll(dir, 0700); err != nil {
			h.t.Fatal(err)
		}
		for _, stream := range []string{"stdout", "stderr"} {
			if err := syscall.Mkfifo(filepath.Join(dir, id+"-"+stream), 0600); err != nil {
				h.t.Fatal(err)
			}
		}
	}
	reply := h.call(records.ControlRequest{Operation: "register", Binding: &b})
	if !reply.OK {
		h.t.Fatal(reply.Error)
	}
	return b, *reply.Paths
}

func (h *daemonHarness) connect(binding records.Binding) *net.UnixConn {
	h.t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: h.init.Socket, Net: "unix"})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { conn.Close() })
	data, _ := json.Marshal(records.Hello{Version: records.ProtocolVersion, ID: binding.ID, Token: binding.Token})
	if err = records.WriteFrame(conn, data); err != nil {
		h.t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	data, err = records.ReadFrame(conn)
	if err != nil {
		h.t.Fatal(err)
	}
	var policy records.ProducerPolicy
	if json.Unmarshal(data, &policy) != nil || policy.Revision != h.status().PolicyRevision {
		h.t.Fatal("missing initial producer policy")
	}
	return conn
}

func sendRecord(t *testing.T, conn net.Conn, r records.Record) {
	t.Helper()
	data, err := records.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	if err = records.WriteFrame(conn, data); err != nil {
		t.Fatal(err)
	}
}
func fixtureRecord(source, message string) records.Record {
	return records.Record{Time: time.Now().UTC(), Level: "INFO", Source: source, Component: "test", NodeID: "nod-0123456789", Message: message}
}

func TestServerBindingPolicyAndBoundedLogQueries(t *testing.T) {
	h := startDaemon(t, [2]*os.File{})
	b, _ := h.bind("sbx-0123456789")
	conn := h.connect(b)
	r := fixtureRecord("deno", "bound message")
	r.SandboxID = b.ID
	r.WorkerID = "wrk-0123456789"
	r.ContextID = "ctx-0123456789"
	sendRecord(t, conn, r)
	for _, mutate := range []func(*records.Record){func(r *records.Record) { r.NodeID = "nod-abcdefghij" }, func(r *records.Record) { r.SandboxID = "sbx-abcdefghij" }, func(r *records.Record) { r.Source = "kernel" }} {
		bad := r
		mutate(&bad)
		sendRecord(t, conn, bad)
	}
	h.wait(func(s records.Status) bool { return s.Written >= 2 && s.Invalid == 3 })
	reply := h.call(records.ControlRequest{Operation: "query", Query: &records.Query{Filter: records.Filter{ContextID: r.ContextID}}})
	if !reply.OK || reply.Page == nil || len(reply.Page.Records) != 1 || reply.Page.Records[0].SandboxID != b.ID {
		t.Fatal("trusted producer attribution/query failed", reply.Error)
	}
	policy := h.init.Policy
	policy.Level = "error"
	prepared := h.call(records.ControlRequest{Operation: "prepare", Policy: &policy})
	if !prepared.OK {
		t.Fatal(prepared.Error)
	}
	committed := h.call(records.ControlRequest{Operation: "commit", Preparation: prepared.Preparation})
	if !committed.OK {
		t.Fatal(committed.Error)
	}
	data, err := records.ReadFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	var update records.ProducerPolicy
	if json.Unmarshal(data, &update) != nil || update.Level != "error" || update.Revision != 2 {
		t.Fatal("producer did not receive atomic policy", string(data))
	}
	before := h.status().Accepted
	sendRecord(t, conn, r)
	r.Level = "ERROR"
	r.Message = "retained error"
	sendRecord(t, conn, r)
	h.wait(func(s records.Status) bool { return s.Accepted == before+1 && s.Queue.PendingRecords == 0 })
	reply = h.call(records.ControlRequest{Operation: "query", Query: &records.Query{Filter: records.Filter{Source: "deno"}}})
	if len(reply.Page.Records) != 2 || reply.Page.Records[1].Message != "retained error" {
		t.Fatal("filtering was not applied at ingestion")
	}
	unregistered := h.call(records.ControlRequest{Operation: "unregister", Producer: b.ID})
	if !unregistered.OK {
		t.Fatal(unregistered.Error)
	}
}

func TestRawFIFOUnregisterDrainsUnfinishedUTF8(t *testing.T) {
	h := startDaemon(t, [2]*os.File{})
	b, paths := h.bind("sbx-abcdefghij")
	out, err := os.OpenFile(paths.Stdout, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	errout, err := os.OpenFile(paths.Stderr, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = out.Write([]byte("native stdout\n"))
	_, _ = errout.Write([]byte("native failure 原因💡"))
	_ = out.Close()
	_ = errout.Close()
	r := h.call(records.ControlRequest{Operation: "unregister", Producer: b.ID})
	if !r.OK {
		t.Fatal(r.Error)
	}
	h.wait(func(s records.Status) bool { return s.Written >= 3 && s.Queue.PendingRecords == 0 })
	r = h.call(records.ControlRequest{Operation: "query", Query: &records.Query{Filter: records.Filter{SandboxID: b.ID}}})
	if len(r.Page.Records) != 2 {
		t.Fatal("lost native tail", len(r.Page.Records))
	}
	for _, record := range r.Page.Records {
		if record.WorkerID != "" || record.ContextID != "" {
			t.Fatal("invented attribution for anonymous bytes")
		}
		if record.Stream == "stderr" && (record.Message != "native failure 原因💡" || record.Attributes["unterminated"] != "true") {
			t.Fatal("unfinished tail changed")
		}
	}
	if _, err := os.Stat(paths.Stdout); err != nil {
		t.Fatal("daemon removed kernel-owned transport", err)
	}
}

func TestKernelControlEOFStillCapturesCrashPipeTail(t *testing.T) {
	readOut, writeOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writeOut.Close()
	readErr, writeErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writeErr.Close()
	h := startDaemon(t, [2]*os.File{readOut, readErr})
	_ = h.control.Close()
	_, _ = writeErr.Write([]byte("panic: crash after control EOF\nfinal stack frame"))
	_ = writeOut.Close()
	_ = writeErr.Close()
	select {
	case err := <-h.done:
		h.finished = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("crash drain did not finish")
	}
	p := h.init.Policy
	p.Enabled = false
	s, err := openStore(h.init.Directory, p, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	page := queryPage(t, s, records.Query{Filter: records.Filter{Source: "kernel"}})
	if len(page.Records) != 2 || !strings.Contains(page.Records[0].Message, "control EOF") || page.Records[1].Message != "final stack frame" {
		t.Fatalf("kernel crash tail lost: %#v", page.Records)
	}
}

func TestDuplicateWriterAndCredentialRejection(t *testing.T) {
	h := startDaemon(t, [2]*os.File{})
	b, _ := h.bind(h.init.NodeID)
	parent, child := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), child, [2]*os.File{}) }()
	data, _ := json.Marshal(h.init)
	_ = parent.SetDeadline(time.Now().Add(time.Second))
	if err := records.WriteFrame(parent, data); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "another log writer") {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate writer did not reject")
	}
	_ = parent.Close()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: h.init.Socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	bad := records.Hello{Version: records.ProtocolVersion, ID: b.ID, Token: strings.Repeat("0", 64)}
	data, _ = json.Marshal(bad)
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if err = records.WriteFrame(conn, data); err != nil {
		t.Fatal(err)
	}
	if _, err = records.ReadFrame(conn); err == nil {
		t.Fatal("unauthorized producer authenticated")
	}
	valid := h.connect(b)
	sendRecord(t, valid, fixtureRecord("kernel", "still owned by original writer"))
	h.wait(func(s records.Status) bool { return s.Written >= 2 })
}

func TestShutdownAcknowledgesThenBoundsDrain(t *testing.T) {
	h := startDaemon(t, [2]*os.File{})
	b, _ := h.bind(h.init.NodeID)
	conn := h.connect(b)
	sendRecord(t, conn, fixtureRecord("kernel", "before shutdown"))
	r := h.call(records.ControlRequest{Operation: "shutdown"})
	if !r.OK {
		t.Fatal(r.Error)
	}
	select {
	case err := <-h.done:
		h.finished = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("active producer prevented bounded drain")
	}
}
