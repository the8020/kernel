package development

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"the8020/kernel/auth"
	"the8020/kernel/cbus/core"
	platformconsole "the8020/kernel/console"
	"the8020/kernel/sandbox/backend"
	sshserver "the8020/kernel/ssh"
)

func TestRootlessDevelopmentE2E(t *testing.T) {
	if os.Getenv("THE8020_DEVELOPMENT_E2E") != "1" {
		t.Skip("set THE8020_DEVELOPMENT_E2E=1 after portable runtime installation")
	}
	runDevelopmentE2E(t, true)
}

func TestRootfulDevelopmentE2E(t *testing.T) {
	if os.Getenv("THE8020_DEVELOPMENT_ROOTFUL_E2E") != "1" {
		t.Skip("set THE8020_DEVELOPMENT_ROOTFUL_E2E=1 on a fully authorized host")
	}
	runDevelopmentE2E(t, false)
}

func runDevelopmentE2E(t *testing.T, rootless bool) {
	t.Helper()
	sourceRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	runsc := filepath.Join(sourceRoot, ".development", "runtime", "gvisor", "bin", "runsc")
	if !rootless {
		runsc, err = exec.LookPath("runsc")
		if err != nil {
			t.Fatal(err)
		}
		runsc, _ = filepath.Abs(runsc)
	}
	imageRoot := filepath.Join(sourceRoot, ".development", "runtime", "development", "rootfs")
	imageRecord := filepath.Join(sourceRoot, ".development", "runtime", "development", "image.json")
	for _, path := range []string{runsc, filepath.Join(imageRoot, "usr", "local", "bin", "codex"), imageRecord} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("development E2E prerequisite %s: %v", path, err)
		}
	}

	root := t.TempDir()
	packages := filepath.Join(root, "packages")
	users := filepath.Join(root, "users")
	runtimeRoot := filepath.Join(root, "node", "kernel", "runtime", "development")
	for _, directory := range []string{packages, users, runtimeRoot, filepath.Join(runtimeRoot, "runsc"), filepath.Join(runtimeRoot, "sandboxes"), filepath.Join(runtimeRoot, "logs")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"the8020/dev-core", "the8020/demo"} {
		packageRoot := filepath.Join(packages, filepath.FromSlash(id))
		writeTestFile(t, filepath.Join(packageRoot, "package.toml"), "schema = 1\n")
		writeTestFile(t, filepath.Join(packageRoot, "src", "message.ts"), "export const message = \"shared\";\n")
	}
	driver := NewRunscDriver(RunscConfig{RunscPath: runsc, RuntimeRoot: filepath.Join(runtimeRoot, "runsc"), SandboxRoot: filepath.Join(runtimeRoot, "sandboxes"), LogRoot: filepath.Join(runtimeRoot, "logs"), Rootless: rootless, ignoreCgroups: !rootless && os.Getenv("THE8020_DEVELOPMENT_ROOTFUL_IGNORE_CGROUPS") == "1"})
	registry := core.NewRegistry(nil)
	manager, err := New(Config{Root: root, PackagesRoot: packages, ConfigRoot: filepath.Join(root, "config"), UsersRoot: users, RuntimeRoot: runtimeRoot, ImageRoot: imageRoot, ImageRecord: imageRecord, Driver: driver, ActivationGateway: NewCommandBusGateway(registry)})
	if err != nil {
		t.Fatal(err)
	}
	registerTestActivationCommands(t, registry, manager)
	for _, id := range []string{"the8020/dev-core", "the8020/demo"} {
		if _, err := manager.InitializeRepository(context.Background(), id, "Developer", "developer@example.test", "Initial"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Errorf("close development manager: %v", err)
		}
	})

	workspace, err := manager.Create(context.Background(), "developer")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(workspace.SourcePath, filepath.Join(users, "developer")) || !strings.HasPrefix(workspace.SystemPath, filepath.Join(users, "developer")) {
		t.Fatalf("workspace does not use native per-user storage: %#v", workspace)
	}
	if workspace.ActiveSandboxID != "dev-developer" {
		t.Fatalf("development sandbox ID = %q", workspace.ActiveSandboxID)
	}
	proveInteractiveConsole(t, manager, workspace.ActiveSandboxID)
	proveSSHConsole(t, root, manager)
	shell(t, manager, workspace.WorkspaceID, "test \"$(cat /proc/1/comm)\" = sleep && test \"$(id -u)\" = 0 && test \"$HOME:$USER:$LOGNAME\" = /root:root:root && ! getent passwd developer && test ! -e /home/developer && test ! -e /opt/development/snapshot && codex --version && deno --version && git --version")
	shell(t, manager, workspace.WorkspaceID, "test \"$(stat -c %a /run/lock)\" = 1777 && test \"$(readlink /var/lock)\" = /run/lock && printf lock-ok > /var/lock/the8020-proof && rm /var/lock/the8020-proof && printf transient > /run/the8020-transient")
	shell(t, manager, workspace.WorkspaceID, "install -o 42 -g 4 -m 0640 /dev/null /var/tmp/the8020-idmap-proof && test \"$(stat -c %u:%g /var/tmp/the8020-idmap-proof)\" = 42:4 && rm /var/tmp/the8020-idmap-proof")
	shell(t, manager, workspace.WorkspaceID, "mkdir -p /tmp/the8020-proof/DEBIAN /tmp/the8020-proof/usr/local/bin /tmp/the8020-proof/usr/share/the8020-proof /root/.codex && printf 'Package: the8020-proof\\nVersion: 1\\nArchitecture: all\\nMaintainer: 80|20 Test <test@example.test>\\nDescription: proof\\n' > /tmp/the8020-proof/DEBIAN/control && printf '#!/bin/sh\\necho system-ok\\n' > /tmp/the8020-proof/usr/local/bin/the8020-proof && printf 'directory-ok\\n' > /tmp/the8020-proof/usr/share/the8020-proof/value && chmod 755 /tmp/the8020-proof/usr/local/bin/the8020-proof && dpkg-deb --build /tmp/the8020-proof /tmp/the8020-proof.deb && /usr/bin/dpkg --unpack /tmp/the8020-proof.deb && /usr/bin/dpkg --configure the8020-proof && grep -F directory-ok /usr/share/the8020-proof/value && printf 'home-ok\\n' > /root/.codex/proof && printf 'private\\n' > /workspace/packages/the8020/dev-core/src/message.ts")
	if shared, _ := os.ReadFile(filepath.Join(packages, "the8020", "dev-core", "src", "message.ts")); strings.Contains(string(shared), "private") {
		t.Fatal("private source changed shared repository before activation")
	}
	oldSandbox := workspace.ActiveSandboxID
	oldLogMarker := filepath.Join(runtimeRoot, "logs", oldSandbox, "old-generation")
	if err := os.WriteFile(oldLogMarker, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Restart(context.Background(), workspace.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	workspace, _ = manager.Inspect(workspace.WorkspaceID)
	if workspace.ActiveSandboxID != oldSandbox {
		t.Fatal("restart changed the deterministic development sandbox identity")
	}
	if _, err := os.Stat(oldLogMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restarted sandbox retained disposable logs: %v", err)
	}
	shell(t, manager, workspace.WorkspaceID, "test ! -e /run/the8020-transient && grep -F private /workspace/packages/the8020/dev-core/src/message.ts && grep -F home-ok /root/.codex/proof && test \"$(the8020-proof)\" = system-ok && dpkg-query -W the8020-proof")

	previewJSON := shell(t, manager, workspace.WorkspaceID, "activate --preview --message Preview")
	var preview ActivationPreview
	if err := json.Unmarshal([]byte(previewJSON), &preview); err != nil || len(preview.Packages) != 1 {
		t.Fatalf("helper preview = %q, %v", previewJSON, err)
	}
	activationJSON := shell(t, manager, workspace.WorkspaceID, "activate --message Activate --author-name Developer --author-email developer@example.test")
	var activation ActivationResult
	if err := json.Unmarshal([]byte(activationJSON), &activation); err != nil || !activation.Success {
		t.Fatalf("helper activation = %q, %v", activationJSON, err)
	}
	current, _ := manager.Inspect(workspace.WorkspaceID)
	if current.ActiveSandboxID != workspace.ActiveSandboxID {
		t.Fatal("activation recreated the native-storage sandbox")
	}
	shell(t, manager, workspace.WorkspaceID, "grep -F private /workspace/packages/the8020/dev-core/src/message.ts && grep -F home-ok /root/.codex/proof && test \"$(the8020-proof)\" = system-ok")

	if _, err := manager.ResetSource(context.Background(), workspace.WorkspaceID, true); err != nil {
		t.Fatal(err)
	}
	shell(t, manager, workspace.WorkspaceID, "grep -F private /workspace/packages/the8020/dev-core/src/message.ts && grep -F home-ok /root/.codex/proof && test \"$(the8020-proof)\" = system-ok")
	if _, err := manager.FactoryReset(context.Background(), workspace.WorkspaceID, true); err != nil {
		t.Fatal(err)
	}
	shell(t, manager, workspace.WorkspaceID, "test ! -e /root/.codex/proof && test ! -e /home/developer && ! getent passwd developer && ! command -v the8020-proof && grep -F private /workspace/packages/the8020/dev-core/src/message.ts")
}

func proveSSHConsole(t *testing.T, root string, developmentManager *Manager) {
	t.Helper()
	authentication, err := auth.New(auth.Config{
		UsersFile:    filepath.Join(root, "config", "auth", "bootstrap-users.toml"),
		SessionsRoot: filepath.Join(root, "state", "auth", "bootstrap-sessions"),
		Argon2:       auth.Argon2Parameters{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, OutputLength: 16},
		LockTimeout:  time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	const password = "development-ssh-proof"
	if _, err := authentication.AddUser(context.Background(), "developer", password); err != nil {
		t.Fatal(err)
	}
	consoleManager, err := platformconsole.New(platformconsole.Config{Authentication: authentication, Development: developmentManager})
	if err != nil {
		t.Fatal(err)
	}
	defer consoleManager.Close()
	sshManager, err := sshserver.New(sshserver.Config{
		Port: 0, HostKeyPath: filepath.Join(root, "node", "kernel", "ssh", "host_ed25519"),
		Authentication: authentication, Development: developmentManager, Consoles: consoleManager,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sshManager.Close(context.Background())
	client, err := gossh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", sshManager.Port()), &gossh.ClientConfig{
		User: "developer", Auth: []gossh.AuthMethod{gossh.Password(password)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	input, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.RequestPty("xterm", 27, 90, gossh.TerminalModes{gossh.ECHO: 1}); err != nil {
		t.Fatal(err)
	}
	if err := session.Shell(); err != nil {
		t.Fatal(err)
	}
	if err := session.WindowChange(40, 120); err != nil {
		t.Fatal(err)
	}
	promptTranscript := readSSHUntil(t, output, []byte("/workspace"))
	if bytes.Contains(promptTranscript, []byte("bash-")) || !bytes.Contains(promptTranscript, []byte("root@dev-developer")) ||
		!bytes.Contains(promptTranscript, []byte("\x1b[1;32m")) || !bytes.Contains(promptTranscript, []byte("\x1b[1;34m")) {
		t.Fatalf("initial SSH prompt is not contextual: %q", promptTranscript)
	}
	if _, err := io.WriteString(input, "cd packages\n"); err != nil {
		t.Fatal(err)
	}
	promptTranscript = append(promptTranscript, readSSHUntil(t, output, []byte("/workspace/packages"))...)
	command := "printf '\\036SSH:%s:%s\\037\\n' \"$BASH_VERSION\" \"$(stty size)\"; " +
		"printf '\\036NANO-READY\\037\\n'; nano /tmp/the8020-ssh-nano; printf '\\036NANO-EXIT:%s\\037\\n' \"$?\"; " +
		"printf '\\033[?1049h\\036FULLSCREEN\\037\\033[?1049l\\n'; " +
		"printf '\\036KEY-READY\\037\\n'; IFS= read -rsn 5 key; " +
		"if [[ $key == $'\\e[15~' ]]; then printf '\\036F5-OK\\037\\n'; else printf '\\036F5-BAD:%q\\037\\n' \"$key\"; fi; " +
		"trap 'printf \"\\036INT-OK\\037\\n\"; exit' INT; printf '\\036INT-READY\\037\\n'; while :; do read -r ignored; done\n"
	if _, err := io.WriteString(input, command); err != nil {
		t.Fatal(err)
	}
	transcript := append(promptTranscript, readSSHUntil(t, output, []byte("\x1eNANO-READY\x1f"))...)
	if _, err := input.Write([]byte{0x18}); err != nil {
		t.Fatal(err)
	}
	transcript = append(transcript, readSSHUntil(t, output, []byte("\x1eKEY-READY\x1f"))...)
	if _, err := input.Write([]byte("\x1b[15~")); err != nil {
		t.Fatal(err)
	}
	transcript = append(transcript, readSSHUntil(t, output, []byte("\x1eINT-READY\x1f"))...)
	if _, err := input.Write([]byte{0x03}); err != nil {
		t.Fatal(err)
	}
	transcript = append(transcript, readSSHUntil(t, output, []byte("\x1eINT-OK\x1f"))...)
	if err := session.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, proof := range [][]byte{
		[]byte("\x1eSSH:"), []byte(":40 120\x1f"), []byte("\x1eNANO-EXIT:0\x1f"),
		[]byte("\x1b[?1049h\x1eFULLSCREEN\x1f\x1b[?1049l"),
		[]byte("\x1eF5-OK\x1f"), []byte("\x1eINT-OK\x1f"),
	} {
		if !bytes.Contains(transcript, proof) {
			t.Fatalf("SSH PTY proof %q missing from %q", proof, transcript)
		}
	}
	if bytes.Contains(transcript, []byte("Error opening terminal")) {
		t.Fatalf("full-screen application failed through SSH: %q", transcript)
	}
}

func readSSHUntil(t *testing.T, reader io.Reader, marker []byte) []byte {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	finished := make(chan result, 1)
	go func() {
		buffer := make([]byte, 0, 4096)
		one := make([]byte, 1024)
		for len(buffer) < 1<<20 {
			count, err := reader.Read(one)
			if count > 0 {
				buffer = append(buffer, one[:count]...)
				if bytes.Contains(buffer, marker) {
					finished <- result{data: buffer}
					return
				}
			}
			if err != nil {
				finished <- result{data: buffer, err: err}
				return
			}
		}
		finished <- result{data: buffer, err: errors.New("SSH transcript exceeded one MiB")}
	}()
	select {
	case value := <-finished:
		if value.err != nil {
			t.Fatalf("read SSH transcript through %q: %v (%q)", marker, value.err, value.data)
		}
		return value.data
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for SSH transcript marker %q", marker)
		return nil
	}
}

func proveInteractiveConsole(t *testing.T, manager *Manager, sandboxID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	console, err := manager.OpenConsole(ctx, sandboxID, backend.ConsoleOptions{Arguments: []string{"/bin/bash", "-l"}, Environment: []string{"TERM=xterm-256color", "HOME=/root", "USER=root", "LOGNAME=root", "PATH=" + developmentPath}, WorkingDir: "/workspace", Size: backend.ConsoleSize{Columns: 90, Rows: 27}})
	if err != nil {
		t.Fatal(err)
	}
	defer console.Close()
	if _, err := io.WriteString(console, "printf '\\036CONSOLE:%s\\037\\n' \"$BASH_VERSION\"; pwd; exit\n"); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(console)
	if err != nil || !bytes.Contains(output, []byte("\x1eCONSOLE:")) || !bytes.Contains(output, []byte("/workspace")) {
		t.Fatalf("console output = %q, %v", output, err)
	}
}
