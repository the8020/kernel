package sshserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"the8020/kernel/auth"
	"the8020/kernel/database"
	"the8020/kernel/sandbox/backend"
	"the8020/kernel/settings"
)

type observingAuthentication struct {
	manager  *auth.Manager
	mu       sync.Mutex
	password []byte
}

func (a *observingAuthentication) AuthenticatePassword(username string, password []byte) (auth.AuthContext, error) {
	a.mu.Lock()
	a.password = password
	a.mu.Unlock()
	return a.manager.AuthenticatePassword(username, password)
}

func (a *observingAuthentication) AuthenticateUser(username string) (auth.AuthContext, error) {
	return a.manager.AuthenticateUser(username)
}

func (a *observingAuthentication) cleared() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.password) > 0 && bytes.Equal(a.password, make([]byte, len(a.password)))
}

type fakeDevelopment struct {
	mu      sync.Mutex
	users   []string
	sandbox string
	failure error
	keys    map[string][]byte
	keyErr  error
}

func (d *fakeDevelopment) EnsureSandbox(_ context.Context, username string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.users = append(d.users, username)
	return d.sandbox, d.failure
}

func (d *fakeDevelopment) AuthorizedKeys(username string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.keys[username]...), d.keyErr
}

type openedConsole struct {
	kind      string
	sandboxID string
	options   backend.ConsoleOptions
	terminal  *fakeConsole
}

type fakeConsoles struct{ opened chan openedConsole }

func (c *fakeConsoles) OpenConsole(_ context.Context, kind, sandboxID string, options backend.ConsoleOptions) (backend.Console, error) {
	terminal := newFakeConsole()
	c.opened <- openedConsole{kind: kind, sandboxID: sandboxID, options: options, terminal: terminal}
	return terminal, nil
}

type fakeConsole struct {
	reader     *io.PipeReader
	writer     *io.PipeWriter
	input      chan []byte
	resized    chan backend.ConsoleSize
	done       chan struct{}
	closeOnce  sync.Once
	finishOnce sync.Once
	status     uint32
}

func newFakeConsole() *fakeConsole {
	reader, writer := io.Pipe()
	return &fakeConsole{
		reader: reader, writer: writer, input: make(chan []byte, 16),
		resized: make(chan backend.ConsoleSize, 4), done: make(chan struct{}),
	}
}

func (c *fakeConsole) Read(data []byte) (int, error) { return c.reader.Read(data) }
func (c *fakeConsole) Write(data []byte) (int, error) {
	c.input <- append([]byte(nil), data...)
	return len(data), nil
}
func (c *fakeConsole) CloseWrite() error {
	c.input <- []byte{0x04}
	return nil
}
func (c *fakeConsole) Stderr() io.Reader { return nil }
func (c *fakeConsole) Resize(_ context.Context, size backend.ConsoleSize) error {
	c.resized <- size
	return nil
}
func (c *fakeConsole) Done() <-chan struct{} { return c.done }
func (c *fakeConsole) ExitStatus() uint32    { return c.status }
func (c *fakeConsole) Close() error {
	c.closeOnce.Do(func() {
		_ = c.writer.Close()
		_ = c.reader.Close()
		close(c.done)
	})
	return nil
}
func (c *fakeConsole) emit(data []byte) error {
	_, err := c.writer.Write(data)
	return err
}
func (c *fakeConsole) finish() { c.finishOnce.Do(func() { _ = c.writer.Close() }) }

func TestSSHPasswordTTYAndRouting(t *testing.T) {
	authentication, usersFile := testAuthentication(t)
	observedAuthentication := &observingAuthentication{manager: authentication}
	development := &fakeDevelopment{sandbox: "dev-alice"}
	consoles := &fakeConsoles{opened: make(chan openedConsole, 8)}
	manager, err := New(Config{
		Port: 0, HostKeyPath: filepath.Join(t.TempDir(), "ssh", "host_ed25519"),
		Authentication: observedAuthentication, Development: development, Consoles: consoles,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	address := "127.0.0.1:" + stringPort(manager.Port())
	if _, err := gossh.Dial("tcp", address, clientConfig("alice", "wrong password")); err == nil {
		t.Fatal("wrong SSH password was accepted")
	}
	client, err := gossh.Dial("tcp", address, clientConfig("alice", "correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if !observedAuthentication.cleared() {
		t.Fatal("SSH password buffer was not cleared after authentication")
	}
	data, err := os.ReadFile(usersFile)
	if err != nil || bytes.Contains(data, []byte("correct horse")) {
		t.Fatalf("authentication store exposed presented password: err=%v data=%q", err, data)
	}

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdin, _ := session.StdinPipe()
	stdout, _ := session.StdoutPipe()
	if err := session.Setenv("WARP_CLIENT_VERSION", "test-version"); err != nil {
		t.Fatal(err)
	}
	if err := session.Setenv("HOME", "/untrusted"); err != nil {
		t.Fatal(err)
	}
	if err := session.RequestPty("xterm-256color", 27, 90, gossh.TerminalModes{gossh.ECHO: 1}); err != nil {
		t.Fatal(err)
	}
	if err := session.Shell(); err != nil {
		t.Fatal(err)
	}
	opened := receiveOpened(t, consoles.opened)
	if opened.kind != "development" || opened.sandboxID != "dev-alice" {
		t.Fatalf("default route = %s %s", opened.kind, opened.sandboxID)
	}
	if opened.options.Size != (backend.ConsoleSize{Columns: 90, Rows: 27}) || opened.options.WorkingDir != "/workspace" ||
		!equalStrings(opened.options.Arguments, []string{"/bin/bash", "-l"}) ||
		!containsString(opened.options.Environment, "TERM=xterm-256color") || !containsString(opened.options.Environment, "HOME=/untrusted") ||
		!containsString(opened.options.Environment, "SHELL=/bin/bash") || !containsString(opened.options.Environment, "USER=root") ||
		!containsString(opened.options.Environment, "LOGNAME=root") || !containsString(opened.options.Environment, "WARP_CLIENT_VERSION=test-version") {
		t.Fatalf("development console options = %#v", opened.options)
	}
	development.mu.Lock()
	users := append([]string(nil), development.users...)
	development.mu.Unlock()
	if !equalStrings(users, []string{"alice"}) {
		t.Fatalf("default workspace users = %v", users)
	}

	rawInput := []byte{0x03, 0x1b, '[', '2', '0', '~'}
	if _, err := stdin.Write(rawInput); err != nil {
		t.Fatal(err)
	}
	if got := receiveBytes(t, opened.terminal.input); !bytes.Equal(got, rawInput) {
		t.Fatalf("PTY input = %q, want %q", got, rawInput)
	}
	fullScreen := []byte("\x1b[?1049h\x1b[2Jfull-screen\x1b[?1049l")
	go func() { _ = opened.terminal.emit(fullScreen) }()
	if got := readBytes(t, stdout, len(fullScreen)); !bytes.Equal(got, fullScreen) {
		t.Fatalf("PTY output = %q, want %q", got, fullScreen)
	}
	if err := session.WindowChange(40, 120); err != nil {
		t.Fatal(err)
	}
	select {
	case size := <-opened.terminal.resized:
		if size != (backend.ConsoleSize{Columns: 120, Rows: 40}) {
			t.Fatalf("resize = %#v", size)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SSH window change was not relayed")
	}
	if err := session.Signal(gossh.SIGINT); err != nil {
		t.Fatal(err)
	}
	if got := receiveBytes(t, opened.terminal.input); !bytes.Equal(got, []byte{0x03}) {
		t.Fatalf("SSH INT signal = %q", got)
	}
	opened.terminal.finish()
	if err := session.Wait(); err != nil {
		t.Fatal(err)
	}

	selected, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := selected.Start("the8020 sandbox-id=sbx-ax9thsl3"); err != nil {
		t.Fatal(err)
	}
	selectedOpen := receiveOpened(t, consoles.opened)
	if selectedOpen.kind != "runtime" || selectedOpen.sandboxID != "sbx-ax9thsl3" || selectedOpen.options.WorkingDir != "/" || !containsString(selectedOpen.options.Environment, "HOME=/tmp") {
		t.Fatalf("selected route = %#v", selectedOpen)
	}
	selectedOpen.terminal.finish()
	if err := selected.Wait(); err != nil {
		t.Fatal(err)
	}

	selectedDevelopment, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := selectedDevelopment.Start("the8020 sandbox-id=dev-alice"); err != nil {
		t.Fatal(err)
	}
	selectedDevelopmentOpen := receiveOpened(t, consoles.opened)
	if selectedDevelopmentOpen.kind != "development" || selectedDevelopmentOpen.sandboxID != "dev-alice" || selectedDevelopmentOpen.options.WorkingDir != "/workspace" {
		t.Fatalf("selected development route = %#v", selectedDevelopmentOpen)
	}
	selectedDevelopmentOpen.terminal.finish()
	if err := selectedDevelopment.Wait(); err != nil {
		t.Fatal(err)
	}

	command, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	commandInput, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start("printf 'hello' && uname -a"); err != nil {
		t.Fatal(err)
	}
	commandOpen := receiveOpened(t, consoles.opened)
	if commandOpen.kind != "development" || commandOpen.sandboxID != "dev-alice" ||
		commandOpen.options.Terminal || !equalStrings(commandOpen.options.Arguments, []string{"/bin/bash", "-c", "printf 'hello' && uname -a"}) {
		t.Fatalf("command route = %#v", commandOpen)
	}
	if _, err := commandInput.Write([]byte("streamed script\n")); err != nil {
		t.Fatal(err)
	}
	if err := commandInput.Close(); err != nil {
		t.Fatal(err)
	}
	if got := receiveBytes(t, commandOpen.terminal.input); !bytes.Equal(got, []byte("streamed script\n")) {
		t.Fatalf("command stdin = %q", got)
	}
	if got := receiveBytes(t, commandOpen.terminal.input); !bytes.Equal(got, []byte{0x04}) {
		t.Fatalf("command stdin EOF = %q, want PTY EOF", got)
	}
	commandOpen.terminal.finish()
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}

	failing, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := failing.Start("exit 27"); err != nil {
		t.Fatal(err)
	}
	failingOpen := receiveOpened(t, consoles.opened)
	failingOpen.terminal.status = 27
	failingOpen.terminal.finish()
	var exitError *gossh.ExitError
	if err := failing.Wait(); !errors.As(err, &exitError) || exitError.ExitStatus() != 27 {
		t.Fatalf("remote exit status = %v", err)
	}
	if channel, _, err := client.OpenChannel("direct-tcpip", nil); err == nil {
		_ = channel.Close()
		t.Fatal("SSH forwarding channel was accepted")
	}
}

func TestSSHPublicKeyAuthenticationAndRejections(t *testing.T) {
	authentication, _ := testAuthentication(t)
	matching := testSigner(t)
	nonmatching := testSigner(t)
	authorized := append([]byte("restrict "), gossh.MarshalAuthorizedKey(matching.PublicKey())...)
	authorized = append(bytes.TrimSpace(authorized), []byte(" alice@test\n")...)
	development := &fakeDevelopment{sandbox: "dev-alice", keys: map[string][]byte{"alice": authorized}}
	consoles := &fakeConsoles{opened: make(chan openedConsole, 2)}
	manager, err := New(Config{
		Port: 0, HostKeyPath: filepath.Join(t.TempDir(), "host_ed25519"), Authentication: authentication,
		Development: development, Consoles: consoles,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	address := "127.0.0.1:" + stringPort(manager.Port())
	for name, signer := range map[string]gossh.Signer{"alice": nonmatching, "unknown": matching, "disabled": matching} {
		if client, dialErr := gossh.Dial("tcp", address, publicKeyClientConfig(name, signer)); dialErr == nil {
			_ = client.Close()
			t.Fatalf("public key was accepted for %s", name)
		}
	}
	development.mu.Lock()
	if len(development.users) != 0 {
		t.Fatalf("rejected authentication started development sandboxes: %v", development.users)
	}
	development.mu.Unlock()

	client, err := gossh.Dial("tcp", address, publicKeyClientConfig("alice", matching))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Shell(); err != nil {
		t.Fatal(err)
	}
	opened := receiveOpened(t, consoles.opened)
	if opened.kind != "development" || opened.sandboxID != "dev-alice" || !containsString(opened.options.Environment, "USER=root") || !containsString(opened.options.Environment, "HOME=/root") {
		t.Fatalf("public-key console route = %#v", opened)
	}
	opened.terminal.finish()
	if err := session.Wait(); err != nil {
		t.Fatal(err)
	}
	development.mu.Lock()
	defer development.mu.Unlock()
	if !equalStrings(development.users, []string{"alice"}) {
		t.Fatalf("public-key sandbox users = %v", development.users)
	}
}

func TestAuthorizedKeyParserRejectsMalformedFile(t *testing.T) {
	signer := testSigner(t)
	valid := gossh.MarshalAuthorizedKey(signer.PublicKey())
	if authorizedKeyMatches(append(valid, []byte("not an authorized key\n")...), signer.PublicKey()) {
		t.Fatal("matching key in malformed file was accepted")
	}
}

func TestHostKeyPersistsAndRejectsNonRegularFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "ssh", "host_ed25519")
	first, err := loadOrCreateHostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateHostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.PublicKey().Marshal(), second.PublicKey().Marshal()) {
		t.Fatal("SSH host key changed across loads")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("SSH host key mode = %v, %v", info, err)
	}
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil || directory.Mode().Perm() != 0o700 {
		t.Fatalf("SSH host key directory mode = %v, %v", directory, err)
	}
	symlink := filepath.Join(root, "linked-key")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateHostKey(symlink); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink host key error = %v", err)
	}
	realDirectory := filepath.Join(root, "real-directory")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedDirectory := filepath.Join(root, "linked-directory")
	if err := os.Symlink(realDirectory, linkedDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateHostKey(filepath.Join(linkedDirectory, "host_ed25519")); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlink host key directory error = %v", err)
	}
}

func TestExecGrammar(t *testing.T) {
	valid := map[string]string{
		"the8020":                               "",
		"the8020 sandbox-id=sbx-ax9thsl3":       "sbx-ax9thsl3",
		"  the8020   sandbox-id=sbx-1234abcd  ": "sbx-1234abcd",
		"the8020 sandbox-id=dev-alice":          "dev-alice",
	}
	for command, want := range valid {
		selected, err := parseExec(command)
		if err != nil || selected.sandboxID != want {
			t.Errorf("parse %q = %#v, %v", command, selected, err)
		}
	}
	ordinary := "printf '%s\\n' hello; uname -a"
	selected, err := parseExec(ordinary)
	if err != nil || selected.command != ordinary || selected.sandboxID != "" {
		t.Fatalf("ordinary command parse = %#v, %v", selected, err)
	}
	for _, command := range []string{
		"", "the8020 whoami=yes", "the8020 sandbox-id=sbx-short",
		"the8020 sandbox-id=dev-ab",
		"the8020 sandbox-id=dev-Alice",
		"the8020 sandbox-id=dev-alice-",
		"the8020 sandbox-id=sbx-ax9thsl3 sandbox-id=sbx-bbbbbbbb",
		"the8020 sandbox-id=sbx-ax9thsl3;uname",
	} {
		if _, err := parseExec(command); err == nil {
			t.Errorf("invalid selector %q was accepted", command)
		}
	}
}

func TestEnvironmentValidation(t *testing.T) {
	environment := map[string]string{}
	for _, request := range []envRequest{{Name: "LANG", Value: "en_US.UTF-8"}, {Name: "WARP_IS_SSH", Value: "1"}, {Name: "HOME", Value: "/untrusted"}} {
		if err := acceptEnvironment(environment, request); err != nil {
			t.Fatal(err)
		}
	}
	if environment["LANG"] != "en_US.UTF-8" || environment["WARP_IS_SSH"] != "1" {
		t.Fatalf("forwarded environment = %#v", environment)
	}
	if environment["HOME"] != "/untrusted" {
		t.Fatalf("client environment override was not retained: %#v", environment)
	}
	for _, request := range []envRequest{{Name: "BAD-NAME", Value: "x"}, {Name: "1BAD", Value: "x"}, {Name: "GOOD", Value: "x\x00y"}} {
		if err := acceptEnvironment(environment, request); err == nil {
			t.Fatalf("invalid environment request was accepted: %#v", request)
		}
	}
}

func TestCloseStopsListener(t *testing.T) {
	authentication, _ := testAuthentication(t)
	manager, err := New(Config{
		Port: 0, HostKeyPath: filepath.Join(t.TempDir(), "host_ed25519"), Authentication: authentication,
		Development: &fakeDevelopment{sandbox: "sbx-abc12345"}, Consoles: &fakeConsoles{opened: make(chan openedConsole, 1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	address := "127.0.0.1:" + stringPort(manager.Port())
	client, err := gossh.Dial("tcp", address, clientConfig("alice", "correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if connection, err := netDial(address); err == nil {
		_ = connection.Close()
		t.Fatal("SSH listener remained open after close")
	}
}

func TestRuntimePortReplacementPreservesConnectionsAndRollsBack(t *testing.T) {
	authentication, _ := testAuthentication(t)
	manager, err := New(Config{
		Port: 0, HostKeyPath: filepath.Join(t.TempDir(), "host_ed25519"), Authentication: authentication,
		Development: &fakeDevelopment{sandbox: "dev-alice"}, Consoles: &fakeConsoles{opened: make(chan openedConsole, 1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	oldAddress := "127.0.0.1:" + stringPort(manager.Port())
	existing, err := gossh.Dial("tcp", oldAddress, clientConfig("alice", "correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = existing.Close() })

	replacementPort := availableTCPPort(t)
	prepared, err := manager.Prepare(context.Background(), settings.Values{"network.ssh_port": int64(replacementPort)})
	if err != nil {
		t.Fatal(err)
	}
	prepared.Commit()
	if manager.Port() != replacementPort {
		t.Fatalf("active SSH port = %d, want %d", manager.Port(), replacementPort)
	}
	replacementAddress := "127.0.0.1:" + stringPort(replacementPort)
	replacement, err := gossh.Dial("tcp", replacementAddress, clientConfig("alice", "correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	_ = replacement.Close()
	if connection, dialErr := netDial(oldAddress); dialErr == nil {
		_ = connection.Close()
		t.Fatal("old SSH listener accepted a new connection after replacement")
	}
	session, err := existing.NewSession()
	if err != nil {
		t.Fatalf("established SSH connection did not survive replacement: %v", err)
	}
	_ = session.Close()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	occupiedPort := occupied.Addr().(*net.TCPAddr).Port
	if _, err := manager.Prepare(context.Background(), settings.Values{"network.ssh_port": int64(occupiedPort)}); !errors.Is(err, ErrPortUnavailable) {
		_ = occupied.Close()
		t.Fatalf("occupied SSH port error = %v, want ErrPortUnavailable", err)
	}
	_ = occupied.Close()
	if manager.Port() != replacementPort {
		t.Fatalf("failed replacement changed SSH port to %d", manager.Port())
	}

	discardedPort := availableTCPPort(t)
	discarded, err := manager.Prepare(context.Background(), settings.Values{"network.ssh_port": int64(discardedPort)})
	if err != nil {
		t.Fatal(err)
	}
	discarded.Discard()
	rebound, err := net.Listen("tcp", "127.0.0.1:"+stringPort(discardedPort))
	if err != nil {
		t.Fatalf("discarded SSH listener still owns its port: %v", err)
	}
	_ = rebound.Close()
	if manager.Port() != replacementPort {
		t.Fatalf("discard changed SSH port to %d", manager.Port())
	}
}

func testAuthentication(t *testing.T) (*auth.Manager, string) {
	t.Helper()
	root := t.TempDir()
	databaseFile := filepath.Join(root, "system.db")
	db := database.New(database.Config{
		Backend: database.BackendSQLite, Location: databaseFile,
		MaximumOpenConnections: 8, MaximumIdleConnections: 2,
	})
	if _, err := db.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE "the8020__users__users" ("username" TEXT PRIMARY KEY, "passwordHash" TEXT NOT NULL, "enabled" INTEGER NOT NULL, "authVersion" INTEGER NOT NULL, "createdAt" TEXT NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`,
		`CREATE TABLE "the8020__users__sessions" ("sessionId" TEXT PRIMARY KEY, "username" TEXT NOT NULL, "secretHash" TEXT NOT NULL, "authVersion" INTEGER NOT NULL, "createdAt" TEXT NOT NULL, "expiresAt" TEXT NOT NULL) STRICT`,
	} {
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	parameters := auth.Argon2Parameters{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, OutputLength: 16}
	hasher, err := auth.NewPasswordHasher(parameters, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := auth.New(auth.Config{
		Database: db,
		Argon2:   parameters,
		Hasher:   hasher,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := database.EncodeTime(db, time.Now())
	for _, user := range []struct {
		username string
		password string
		enabled  bool
	}{{"alice", "correct horse", true}, {"disabled", "unused password", false}} {
		passwordHash, err := hasher.Hash(user.password)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(context.Background(), `INSERT INTO "the8020__users__users" ("username", "passwordHash", "enabled", "authVersion", "createdAt", "updatedAt") VALUES ($1, $2, $3, 1, $4, $4)`, user.username, passwordHash, user.enabled, now); err != nil {
			t.Fatal(err)
		}
	}
	return manager, databaseFile
}

func clientConfig(username, password string) *gossh.ClientConfig {
	return &gossh.ClientConfig{
		User: username, Auth: []gossh.AuthMethod{gossh.Password(password)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), Timeout: 2 * time.Second,
	}
}

func testSigner(t *testing.T) gossh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func publicKeyClientConfig(username string, signer gossh.Signer) *gossh.ClientConfig {
	return &gossh.ClientConfig{
		User: username, Auth: []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), Timeout: 2 * time.Second,
	}
}

func receiveOpened(t *testing.T, opened <-chan openedConsole) openedConsole {
	t.Helper()
	select {
	case value := <-opened:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("console was not opened")
		return openedConsole{}
	}
}

func receiveBytes(t *testing.T, input <-chan []byte) []byte {
	t.Helper()
	select {
	case value := <-input:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("PTY input was not relayed")
		return nil
	}
}

func readBytes(t *testing.T, reader io.Reader, size int) []byte {
	t.Helper()
	result := make(chan []byte, 1)
	failure := make(chan error, 1)
	go func() {
		data := make([]byte, size)
		_, err := io.ReadFull(reader, data)
		if err != nil {
			failure <- err
			return
		}
		result <- data
	}()
	select {
	case data := <-result:
		return data
	case err := <-failure:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("PTY output was not relayed")
	}
	return nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func stringPort(port int) string {
	const digits = "0123456789"
	if port == 0 {
		return "0"
	}
	var result [5]byte
	index := len(result)
	for port > 0 {
		index--
		result[index] = digits[port%10]
		port /= 10
	}
	return string(result[index:])
}

func availableTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func netDial(address string) (io.Closer, error) {
	connection, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).Dial("tcp", address)
	if err != nil {
		return nil, err
	}
	return connection, nil
}
