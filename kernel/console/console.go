// Package console bridges authenticated local WebSockets to interactive
// processes in running sandboxes.
package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/net/websocket"

	"the8020/kernel/auth"
	"the8020/kernel/execution"
	"the8020/kernel/sandbox/backend"
)

const (
	Route       = "/_the8020/console"
	Subprotocol = "the8020.console.v1"
	maxSessions = 32
	maxFrame    = 64 * 1024
)

type Authentication interface {
	AuthenticateToken(context.Context, string) (execution.User, error)
}

type Provider interface {
	OpenConsole(context.Context, string, backend.ConsoleOptions) (backend.Console, error)
}

type Config struct {
	Authentication Authentication
	Development    Provider
}

type Manager struct {
	mu             sync.Mutex
	authentication Authentication
	development    Provider
	runtime        Provider
	sessions       map[*session]struct{}
	closed         bool
}

type session struct {
	manager  *Manager
	kind     string
	provider Provider
	socket   *websocket.Conn
	console  backend.Console
	once     sync.Once
}

type openMessage struct {
	Type        string   `json:"type"`
	Target      target   `json:"target"`
	Arguments   []string `json:"arguments"`
	Environment []string `json:"environment"`
	WorkingDir  string   `json:"workingDirectory"`
	Columns     int      `json:"columns"`
	Rows        int      `json:"rows"`
}

type target struct {
	Kind      string `json:"kind"`
	SandboxID string `json:"sandboxId"`
}

type resizeMessage struct {
	Type    string `json:"type"`
	Columns int    `json:"columns"`
	Rows    int    `json:"rows"`
}

type incomingFrame struct {
	payloadType byte
	data        []byte
}

var incomingFrames = websocket.Codec{
	Unmarshal: func(data []byte, payloadType byte, value any) error {
		frame, ok := value.(*incomingFrame)
		if !ok || (payloadType != websocket.TextFrame && payloadType != websocket.BinaryFrame) {
			return websocket.ErrNotSupported
		}
		frame.payloadType = payloadType
		frame.data = append(frame.data[:0], data...)
		return nil
	},
}

func New(config Config) (*Manager, error) {
	if config.Authentication == nil || config.Development == nil {
		return nil, errors.New("console authentication and development provider are required")
	}
	return &Manager{
		authentication: config.Authentication,
		development:    config.Development,
		sessions:       make(map[*session]struct{}),
	}, nil
}

// SetRuntime publishes or clears the ordinary runtime-sandbox provider.
func (m *Manager) SetRuntime(provider Provider) {
	m.mu.Lock()
	m.runtime = provider
	closing := []*session{}
	if provider == nil {
		for item := range m.sessions {
			if item.kind == "runtime" {
				closing = append(closing, item)
			}
		}
	}
	m.mu.Unlock()
	for _, item := range closing {
		item.close()
	}
}

func (m *Manager) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != Route {
		http.NotFound(writer, request)
		return
	}
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		http.Error(writer, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	token, fromCookie := auth.RequestToken(request)
	identity, err := m.authentication.AuthenticateToken(request.Context(), token)
	if err != nil || !identity.Valid() {
		if fromCookie {
			auth.ClearTokenCookie(writer, auth.SecureTransport(request))
		}
		http.Error(writer, "Authentication required", http.StatusUnauthorized)
		return
	}
	server := websocket.Server{
		Handshake: sameOriginProtocol,
		Handler: func(socket *websocket.Conn) {
			socket.MaxPayloadBytes = maxFrame
			m.serveSocket(socket)
		},
	}
	server.ServeHTTP(writer, request)
}

func sameOriginProtocol(config *websocket.Config, request *http.Request) error {
	origin, err := websocket.Origin(config, request)
	if err != nil || origin == nil || !sameHost(origin, request.Host) {
		return errors.New("same-origin WebSocket is required")
	}
	found := false
	for _, protocol := range config.Protocol {
		if protocol == Subprotocol {
			found = true
			break
		}
	}
	if !found {
		return errors.New("console WebSocket subprotocol is required")
	}
	config.Protocol = []string{Subprotocol}
	return nil
}

func sameHost(origin *url.URL, host string) bool {
	return (origin.Scheme == "http" || origin.Scheme == "https") &&
		strings.EqualFold(origin.Host, host)
}

func (m *Manager) serveSocket(socket *websocket.Conn) {
	message, err := receiveOpen(socket)
	if err != nil {
		sendError(socket, err)
		return
	}
	options := backend.ConsoleOptions{
		Arguments:   append([]string(nil), message.Arguments...),
		Environment: append([]string(nil), message.Environment...),
		WorkingDir:  message.WorkingDir,
		Size:        backend.ConsoleSize{Columns: message.Columns, Rows: message.Rows},
		Terminal:    true,
	}
	if err := backend.ValidateConsoleOptions(options); err != nil {
		sendError(socket, err)
		return
	}
	active, err := m.openConsole(socket.Request().Context(), message.Target.Kind, message.Target.SandboxID, options, socket)
	if err != nil {
		sendError(socket, err)
		return
	}
	defer active.Close()
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		buffer := make([]byte, 16*1024)
		for {
			count, readErr := active.Read(buffer)
			if count > 0 {
				if sendErr := websocket.Message.Send(socket, append([]byte(nil), buffer[:count]...)); sendErr != nil {
					return
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					sendError(socket, fmt.Errorf("console output failed: %w", readErr))
				}
				_ = socket.Close()
				return
			}
		}
	}()
	for {
		var incoming incomingFrame
		if err := incomingFrames.Receive(socket, &incoming); err != nil {
			break
		}
		switch incoming.payloadType {
		case websocket.BinaryFrame:
			if len(incoming.data) == 0 || len(incoming.data) > maxFrame {
				sendError(socket, errors.New("console input frame is invalid"))
				return
			}
			if _, err := active.Write(incoming.data); err != nil {
				sendError(socket, fmt.Errorf("console input failed: %w", err))
				return
			}
		case websocket.TextFrame:
			var resize resizeMessage
			if err := decodeJSON(string(incoming.data), &resize); err != nil || resize.Type != "resize" {
				sendError(socket, errors.New("invalid console control message"))
				return
			}
			size := backend.ConsoleSize{Columns: resize.Columns, Rows: resize.Rows}
			if err := backend.ValidateConsoleOptions(backend.ConsoleOptions{
				Arguments: message.Arguments, Environment: message.Environment,
				WorkingDir: message.WorkingDir, Size: size,
			}); err != nil {
				sendError(socket, err)
				return
			}
			if err := active.Resize(socket.Request().Context(), size); err != nil {
				sendError(socket, fmt.Errorf("resize console: %w", err))
				return
			}
		default:
			sendError(socket, errors.New("unsupported console frame"))
			return
		}
	}
	_ = active.Close()
	_ = socket.Close()
	<-outputDone
}

// OpenConsole acquires one transport-neutral, lifecycle-tracked PTY lease.
// The caller owns the returned console and must close it.
func (m *Manager) OpenConsole(ctx context.Context, kind, sandboxID string, options backend.ConsoleOptions) (backend.Console, error) {
	if !validTarget(target{Kind: kind, SandboxID: sandboxID}) {
		return nil, errors.New("invalid console target")
	}
	return m.openConsole(ctx, kind, sandboxID, options, nil)
}

func (m *Manager) openConsole(ctx context.Context, kind, sandboxID string, options backend.ConsoleOptions, socket *websocket.Conn) (*session, error) {
	provider, err := m.provider(kind)
	if err != nil {
		return nil, err
	}
	if err := backend.ValidateConsoleOptions(options); err != nil {
		return nil, err
	}
	value, err := provider.OpenConsole(ctx, sandboxID, options)
	if err != nil {
		return nil, err
	}
	active := &session{manager: m, kind: kind, provider: provider, socket: socket, console: value}
	if err := m.register(active); err != nil {
		_ = value.Close()
		return nil, err
	}
	return active, nil
}

func receiveOpen(socket *websocket.Conn) (openMessage, error) {
	var frame incomingFrame
	if err := incomingFrames.Receive(socket, &frame); err != nil {
		return openMessage{}, err
	}
	if frame.payloadType != websocket.TextFrame {
		return openMessage{}, errors.New("first console message must be text")
	}
	var message openMessage
	if err := decodeJSON(string(frame.data), &message); err != nil {
		return openMessage{}, errors.New("invalid console open message")
	}
	if message.Type != "open" || !validTarget(message.Target) {
		return openMessage{}, errors.New("invalid console target")
	}
	return message, nil
}

func validTarget(value target) bool {
	if value.Kind != "runtime" && value.Kind != "development" {
		return false
	}
	if value.SandboxID == "" || len(value.SandboxID) > 160 {
		return false
	}
	for _, character := range value.SandboxID {
		if (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func decodeJSON(text string, result any) error {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func sendError(socket *websocket.Conn, err error) {
	data, _ := json.Marshal(map[string]string{"type": "error", "message": err.Error()})
	_ = websocket.Message.Send(socket, string(data))
}

func (m *Manager) provider(kind string) (Provider, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("console broker is closed")
	}
	if kind == "development" {
		return m.development, nil
	}
	if kind == "runtime" && m.runtime != nil {
		return m.runtime, nil
	}
	return nil, errors.New("runtime sandbox consoles are not available")
}

func (m *Manager) register(value *session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("console broker is closed")
	}
	if value.kind == "runtime" && m.runtime != value.provider {
		return errors.New("runtime sandbox consoles are not available")
	}
	if len(m.sessions) >= maxSessions {
		return errors.New("console session limit reached")
	}
	m.sessions[value] = struct{}{}
	return nil
}

func (m *Manager) unregister(value *session) {
	m.mu.Lock()
	delete(m.sessions, value)
	m.mu.Unlock()
}

func (s *session) Read(data []byte) (int, error) { return s.console.Read(data) }

func (s *session) Write(data []byte) (int, error) { return s.console.Write(data) }

func (s *session) CloseWrite() error { return s.console.CloseWrite() }

func (s *session) Stderr() io.Reader { return s.console.Stderr() }

func (s *session) Resize(ctx context.Context, size backend.ConsoleSize) error {
	return s.console.Resize(ctx, size)
}

func (s *session) Done() <-chan struct{} { return s.console.Done() }

func (s *session) ExitStatus() uint32 {
	if result, ok := s.console.(backend.ConsoleExitStatus); ok {
		return result.ExitStatus()
	}
	return 0
}

func (s *session) close() error {
	var result error
	s.once.Do(func() {
		result = s.console.Close()
		if s.socket != nil {
			result = errors.Join(result, s.socket.Close())
		}
		if s.manager != nil {
			s.manager.unregister(s)
		}
	})
	return result
}

func (s *session) Close() error { return s.close() }

func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	sessions := make([]*session, 0, len(m.sessions))
	for item := range m.sessions {
		sessions = append(sessions, item)
	}
	m.mu.Unlock()
	for _, item := range sessions {
		item.close()
	}
	return nil
}
