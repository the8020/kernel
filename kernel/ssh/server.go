// Package sshserver exposes kernel-authenticated SSH sessions backed by the
// existing sandbox console broker.
package sshserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"the8020/kernel/auth"
	"the8020/kernel/sandbox/backend"
	"the8020/kernel/sandbox/model"
)

const (
	defaultColumns       = 80
	defaultRows          = 24
	defaultTerminal      = "xterm-256color"
	maximumConnections   = 64
	maximumAuthenticates = 4
	maximumSessions      = 64
	maximumCommandBytes  = 16 << 10
	maximumEnvironment   = 48
	maximumEnvBytes      = 16 << 10
	maximumTerminalBytes = 128
)

type Authenticator interface {
	AuthenticatePassword(string, []byte) (auth.AuthContext, error)
}

type Development interface {
	EnsureDefaultSandbox(context.Context, string) (string, error)
}

type Consoles interface {
	OpenConsole(context.Context, string, string, backend.ConsoleOptions) (backend.Console, error)
}

type Config struct {
	Port           int
	HostKeyPath    string
	Authentication Authenticator
	Development    Development
	Consoles       Consoles
	Logger         *slog.Logger
}

type Manager struct {
	mu             sync.Mutex
	listener       net.Listener
	server         *gossh.ServerConfig
	development    Development
	consoles       Consoles
	logger         *slog.Logger
	connections    map[net.Conn]struct{}
	activeSessions int
	closed         bool
	cancel         context.CancelFunc
	done           chan struct{}
}

type selector struct {
	sandboxID string
	command   string
}

type terminal struct {
	name      string
	size      backend.ConsoleSize
	allocated bool
}

type ptyRequest struct {
	Terminal     string
	Columns      uint32
	Rows         uint32
	PixelWidth   uint32
	PixelHeight  uint32
	EncodedModes string
}

type windowChangeRequest struct {
	Columns     uint32
	Rows        uint32
	PixelWidth  uint32
	PixelHeight uint32
}

type execRequest struct{ Command string }
type envRequest struct{ Name, Value string }
type signalRequest struct{ Signal string }
type exitStatus struct{ Status uint32 }

func New(config Config) (*Manager, error) {
	if config.Port < 0 || config.Port > 65535 {
		return nil, errors.New("SSH port must be between 0 and 65535")
	}
	if config.Authentication == nil || config.Development == nil || config.Consoles == nil {
		return nil, errors.New("SSH authentication, development, and console providers are required")
	}
	signer, err := loadOrCreateHostKey(config.HostKeyPath)
	if err != nil {
		return nil, err
	}
	serverConfig := &gossh.ServerConfig{
		MaxAuthTries:  3,
		ServerVersion: "SSH-2.0-80x20",
	}
	authenticationSlots := make(chan struct{}, maximumAuthenticates)
	serverConfig.PasswordCallback = func(metadata gossh.ConnMetadata, password []byte) (*gossh.Permissions, error) {
		defer clear(password)
		select {
		case authenticationSlots <- struct{}{}:
			defer func() { <-authenticationSlots }()
		default:
			return nil, errors.New("authentication is temporarily unavailable")
		}
		identity, authenticateErr := config.Authentication.AuthenticatePassword(metadata.User(), password)
		if authenticateErr != nil || !identity.Authenticated || identity.Username == "" {
			return nil, errors.New("authentication failed")
		}
		return &gossh.Permissions{Extensions: map[string]string{"username": identity.Username}}, nil
	}
	serverConfig.AddHostKey(signer)
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(config.Port))
	if err != nil {
		return nil, fmt.Errorf("listen for SSH: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager := &Manager{
		listener: listener, server: serverConfig,
		development: config.Development, consoles: config.Consoles, logger: config.Logger,
		connections: make(map[net.Conn]struct{}), cancel: cancel, done: make(chan struct{}),
	}
	go func() {
		manager.accept(ctx)
		close(manager.done)
	}()
	return manager, nil
}

func (m *Manager) Port() int {
	if address, ok := m.listener.Addr().(*net.TCPAddr); ok {
		return address.Port
	}
	return 0
}

func (m *Manager) accept(ctx context.Context) {
	var connections sync.WaitGroup
	defer connections.Wait()
	for {
		connection, err := m.listener.Accept()
		if err != nil {
			if ctx.Err() == nil && m.logger != nil {
				m.logger.Error("SSH listener failed", "error", err)
			}
			return
		}
		if !m.registerConnection(connection) {
			_ = connection.Close()
			continue
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			m.serveConnection(ctx, connection)
		}()
	}
}

func (m *Manager) registerConnection(connection net.Conn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || len(m.connections) >= maximumConnections {
		return false
	}
	m.connections[connection] = struct{}{}
	return true
}

func (m *Manager) serveConnection(ctx context.Context, connection net.Conn) {
	defer func() {
		_ = connection.Close()
		m.mu.Lock()
		delete(m.connections, connection)
		m.mu.Unlock()
	}()
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	serverConnection, channels, requests, err := gossh.NewServerConn(connection, m.server)
	if err != nil {
		return
	}
	_ = connection.SetDeadline(time.Time{})
	defer serverConnection.Close()
	connectionContext, cancelConnection := context.WithCancel(ctx)
	defer cancelConnection()
	go rejectGlobalRequests(requests)
	username := serverConnection.User()
	if serverConnection.Permissions != nil && serverConnection.Permissions.Extensions["username"] != "" {
		username = serverConnection.Permissions.Extensions["username"]
	}
	for channelRequest := range channels {
		if channelRequest.ChannelType() != "session" || !m.reserveSession() {
			if m.logger != nil {
				m.logger.Warn("SSH channel rejected", "username", username, "channel_type", channelRequest.ChannelType())
			}
			_ = channelRequest.Reject(gossh.Prohibited, "only bounded terminal sessions are available")
			continue
		}
		channel, channelRequests, err := channelRequest.Accept()
		if err != nil {
			m.releaseSession()
			continue
		}
		go func() {
			defer m.releaseSession()
			m.serveSession(connectionContext, username, channel, channelRequests)
		}()
	}
}

func rejectGlobalRequests(requests <-chan *gossh.Request) {
	for request := range requests {
		if request.WantReply {
			_ = request.Reply(false, nil)
		}
	}
}

func (m *Manager) reserveSession() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.activeSessions >= maximumSessions {
		return false
	}
	m.activeSessions++
	return true
}

func (m *Manager) releaseSession() {
	m.mu.Lock()
	m.activeSessions--
	m.mu.Unlock()
}

func (m *Manager) serveSession(ctx context.Context, username string, channel gossh.Channel, requests <-chan *gossh.Request) {
	defer channel.Close()
	configuration := terminal{name: defaultTerminal, size: backend.ConsoleSize{Columns: defaultColumns, Rows: defaultRows}}
	environment := make(map[string]string)
	for request := range requests {
		switch request.Type {
		case "pty-req":
			var payload ptyRequest
			err := gossh.Unmarshal(request.Payload, &payload)
			if err == nil {
				configuration, err = terminalFromRequest(payload)
				configuration.allocated = err == nil
			}
			reply(request, err == nil)
		case "env":
			var payload envRequest
			err := gossh.Unmarshal(request.Payload, &payload)
			if err == nil {
				err = acceptEnvironment(environment, payload)
			}
			if err != nil {
				m.logSessionFailure(username, "env", err)
			}
			reply(request, err == nil)
		case "shell":
			if len(request.Payload) != 0 {
				reply(request, false)
				return
			}
			m.launch(ctx, username, selector{}, configuration, environment, channel, requests, request)
			return
		case "exec":
			var payload execRequest
			err := gossh.Unmarshal(request.Payload, &payload)
			var selected selector
			if err == nil {
				selected, err = parseExec(payload.Command)
			}
			if err != nil {
				m.logSessionFailure(username, "exec", err)
				writeSessionError(channel, err)
				reply(request, false)
				return
			}
			m.launch(ctx, username, selected, configuration, environment, channel, requests, request)
			return
		default:
			m.logSessionFailure(username, request.Type, errors.New("unsupported SSH session request"))
			reply(request, false)
		}
	}
}

func (m *Manager) launch(ctx context.Context, username string, selected selector, configuration terminal, environment map[string]string, channel gossh.Channel, requests <-chan *gossh.Request, start *gossh.Request) {
	kind, sandboxID, err := m.resolveTarget(ctx, username, selected)
	if err != nil {
		m.logSessionFailure(username, "resolve-target", err)
		writeSessionError(channel, err)
		reply(start, false)
		return
	}
	home, workingDirectory, loginName := "/tmp", "/", username
	if kind == "development" {
		home, workingDirectory, loginName = "/root", "/workspace", "root"
	}
	arguments := []string{"/bin/bash", "-l"}
	if selected.command != "" {
		arguments = []string{"/bin/bash", "-c", selected.command}
	}
	forwarded := map[string]string{
		"TERM": configuration.name, "PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME": home, "SHELL": "/bin/bash", "USER": loginName, "LOGNAME": loginName,
	}
	for name, value := range environment {
		forwarded[name] = value
	}
	names := make([]string, 0, len(forwarded))
	for name := range forwarded {
		names = append(names, name)
	}
	sort.Strings(names)
	forwardedEnvironment := make([]string, 0, len(names))
	for _, name := range names {
		forwardedEnvironment = append(forwardedEnvironment, name+"="+forwarded[name])
	}
	options := backend.ConsoleOptions{
		Arguments:   arguments,
		Environment: forwardedEnvironment,
		WorkingDir:  workingDirectory,
		Size:        configuration.size,
		Terminal:    configuration.allocated,
	}
	console, err := m.consoles.OpenConsole(ctx, kind, sandboxID, options)
	if err != nil {
		m.logSessionFailure(username, "open-console", err)
		writeSessionError(channel, err)
		reply(start, false)
		return
	}
	defer console.Close()
	if m.logger != nil {
		m.logger.Info("SSH session started", "username", username, "target_kind", kind, "sandbox_id", sandboxID, "command_bytes", len(selected.command))
	}
	reply(start, true)
	go relayRequests(ctx, console, requests)

	outputCount := 1
	if console.Stderr() != nil {
		outputCount++
	}
	outputDone := make(chan error, outputCount)
	go func() {
		_, copyErr := io.CopyBuffer(channel, console, make([]byte, 32*1024))
		outputDone <- copyErr
	}()
	if stderr := console.Stderr(); stderr != nil {
		go func() {
			_, copyErr := io.CopyBuffer(channel.Stderr(), stderr, make([]byte, 16*1024))
			outputDone <- copyErr
		}()
	}
	go func() {
		_, _ = io.CopyBuffer(console, channel, make([]byte, 32*1024))
		_ = console.CloseWrite()
	}()
	status := uint32(0)
	for range outputCount {
		select {
		case err := <-outputDone:
			if err != nil && !errors.Is(err, io.EOF) {
				status = 255
			}
		case <-ctx.Done():
			status = 255
		}
	}
	if status == 0 {
		if result, ok := console.(backend.ConsoleExitStatus); ok {
			status = result.ExitStatus()
		}
	}
	_ = console.Close()
	_, _ = channel.SendRequest("exit-status", false, gossh.Marshal(exitStatus{Status: status}))
}

func acceptEnvironment(environment map[string]string, request envRequest) error {
	if !validEnvironmentName(request.Name) || len(request.Value) > maximumEnvBytes || strings.IndexByte(request.Value, 0) >= 0 {
		return errors.New("SSH environment entry is invalid")
	}
	if _, exists := environment[request.Name]; !exists && len(environment) >= maximumEnvironment {
		return errors.New("SSH environment has too many entries")
	}
	total := len(request.Name) + len(request.Value)
	for name, value := range environment {
		if name != request.Name {
			total += len(name) + len(value)
		}
	}
	if total > maximumEnvBytes {
		return errors.New("SSH environment exceeds the size limit")
	}
	environment[request.Name] = request.Value
	return nil
}

func validEnvironmentName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for index, character := range name {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && character != '_' &&
			(index == 0 || character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func (m *Manager) logSessionFailure(username, stage string, err error) {
	if m.logger != nil {
		m.logger.Warn("SSH session failed", "username", username, "stage", stage, "error", err)
	}
}

func relayRequests(ctx context.Context, console backend.Console, requests <-chan *gossh.Request) {
	for request := range requests {
		accepted := false
		switch request.Type {
		case "window-change":
			var payload windowChangeRequest
			if gossh.Unmarshal(request.Payload, &payload) == nil {
				size, err := consoleSize(payload.Columns, payload.Rows)
				if err == nil {
					accepted = console.Resize(ctx, size) == nil
				}
			}
		case "signal":
			var payload signalRequest
			if gossh.Unmarshal(request.Payload, &payload) == nil {
				accepted = relaySignal(console, payload.Signal)
			}
		}
		reply(request, accepted)
	}
}

func relaySignal(console backend.Console, signal string) bool {
	switch signal {
	case "INT":
		_, _ = console.Write([]byte{0x03})
	case "QUIT":
		_, _ = console.Write([]byte{0x1c})
	case "TSTP":
		_, _ = console.Write([]byte{0x1a})
	case "HUP", "TERM", "KILL":
		_ = console.Close()
	default:
		return false
	}
	return true
}

func (m *Manager) resolveTarget(ctx context.Context, username string, selected selector) (string, string, error) {
	if selected.sandboxID == "" {
		sandboxID, err := m.development.EnsureDefaultSandbox(ctx, username)
		if err != nil {
			return "", "", fmt.Errorf("prepare default development sandbox: %w", err)
		}
		if !validDevelopmentSandboxID(sandboxID) {
			return "", "", errors.New("default development sandbox identity is invalid")
		}
		return "development", sandboxID, nil
	}
	if validDevelopmentSandboxID(selected.sandboxID) {
		return "development", selected.sandboxID, nil
	}
	if model.IsSandboxID(selected.sandboxID) {
		return "runtime", selected.sandboxID, nil
	}
	return "", "", errors.New("selected sandbox ID is invalid")
}

func parseExec(command string) (selector, error) {
	if command == "" || len(command) > maximumCommandBytes || strings.IndexByte(command, 0) >= 0 {
		return selector{}, errors.New("remote command is empty or exceeds the size limit")
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return selector{}, errors.New("remote command is empty")
	}
	if fields[0] != "the8020" {
		return selector{command: command}, nil
	}
	selected := selector{}
	for _, field := range fields[1:] {
		name, value, ok := strings.Cut(field, "=")
		if !ok || name != "sandbox-id" || value == "" {
			return selector{}, errors.New("unknown or malformed the8020 selector parameter")
		}
		if selected.sandboxID != "" {
			return selector{}, errors.New("sandbox-id may be specified only once")
		}
		if !validSelectorSandboxID(value) {
			return selector{}, errors.New("sandbox-id must be a canonical sbx- ID or dev-<username>")
		}
		selected.sandboxID = value
	}
	return selected, nil
}

func validSelectorSandboxID(value string) bool {
	return model.IsSandboxID(value) || validDevelopmentSandboxID(value)
}

func validDevelopmentSandboxID(value string) bool {
	username, found := strings.CutPrefix(value, "dev-")
	return found && auth.ValidateUsername(username) == nil
}

func terminalFromRequest(request ptyRequest) (terminal, error) {
	if request.Terminal == "" || len(request.Terminal) > maximumTerminalBytes {
		return terminal{}, errors.New("SSH terminal name is invalid")
	}
	for _, character := range request.Terminal {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && !strings.ContainsRune("+._-", character) {
			return terminal{}, errors.New("SSH terminal name is invalid")
		}
	}
	size, err := consoleSize(request.Columns, request.Rows)
	if err != nil {
		return terminal{}, err
	}
	return terminal{name: request.Terminal, size: size}, nil
}

func consoleSize(columns, rows uint32) (backend.ConsoleSize, error) {
	if columns == 0 {
		columns = defaultColumns
	}
	if rows == 0 {
		rows = defaultRows
	}
	size := backend.ConsoleSize{Columns: int(columns), Rows: int(rows)}
	if size.Columns < 2 || size.Columns > 500 || size.Rows < 1 || size.Rows > 200 {
		return backend.ConsoleSize{}, errors.New("SSH terminal size is outside the supported range")
	}
	return size, nil
}

func writeSessionError(channel gossh.Channel, err error) {
	_, _ = io.WriteString(channel.Stderr(), "the8020: "+err.Error()+"\r\n")
}

func reply(request *gossh.Request, accepted bool) {
	if request.WantReply {
		_ = request.Reply(accepted, nil)
	}
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		m.cancel()
		_ = m.listener.Close()
		for connection := range m.connections {
			_ = connection.Close()
		}
	}
	done := m.done
	m.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
