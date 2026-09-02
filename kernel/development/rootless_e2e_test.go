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

func TestRootlessDevelopmentOverlayProbe(t *testing.T) {
	if os.Getenv("THE8020_DEVELOPMENT_OVERLAY_PROBE") != "1" {
		t.Skip("set THE8020_DEVELOPMENT_OVERLAY_PROBE=1 to probe the pinned gVisor overlay")
	}
	sourceRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	runsc := filepath.Join(sourceRoot, ".development", "runtime", "gvisor", "bin", "runsc")
	imageRoot := filepath.Join(sourceRoot, ".development", "runtime", "development", "rootfs")
	root := t.TempDir()
	rootfs := filepath.Join(root, "rootfs")
	if err := copySystemRoot(context.Background(), imageRoot, rootfs); err != nil {
		t.Fatal(err)
	}
	packages := filepath.Join(root, "packages")
	writeTestFile(t, filepath.Join(packages, "unchanged.txt"), "lower-one\n")
	writeTestFile(t, filepath.Join(packages, "changed.txt"), "lower-one\n")
	driver := NewRootlessDriver(RootlessConfig{
		RunscPath: runsc, RuntimeRoot: filepath.Join(root, "runsc"),
		SandboxRoot: filepath.Join(root, "sandboxes"), LogRoot: filepath.Join(root, "logs"),
	})
	start := SandboxStart{
		UserID: "overlayprobe", SandboxID: "dev-overlayprobe", Packages: packages, RootFS: rootfs,
		Mounts: []SandboxMount{
			{MountDefinition: MountDefinition{ID: "packages", Target: "/workspace/packages", Behavior: MountSandboxSource, Writable: true}, HostSource: packages},
			{MountDefinition: MountDefinition{ID: "temporary", Target: "/tmp", Behavior: MountEphemeral, Writable: true}},
		},
	}
	if err := driver.Start(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		_ = driver.Kill(context.Background(), start.SandboxID)
		_ = driver.Delete(context.Background(), start.SandboxID)
	}
	defer cleanup()
	if _, err := driver.Exec(context.Background(), start.SandboxID, "printf private > /workspace/packages/changed.txt; printf added > /workspace/packages/added.txt"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(packages, "changed.txt")); err != nil || string(data) != "lower-one\n" {
		t.Fatalf("overlay changed lower file: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(packages, "added.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("overlay created a lower file: %v", err)
	}
	writeTestFile(t, filepath.Join(packages, "unchanged.txt"), "lower-two\n")
	if output, err := driver.Exec(context.Background(), start.SandboxID, "cat /workspace/packages/unchanged.txt; cat /workspace/packages/changed.txt"); err != nil || string(output) != "lower-two\nprivate" {
		t.Fatalf("overlay lower refresh/private view = %q, %v", output, err)
	}
	cleanup()
	if err := driver.Start(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	if output, err := driver.Exec(context.Background(), start.SandboxID, "cat /workspace/packages/changed.txt; test ! -e /workspace/packages/added.txt"); err != nil || string(output) != "lower-one\n" {
		t.Fatalf("fresh overlay after restart = %q, %v", output, err)
	}
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
	for _, path := range []string{runsc, imageRecord} {
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
	if err := copyDirectory(context.Background(), filepath.Join(sourceRoot, "defaults", "scripts"), filepath.Join(root, "scripts")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"the8020/dev-core", "the8020/demo"} {
		packageRoot := filepath.Join(packages, filepath.FromSlash(id))
		writeTestFile(t, filepath.Join(packageRoot, "package.toml"), "schema = 1\n")
		writeTestFile(t, filepath.Join(packageRoot, "src", "message.ts"), "export const message = \"shared\";\n")
		if id == "the8020/dev-core" {
			writeTestFile(t, filepath.Join(packageRoot, ".gitignore"), "ignored/\n")
		}
	}
	driver := NewRunscDriver(RunscConfig{RunscPath: runsc, RuntimeRoot: filepath.Join(runtimeRoot, "runsc"), SandboxRoot: filepath.Join(runtimeRoot, "sandboxes"), LogRoot: filepath.Join(runtimeRoot, "logs"), Rootless: rootless, ignoreCgroups: !rootless && os.Getenv("THE8020_DEVELOPMENT_ROOTFUL_IGNORE_CGROUPS") == "1"})
	registry := core.NewRegistry(nil)
	manager, err := New(Config{Root: root, PackagesRoot: packages, ConfigRoot: filepath.Join(root, "config"), UsersRoot: users, RuntimeRoot: runtimeRoot, ImageRoot: imageRoot, ImageRecord: imageRecord, Driver: driver, ActivationGateway: NewCommandBusGateway(registry)})
	if err != nil {
		t.Fatal(err)
	}
	registerTestActivationCommands(t, registry, manager)
	for _, id := range []string{"the8020/dev-core", "the8020/demo"} {
		initializeTestRepository(t, manager, id, "Developer", "developer@example.test", "Initial")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Errorf("close development manager: %v", err)
		}
	})

	sandbox, err := manager.Create(context.Background(), "developer")
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.SourcePath != packages || !strings.HasPrefix(sandbox.SystemPath, filepath.Join(users, "developer", "dev-sandbox")) {
		t.Fatalf("sandbox does not use shared lower and per-user system storage: %#v", sandbox)
	}
	if sandbox.SandboxID != "dev-developer" {
		t.Fatalf("development sandbox ID = %q", sandbox.SandboxID)
	}
	proveInteractiveConsole(t, manager, sandbox.SandboxID)
	proveSSHConsole(t, root, manager)
	if _, err := os.Stat(filepath.Join(packages, "the8020", "dev-core", "ssh-proof.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SSH package edit escaped the private overlay: %v", err)
	}
	shell(t, manager, sandbox.UserID, "test \"$(cat /proc/1/comm)\" = sleep && test \"$(id -u)\" = 0 && test \"$HOME:$USER:$LOGNAME\" = /root:root:root && ! getent passwd developer && test ! -e /home/developer && test ! -e /opt/development/snapshot && ! command -v codex && ! command -v node && ! command -v nodejs && ! command -v npm && ! command -v npx && deno --version && git --version")
	shell(t, manager, sandbox.UserID, "test \"$(command -v activate)\" = /workspace/scripts/activate && test -x /workspace/scripts/activate && ! sh -c 'printf no > /workspace/scripts/not-writable'")
	shell(t, manager, sandbox.UserID, "test \"$(stat -c %a /run/lock)\" = 1777 && test \"$(readlink /var/lock)\" = /run/lock && printf lock-ok > /var/lock/the8020-proof && rm /var/lock/the8020-proof && printf transient > /run/the8020-transient")
	shell(t, manager, sandbox.UserID, "apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends aptitude && aptitude --version")
	shell(t, manager, sandbox.UserID, "install -o 42 -g 4 -m 0640 /dev/null /var/tmp/the8020-idmap-proof && test \"$(stat -c %u:%g /var/tmp/the8020-idmap-proof)\" = 42:4 && rm /var/tmp/the8020-idmap-proof")
	shell(t, manager, sandbox.UserID, "mkdir -p /tmp/the8020-proof/DEBIAN /tmp/the8020-proof/usr/local/bin /tmp/the8020-proof/usr/share/the8020-proof /root/.config/editor /workspace/packages/the8020/dev-core/ignored && printf 'Package: the8020-proof\\nVersion: 1\\nArchitecture: all\\nMaintainer: 80|20 Test <test@example.test>\\nDescription: proof\\n' > /tmp/the8020-proof/DEBIAN/control && printf '#!/bin/sh\\necho system-ok\\n' > /tmp/the8020-proof/usr/local/bin/the8020-proof && printf 'directory-ok\\n' > /tmp/the8020-proof/usr/share/the8020-proof/value && chmod 755 /tmp/the8020-proof/usr/local/bin/the8020-proof && dpkg-deb --build /tmp/the8020-proof /tmp/the8020-proof.deb && /usr/bin/dpkg --unpack /tmp/the8020-proof.deb && /usr/bin/dpkg --configure the8020-proof && grep -F directory-ok /usr/share/the8020-proof/value && printf 'home-ok\\n' > /root/.config/editor/proof && printf 'private\\n' > /workspace/packages/the8020/dev-core/src/message.ts && printf 'generated\\n' > /workspace/packages/the8020/dev-core/ignored/generated.dat")
	if shared, _ := os.ReadFile(filepath.Join(packages, "the8020", "dev-core", "src", "message.ts")); strings.Contains(string(shared), "private") {
		t.Fatal("private package edit changed the shared repository before activation")
	}
	oldSandbox := sandbox.SandboxID
	oldLogMarker := filepath.Join(runtimeRoot, "logs", oldSandbox, "old-generation")
	if err := os.WriteFile(oldLogMarker, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Restart(context.Background(), sandbox.UserID); err != nil {
		t.Fatal(err)
	}
	sandbox, _ = manager.Inspect(sandbox.UserID)
	if sandbox.SandboxID != oldSandbox {
		t.Fatal("restart changed the deterministic development sandbox identity")
	}
	if _, err := os.Stat(oldLogMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restarted sandbox retained disposable logs: %v", err)
	}
	shell(t, manager, sandbox.UserID, "test ! -e /run/the8020-transient && test ! -e /workspace/packages/the8020/dev-core/ignored/generated.dat && grep -F private /workspace/packages/the8020/dev-core/src/message.ts && grep -F home-ok /root/.config/editor/proof && test \"$(the8020-proof)\" = system-ok && dpkg-query -W the8020-proof && aptitude --version")

	previewJSON := shell(t, manager, sandbox.UserID, "activate --preview --message Preview")
	var preview ActivationPreview
	if err := json.Unmarshal([]byte(previewJSON), &preview); err != nil || len(preview.Packages) != 1 || preview.Packages[0].ChangedFiles != 2 || preview.Packages[0].AddedRows != 2 || preview.Packages[0].RemovedRows != 1 {
		t.Fatalf("helper preview = %q, %v", previewJSON, err)
	}
	activationJSON := shell(t, manager, sandbox.UserID, "activate --message Activate --author-name Developer --author-email developer@example.test")
	var activation ActivationResult
	if err := json.Unmarshal([]byte(activationJSON), &activation); err != nil || !activation.Success {
		t.Fatalf("helper activation = %q, %v", activationJSON, err)
	}
	waitForOverlayReset(t, manager, sandbox.UserID)
	if contents, err := os.ReadFile(filepath.Join(packages, "the8020", "dev-core", "ssh-proof.txt")); err != nil || string(contents) != "changed through SSH\n" {
		t.Fatalf("SSH package edit was not activated: %q, %v", contents, err)
	}
	message, err := gitOutput(filepath.Join(packages, "the8020", "dev-core"), "log", "-1", "--pretty=%B")
	if err != nil || !strings.Contains(message, "[the8020.activation]") || !strings.Contains(message, `"metadata_client" = "sandbox-helper"`) || !strings.Contains(message, `"sandbox" = "`+sandbox.SandboxID+`"`) {
		t.Fatalf("activation TOML metadata = %q, %v", message, err)
	}
	current, _ := manager.Inspect(sandbox.UserID)
	if current.SandboxID != sandbox.SandboxID {
		t.Fatal("activation recreated the native-storage sandbox")
	}
	shell(t, manager, sandbox.UserID, "grep -F private /workspace/packages/the8020/dev-core/src/message.ts && grep -F home-ok /root/.config/editor/proof && test \"$(the8020-proof)\" = system-ok")
	shell(t, manager, sandbox.UserID, "printf 'second activation\\n' > /workspace/packages/the8020/demo/second-activation.txt")
	if _, err := os.Stat(filepath.Join(packages, "the8020", "demo", "second-activation.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second private edit escaped the overlay: %v", err)
	}
	secondJSON := shell(t, manager, sandbox.UserID, "activate --message 'Activate again'")
	var second ActivationResult
	if err := json.Unmarshal([]byte(secondJSON), &second); err != nil || !second.Success || packageResult(second, "the8020/demo").Status != "committed" {
		t.Fatalf("second helper activation = %q, %v", secondJSON, err)
	}
	waitForOverlayReset(t, manager, sandbox.UserID)
	if contents, err := os.ReadFile(filepath.Join(packages, "the8020", "demo", "second-activation.txt")); err != nil || string(contents) != "second activation\n" {
		t.Fatalf("second activation was not published: %q, %v", contents, err)
	}
	secondSubject, err := gitOutput(filepath.Join(packages, "the8020", "demo"), "log", "-1", "--pretty=%s")
	if err != nil || secondSubject != "Activate again" {
		t.Fatalf("second activation commit = %q, %v", secondSubject, err)
	}

	if _, err := manager.ResetSource(context.Background(), sandbox.UserID, true); err != nil {
		t.Fatal(err)
	}
	shell(t, manager, sandbox.UserID, "grep -F private /workspace/packages/the8020/dev-core/src/message.ts && grep -F home-ok /root/.config/editor/proof && test \"$(the8020-proof)\" = system-ok")
	if _, err := manager.FactoryReset(context.Background(), sandbox.UserID, true); err != nil {
		t.Fatal(err)
	}
	shell(t, manager, sandbox.UserID, "test ! -e /root/.config/editor/proof && test ! -e /home/developer && ! getent passwd developer && ! command -v the8020-proof && ! command -v aptitude && grep -F private /workspace/packages/the8020/dev-core/src/message.ts")
}

func waitForOverlayReset(t *testing.T, manager *Manager, userID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		sandbox, inspectErr := manager.Inspect(userID)
		if inspectErr == nil && sandbox.State == StateReady && sandbox.LastActivationResult != nil && sandbox.LastActivationResult.OverlayReset && !sandbox.LastActivationResult.OverlayResetPending {
			preview, previewErr := manager.Preview(context.Background(), userID, ActivationOptions{})
			if previewErr == nil && len(preview.Packages) == 0 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("development overlay did not reset after helper activation")
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
		"printf 'changed through SSH\\n' > /workspace/packages/the8020/dev-core/ssh-proof.txt; " +
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
