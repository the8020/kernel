package operations

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"the8020/kernel/console"
	"the8020/kernel/execution"
	"the8020/kernel/sandbox/backend"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/services"
)

type terminalTestProvider struct {
	opened  chan net.Conn
	opening chan struct{}
	block   bool
}

func (*terminalTestProvider) HasSandbox(string) bool { return true }
func (p *terminalTestProvider) OpenConsole(ctx context.Context, _ string, _ backend.ConsoleOptions) (backend.Console, error) {
	if p.opening != nil {
		close(p.opening)
	}
	if p.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	left, right := net.Pipe()
	p.opened <- right
	return &terminalTestConsole{Conn: left, done: make(chan struct{})}, nil
}

type terminalTestConsole struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *terminalTestConsole) Close() error {
	c.once.Do(func() { _ = c.Conn.Close(); close(c.done) })
	return nil
}
func (c *terminalTestConsole) CloseWrite() error {
	return errors.New("retained terminals cannot send implicit EOF")
}
func (*terminalTestConsole) Stderr() io.Reader                                 { return nil }
func (*terminalTestConsole) Resize(context.Context, backend.ConsoleSize) error { return nil }
func (c *terminalTestConsole) Done() <-chan struct{}                           { return c.done }

type terminalTestAuth struct{}

func (terminalTestAuth) AuthenticateToken(context.Context, string) (execution.User, error) {
	return execution.SystemUser(), nil
}

func terminalTestDispatcher(t *testing.T, p *terminalTestProvider) (*Dispatcher, *console.Manager, context.Context) {
	t.Helper()
	m, err := console.New(console.Config{Authentication: terminalTestAuth{}, Development: p})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	d := &Dispatcher{services: &services.Services{Consoles: m, Instance: services.InstanceInfo{UUID: "nod-aaaaaaaaaa"}}, terminalOwners: make(map[string]*terminalOwner)}
	ctx := execution.WithCaller(context.Background(), execution.Caller{
		ContextID: "ctx-aaaaaaaaaa", SandboxID: "sbx-bbbbbbbbbb", WorkerID: "wrk-aaaaaaaaaa",
		Workload: model.WorkloadService, User: execution.SystemUser(),
	})
	return d, m, ctx
}

func terminalCreateArguments() map[string]any {
	return map[string]any{"kind": "development", "sandboxId": "sbx-aaaaaaaaaa", "arguments": []string{"/bin/bash", "-l"},
		"environment": []string{"TERM=xterm-256color"}, "workingDir": "/workspace", "size": backend.ConsoleSize{Columns: 80, Rows: 24}}
}

func TestNamedTerminalBridgeValidatesOwnerAndReclaimsLostWorker(t *testing.T) {
	p := &terminalTestProvider{opened: make(chan net.Conn, 2)}
	d, m, ctx := terminalTestDispatcher(t, p)
	args := terminalCreateArguments()
	args["sessionId"] = "1"
	owner := console.TerminalOwner{NodeID: "nod-aaaaaaaaaa", SandboxID: "sbx-bbbbbbbbbb", WorkerID: "wrk-aaaaaaaaaa", PersistentExecutionID: "pex-aaaaaaaaaa"}
	args["owner"] = owner
	first, err := d.Execute(ctx, "terminal.open", args)
	if err != nil {
		t.Fatal(err)
	}
	defer (<-p.opened).Close()
	physical := first.(map[string]any)["terminal"].(console.TerminalInfo).ID
	other := execution.WithCaller(ctx, execution.Caller{ContextID: "ctx-bbbbbbbbbb", SandboxID: owner.SandboxID, WorkerID: "wrk-bbbbbbbbbb", Workload: model.WorkloadService, User: execution.SystemUser()})
	if _, err := d.Execute(other, "terminal.open", args); err == nil {
		t.Fatal("forged processor Worker accepted")
	}
	owner.WorkerID = "wrk-bbbbbbbbbb"
	owner.PersistentExecutionID = "pex-bbbbbbbbbb"
	args["owner"] = owner
	current, err := d.Execute(other, "terminal.open", args)
	if err != nil || current.(map[string]any)["owner"].(console.TerminalOwner).WorkerID != "wrk-aaaaaaaaaa" {
		t.Fatalf("live owner = %#v, %v", current, err)
	}
	d.ReleaseWorker("sbx-bbbbbbbbbb", "wrk-aaaaaaaaaa")
	adopted, err := d.Execute(other, "terminal.open", args)
	if err != nil || adopted.(map[string]any)["reset"] != true || adopted.(map[string]any)["terminal"].(console.TerminalInfo).ID != physical {
		t.Fatalf("adopted = %#v, %v", adopted, err)
	}
	if err := d.CloseTerminal(ctx, physical); err != nil {
		t.Fatal(err)
	}
	recreated, err := d.Execute(other, "terminal.open", args)
	if err != nil || recreated.(map[string]any)["terminal"].(console.TerminalInfo).ID == physical {
		t.Fatalf("recreated = %#v, %v", recreated, err)
	}
	defer (<-p.opened).Close()
	if got := m.Terminals("development", "sbx-aaaaaaaaaa"); len(got) != 1 || got[0].SessionID != "1" {
		t.Fatalf("named catalog = %#v", got)
	}
}

func TestNativeViewBridgeRequiresExactProcessorWorker(t *testing.T) {
	p := &terminalTestProvider{opened: make(chan net.Conn, 1)}
	d, m, ctx := terminalTestDispatcher(t, p)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	created, err := d.Execute(ctx, "terminal.create", terminalCreateArguments())
	if err != nil {
		t.Fatal(err)
	}
	result := created.(map[string]any)
	info := result["terminal"].(console.TerminalInfo)
	processor := result["attachmentId"].(string)
	peer := <-p.opened
	defer peer.Close()
	view, err := m.OpenTerminalView(ctx, info.ID, "", info.Size)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	other := execution.WithCaller(ctx, execution.Caller{ContextID: "ctx-bbbbbbbbbb", SandboxID: "sbx-bbbbbbbbbb", WorkerID: "wrk-bbbbbbbbbb", Workload: model.WorkloadService, User: execution.SystemUser()})
	if _, err := d.Execute(other, "terminal.view-next", map[string]any{"attachmentId": processor}); !errors.Is(err, console.ErrTerminalDetached) {
		t.Fatalf("foreign processor accepted native view: %v", err)
	}
	accepted, err := d.Execute(ctx, "terminal.view-next", map[string]any{"attachmentId": processor})
	if err != nil {
		t.Fatal(err)
	}
	request := accepted.(console.TerminalViewRequest)
	args := map[string]any{"attachmentId": processor, "viewId": request.ViewID, "data": base64.StdEncoding.EncodeToString([]byte("display"))}
	if _, err := d.Execute(other, "terminal.view-write", args); !errors.Is(err, console.ErrTerminalDetached) {
		t.Fatalf("foreign Worker wrote native view: %v", err)
	}
	write := make(chan error, 1)
	go func() { _, err := d.Execute(ctx, "terminal.view-write", args); write <- err }()
	bytes := make([]byte, 7)
	if _, err := io.ReadFull(view, bytes); err != nil || string(bytes) != "display" {
		t.Fatalf("display=%q error=%v", bytes, err)
	}
	if err := <-write; err != nil {
		t.Fatal(err)
	}
	busy, err := d.Execute(other, "terminal.attach", map[string]any{"terminalId": info.ID, "mode": "control"})
	if err != nil || busy.(map[string]any)["busy"] != true {
		t.Fatalf("busy=%#v error=%v", busy, err)
	}
	if _, err := d.Execute(other, "terminal.attach", map[string]any{"terminalId": info.ID, "mode": "take-control"}); err != nil {
		t.Fatal(err)
	}
	if _, err := view.Write([]byte("stale input")); !errors.Is(err, console.ErrTerminalDetached) {
		t.Fatalf("replaced view wrote: %v", err)
	}
	d.ReleaseWorker("sbx-bbbbbbbbbb", "wrk-aaaaaaaaaa")
	if current, err := m.Terminal(info.ID); err != nil || current.Info().Exited {
		t.Fatal("display owner loss destroyed PTY")
	}
	if err := d.CloseTerminal(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Terminal(info.ID); !errors.Is(err, console.ErrTerminalGone) {
		t.Fatalf("native close left terminal: %v", err)
	}
	if len(d.terminalOwners) != 0 {
		t.Fatal("native close retained Worker attachment references")
	}
	if err := d.CloseTerminal(ctx, info.ID); err != nil {
		t.Fatalf("repeated native close: %v", err)
	}
}

func TestTerminalQueryReplyDoesNotBlockItsOutputProcessor(t *testing.T) {
	p := &terminalTestProvider{opened: make(chan net.Conn, 1)}
	d, _, ctx := terminalTestDispatcher(t, p)
	created, err := d.Execute(ctx, "terminal.create", terminalCreateArguments())
	if err != nil {
		t.Fatal(err)
	}
	processor := created.(map[string]any)["attachmentId"].(string)
	peer := <-p.opened
	defer peer.Close()
	_ = peer.SetDeadline(time.Now().Add(time.Second))
	callCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	// A native program may finish producing output before reading its reply.
	// Waiting for native consumption here would stall that same output reader.
	if _, err := d.Execute(callCtx, "terminal.respond", map[string]any{
		"attachmentId": processor, "data": base64.StdEncoding.EncodeToString([]byte("reply")),
	}); err != nil {
		t.Fatalf("query reply blocked output processing: %v", err)
	}
	if _, err := peer.Write([]byte("more output")); err != nil {
		t.Fatal(err)
	}
	batch, err := d.Execute(ctx, "terminal.read", map[string]any{"attachmentId": processor, "after": 0})
	if err != nil || string(batch.(console.TerminalBatch).Events[0].Data) != "more output" {
		t.Fatalf("output=%#v error=%v", batch, err)
	}
	reply := make([]byte, 5)
	if _, err := io.ReadFull(peer, reply); err != nil || string(reply) != "reply" {
		t.Fatalf("reply=%q error=%v", reply, err)
	}
}

func TestTerminalRuntimeOperationsReleaseWorkerWithoutClosingPTY(t *testing.T) {
	p := &terminalTestProvider{opened: make(chan net.Conn, 1)}
	d, m, ctx := terminalTestDispatcher(t, p)
	created, err := d.Execute(ctx, "terminal.create", terminalCreateArguments())
	if err != nil {
		t.Fatal(err)
	}
	result := created.(map[string]any)
	info := result["terminal"].(console.TerminalInfo)
	processor := result["attachmentId"].(string)
	peer := <-p.opened
	defer peer.Close()
	_ = peer.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := peer.Write([]byte("first output")); err != nil {
		t.Fatal(err)
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	batch, err := d.Execute(callCtx, "terminal.read", map[string]any{"attachmentId": processor, "after": 0})
	if err != nil || string(batch.(console.TerminalBatch).Events[0].Data) != "first output" {
		t.Fatalf("batch=%#v error=%v", batch, err)
	}
	otherCtx := execution.WithCaller(ctx, execution.Caller{ContextID: "ctx-bbbbbbbbbb", SandboxID: "sbx-bbbbbbbbbb", WorkerID: "wrk-bbbbbbbbbb", Workload: model.WorkloadService, User: execution.SystemUser()})
	if _, err := d.Execute(otherCtx, "terminal.respond", map[string]any{"attachmentId": processor, "data": base64.StdEncoding.EncodeToString([]byte("forged"))}); !errors.Is(err, console.ErrTerminalDetached) {
		t.Fatalf("another Worker used the processor: %v", err)
	}
	controllerResult, err := d.Execute(ctx, "terminal.attach", map[string]any{"terminalId": info.ID, "mode": "control"})
	if err != nil {
		t.Fatal(err)
	}
	controller := controllerResult.(map[string]any)["attachmentId"].(string)
	writeDone := make(chan error, 1)
	go func() {
		_, err := d.Execute(callCtx, "terminal.write", map[string]any{"attachmentId": controller, "data": base64.StdEncoding.EncodeToString([]byte("hello"))})
		writeDone <- err
	}()
	input := make([]byte, 5)
	if _, err := io.ReadFull(peer, input); err != nil || string(input) != "hello" {
		t.Fatalf("input=%q error=%v", input, err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	d.ReleaseWorker("sbx-bbbbbbbbbb", "wrk-aaaaaaaaaa")
	if len(d.terminalOwners) != 0 {
		t.Fatal("released Worker retained attachment state")
	}
	if _, err := d.Execute(ctx, "terminal.read", map[string]any{"attachmentId": processor, "after": 1}); !errors.Is(err, console.ErrTerminalDetached) {
		t.Fatalf("released processor remained callable: %v", err)
	}
	physical, err := m.Terminal(info.ID)
	if err != nil || physical.Info().Exited {
		t.Fatal("Worker release killed its retained process")
	}
	if _, err := peer.Write([]byte("detached output")); err != nil {
		t.Fatal(err)
	}
	attached, err := d.Execute(otherCtx, "terminal.attach", map[string]any{"terminalId": info.ID, "mode": "control"})
	if err != nil {
		t.Fatal(err)
	}
	if got := attached.(map[string]any)["terminal"].(console.TerminalInfo).ID; got != info.ID {
		t.Fatal("reattach created another process")
	}
	if _, err := d.Execute(otherCtx, "terminal.close", map[string]any{"terminalId": info.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Terminal(info.ID); !errors.Is(err, console.ErrTerminalGone) {
		t.Fatalf("explicit close kept process identity: %v", err)
	}
	if len(d.terminalOwners) != 0 {
		t.Fatal("explicit close leaked attachment owners")
	}
}

func TestTerminalRuntimeWorkerReleaseCancelsPendingNativeOpen(t *testing.T) {
	p := &terminalTestProvider{opening: make(chan struct{}), block: true}
	d, m, ctx := terminalTestDispatcher(t, p)
	done := make(chan error, 1)
	go func() { _, err := d.Execute(ctx, "terminal.create", terminalCreateArguments()); done <- err }()
	<-p.opening
	d.ReleaseWorker("sbx-bbbbbbbbbb", "wrk-aaaaaaaaaa")
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("open=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Worker release left native opening blocked")
	}
	if len(d.terminalOwners) != 0 || len(m.Terminals("development", "sbx-aaaaaaaaaa")) != 0 {
		t.Fatal("cancelled creation leaked owner or process state")
	}
}

func TestTerminalDestructionReachesBlockedProcessorAsTypedClosure(t *testing.T) {
	p := &terminalTestProvider{opened: make(chan net.Conn, 1)}
	d, m, ctx := terminalTestDispatcher(t, p)
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	created, err := d.Execute(ctx, "terminal.create", terminalCreateArguments())
	if err != nil {
		t.Fatal(err)
	}
	result := created.(map[string]any)
	info := result["terminal"].(console.TerminalInfo)
	processor := result["attachmentId"].(string)
	defer (<-p.opened).Close()
	type outcome struct {
		value any
		err   error
	}
	waiting := make(chan outcome, 2)
	for _, operation := range []string{"terminal.read", "terminal.view-next"} {
		go func() {
			value, err := d.Execute(ctx, operation, map[string]any{"attachmentId": processor})
			waiting <- outcome{value, err}
		}()
	}
	terminal, err := m.Terminal(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = terminal.Close() // Kernel lifecycle destruction, without dispatcher assistance.
	for range 2 {
		got := <-waiting
		if got.err != nil || got.value.(map[string]any)["closed"] != true {
			t.Fatalf("closure=%#v error=%v", got.value, got.err)
		}
	}
	if _, err := d.Execute(ctx, "terminal.detach", map[string]any{"attachmentId": processor}); err != nil {
		t.Fatal(err)
	}
	if len(d.terminalOwners) != 0 {
		t.Fatal("expired processor cleanup retained Worker references")
	}
}
