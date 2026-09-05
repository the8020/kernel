package console

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"the8020/kernel/execution"
	"the8020/kernel/sandbox/backend"
)

type testAuthentication struct{}

func (testAuthentication) AuthenticateToken(_ context.Context, value string) (execution.User, error) {
	if value != "valid" {
		return execution.User{}, errors.New("invalid cookie")
	}
	return execution.User{ID: "user:alice", Username: "alice"}, nil
}

type testProvider struct {
	opened chan testOpen
}

type testOpen struct {
	sandboxID string
	options   backend.ConsoleOptions
	console   *testConsole
	peer      net.Conn
}

func (p *testProvider) OpenConsole(_ context.Context, sandboxID string, options backend.ConsoleOptions) (backend.Console, error) {
	broker, peer := net.Pipe()
	value := newTestConsole(broker)
	opened := testOpen{sandboxID: sandboxID, options: options, console: value, peer: peer}
	select {
	case p.opened <- opened:
		return value, nil
	case <-time.After(time.Second):
		_ = value.Close()
		_ = peer.Close()
		return nil, errors.New("test did not receive opened console")
	}
}

type testConsole struct {
	net.Conn
	resized chan backend.ConsoleSize
	done    chan struct{}
	once    sync.Once
}

func (c *testConsole) ExitStatus() uint32 { return 19 }

func newTestConsole(connection net.Conn) *testConsole {
	return &testConsole{
		Conn:    connection,
		resized: make(chan backend.ConsoleSize, 1),
		done:    make(chan struct{}),
	}
}

func (c *testConsole) Resize(_ context.Context, size backend.ConsoleSize) error {
	c.resized <- size
	return nil
}

func (c *testConsole) Done() <-chan struct{} { return c.done }

func (c *testConsole) CloseWrite() error { return nil }
func (c *testConsole) Stderr() io.Reader { return nil }

func (c *testConsole) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		close(c.done)
	})
	return err
}

func TestConsoleWebSocketStreamsAndResizes(t *testing.T) {
	development := &testProvider{opened: make(chan testOpen, 1)}
	manager, err := New(Config{Authentication: testAuthentication{}, Development: development})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	server := httptest.NewServer(manager)
	t.Cleanup(server.Close)

	socket := dialTestConsole(t, server, server.URL, Subprotocol, "the8020_auth=valid")
	if err := websocket.Message.Send(socket, `{"type":"open","target":{"kind":"development","sandboxId":"sandbox-1"},"arguments":["/bin/bash","-l"],"environment":["TERM=xterm-256color"],"workingDirectory":"/workspace","columns":90,"rows":27}`); err != nil {
		t.Fatal(err)
	}
	var opened testOpen
	select {
	case opened = <-development.opened:
	case <-time.After(time.Second):
		t.Fatal("console provider was not opened")
	}
	t.Cleanup(func() { _ = opened.peer.Close() })
	if opened.sandboxID != "sandbox-1" || len(opened.options.Arguments) != 2 || opened.options.WorkingDir != "/workspace" || opened.options.Size != (backend.ConsoleSize{Columns: 90, Rows: 27}) {
		t.Fatalf("open = %#v", opened)
	}

	outputWritten := make(chan error, 1)
	go func() {
		_, writeErr := opened.peer.Write([]byte("PTY output"))
		outputWritten <- writeErr
	}()
	var output []byte
	if err := websocket.Message.Receive(socket, &output); err != nil {
		t.Fatal(err)
	}
	if string(output) != "PTY output" {
		t.Fatalf("output = %q", output)
	}
	if err := <-outputWritten; err != nil {
		t.Fatal(err)
	}

	if err := websocket.Message.Send(socket, []byte("printf proof\n")); err != nil {
		t.Fatal(err)
	}
	input := make([]byte, len("printf proof\n"))
	if _, err := io.ReadFull(opened.peer, input); err != nil {
		t.Fatal(err)
	}
	if string(input) != "printf proof\n" {
		t.Fatalf("input = %q", input)
	}

	if err := websocket.Message.Send(socket, `{"type":"resize","columns":120,"rows":40}`); err != nil {
		t.Fatal(err)
	}
	select {
	case size := <-opened.console.resized:
		if size != (backend.ConsoleSize{Columns: 120, Rows: 40}) {
			t.Fatalf("resize = %#v", size)
		}
	case <-time.After(time.Second):
		t.Fatal("console was not resized")
	}

	if err := socket.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-opened.console.Done():
	case <-time.After(time.Second):
		t.Fatal("console process was not closed with its WebSocket")
	}
}

func TestConsoleWebSocketRequiresAuthenticationOriginAndProtocol(t *testing.T) {
	development := &testProvider{opened: make(chan testOpen, 1)}
	manager, err := New(Config{Authentication: testAuthentication{}, Development: development})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(manager)
	t.Cleanup(func() {
		server.Close()
		_ = manager.Close()
	})

	response, err := http.Get(server.URL + Route)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", response.StatusCode)
	}

	assertDialFails(t, server, server.URL, "", "the8020_auth=valid")
	assertDialFails(t, server, "http://different-origin.invalid", Subprotocol, "the8020_auth=valid")
	assertDialFails(t, server, server.URL, Subprotocol, "the8020_auth=invalid")
}

func TestRemovingRuntimeProviderClosesItsConsoles(t *testing.T) {
	development := &testProvider{opened: make(chan testOpen, 1)}
	runtimeProvider := &testProvider{opened: make(chan testOpen, 1)}
	manager, err := New(Config{Authentication: testAuthentication{}, Development: development})
	if err != nil {
		t.Fatal(err)
	}
	manager.SetRuntime(runtimeProvider)
	server := httptest.NewServer(manager)
	t.Cleanup(func() {
		server.Close()
		_ = manager.Close()
	})

	socket := dialTestConsole(t, server, server.URL, Subprotocol, "the8020_auth=valid")
	if err := websocket.Message.Send(socket, `{"type":"open","target":{"kind":"runtime","sandboxId":"sandbox-2"},"arguments":["/bin/bash","-l"],"environment":["HOME=/tmp"],"workingDirectory":"/","columns":80,"rows":24}`); err != nil {
		t.Fatal(err)
	}
	var opened testOpen
	select {
	case opened = <-runtimeProvider.opened:
	case <-time.After(time.Second):
		t.Fatal("runtime console provider was not opened")
	}
	t.Cleanup(func() { _ = opened.peer.Close() })
	manager.SetRuntime(nil)
	select {
	case <-opened.console.Done():
	case <-time.After(time.Second):
		t.Fatal("runtime console remained open after provider removal")
	}

	unavailable := dialTestConsole(t, server, server.URL, Subprotocol, "the8020_auth=valid")
	defer unavailable.Close()
	if err := websocket.Message.Send(unavailable, `{"type":"open","target":{"kind":"runtime","sandboxId":"sandbox-2"},"arguments":["/bin/bash"],"environment":["HOME=/tmp"],"workingDirectory":"/","columns":80,"rows":24}`); err != nil {
		t.Fatal(err)
	}
	var control string
	if err := websocket.Message.Receive(unavailable, &control); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(control, "not available") {
		t.Fatalf("runtime provider error = %q", control)
	}
}

func TestTransportNeutralConsoleLeaseIsTracked(t *testing.T) {
	development := &testProvider{opened: make(chan testOpen, 1)}
	manager, err := New(Config{Authentication: testAuthentication{}, Development: development})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.OpenConsole(context.Background(), "development", "sbx-abc12345", backend.ConsoleOptions{
		Arguments: []string{"/bin/bash", "-l"}, Environment: []string{"HOME=/root"},
		WorkingDir: "/workspace", Size: backend.ConsoleSize{Columns: 80, Rows: 24},
	})
	if err != nil {
		t.Fatal(err)
	}
	opened := <-development.opened
	t.Cleanup(func() { _ = opened.peer.Close() })
	status, ok := lease.(backend.ConsoleExitStatus)
	if !ok {
		t.Fatal("brokered console does not expose exit status")
	}
	if status.ExitStatus() != 19 {
		t.Fatalf("brokered exit status = %d", status.ExitStatus())
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lease.Done():
	case <-time.After(time.Second):
		t.Fatal("transport-neutral console remained open after broker close")
	}
}

func TestConsoleWebSocketRejectsBinaryOpenFrame(t *testing.T) {
	development := &testProvider{opened: make(chan testOpen, 1)}
	manager, err := New(Config{Authentication: testAuthentication{}, Development: development})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(manager)
	t.Cleanup(func() {
		server.Close()
		_ = manager.Close()
	})
	socket := dialTestConsole(t, server, server.URL, Subprotocol, "the8020_auth=valid")
	defer socket.Close()
	if err := websocket.Message.Send(socket, []byte(`{"type":"open"}`)); err != nil {
		t.Fatal(err)
	}
	var control string
	if err := websocket.Message.Receive(socket, &control); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(control, "must be text") {
		t.Fatalf("binary open error = %q", control)
	}
	select {
	case unexpected := <-development.opened:
		_ = unexpected.peer.Close()
		t.Fatal("provider opened for invalid first frame")
	default:
	}
}

func dialTestConsole(t *testing.T, server *httptest.Server, origin, protocol, cookie string) *websocket.Conn {
	t.Helper()
	config, err := websocket.NewConfig("ws"+strings.TrimPrefix(server.URL, "http")+Route, origin)
	if err != nil {
		t.Fatal(err)
	}
	if protocol != "" {
		config.Protocol = []string{protocol}
	}
	config.Header.Set("Cookie", cookie)
	socket, err := websocket.DialConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return socket
}

func assertDialFails(t *testing.T, server *httptest.Server, origin, protocol, cookie string) {
	t.Helper()
	config, err := websocket.NewConfig("ws"+strings.TrimPrefix(server.URL, "http")+Route, origin)
	if err != nil {
		t.Fatal(err)
	}
	if protocol != "" {
		config.Protocol = []string{protocol}
	}
	config.Header.Set("Cookie", cookie)
	socket, err := websocket.DialConfig(config)
	if socket != nil {
		_ = socket.Close()
	}
	if err == nil {
		t.Fatal("WebSocket dial unexpectedly succeeded")
	}
}
