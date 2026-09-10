package development

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"the8020/kernel/cbus/core"
)

func (d *workspace) isSharedGitObject(name string, info os.FileInfo) bool {
	relative, err := filepath.Rel(filepath.Join(d.storage, "borrowed"), name)
	parts := strings.SplitN(relative, string(filepath.Separator), 3)
	if err != nil || len(parts) != 3 || parts[0] == ".." {
		return false
	}
	original, err := os.Stat(filepath.Join(d.shared, parts[0], parts[1], ".git", parts[2]))
	return err == nil && os.SameFile(info, original)
}

// Discard exactly one completed release response at the transport boundary.
type dropReleaseReply struct {
	net.Conn
	pending, dropped bool
}

func (c *dropReleaseReply) Write(value []byte) (int, error) {
	if !c.dropped && strings.Contains(string(value), `"action":"release"`) {
		c.pending = true
	}
	return c.Conn.Write(value)
}

func (c *dropReleaseReply) Read(value []byte) (int, error) {
	if c.pending {
		c.pending, c.dropped = false, true
		var reply map[string]any
		if err := json.NewDecoder(io.LimitReader(c.Conn, 4<<20)).Decode(&reply); err != nil {
			return 0, err
		}
		return 0, errors.New("injected lost capture-release reply")
	}
	return c.Conn.Read(value)
}

func nativeRuntime(t *testing.T) (*Manager, *RunscDriver, string) {
	t.Helper()
	if os.Getenv("THE8020_DEVELOPMENT_E2E") != "1" {
		t.Skip("set THE8020_DEVELOPMENT_E2E=1 for native workspace checks")
	}
	source, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "8020-ws-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	packages := filepath.Join(root, "packages")
	for _, directory := range []string{packages} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	nativeStagePackage(t, filepath.Join(filepath.Dir(source), "dev-skills"), filepath.Join(packages, "the8020/dev-skills"))
	installTestDevelopmentAssets(t, root)
	repository := filepath.Join(packages, "the8020/dev-core")
	writeTestFile(t, filepath.Join(repository, "package.toml"), "schema = 1\n")
	writeTestFile(t, filepath.Join(repository, ".gitignore"), "ignored/\n")
	writeTestFile(t, filepath.Join(repository, "same.txt"), "base\n")
	writeTestFile(t, filepath.Join(repository, "disjoint.txt"), "first\n2\n3\n4\n5\n6\n7\nlast\n")
	writeTestFile(t, filepath.Join(repository, "untouched.txt"), "before\n")
	runtimeRoot := filepath.Join(root, "node/kernel/runtime/development")
	driver := NewRootlessDriver(RootlessConfig{
		RunscPath:   filepath.Join(source, ".development/runtime-bin/runsc"),
		RuntimeRoot: filepath.Join(runtimeRoot, "runsc"),
		SandboxRoot: filepath.Join(runtimeRoot, "sandboxes"), LogRoot: filepath.Join(runtimeRoot, "logs"),
	})
	registry := core.NewRegistry(nil)
	manager, err := New(Config{Root: root, PackagesRoot: packages,
		UsersRoot: filepath.Join(root, "users"), RuntimeRoot: runtimeRoot,
		ImageRoot:   filepath.Join(source, ".development/runtime/development/rootfs"),
		ImageRecord: filepath.Join(source, ".development/runtime/development/image.json"),
		Driver:      driver, ActivationGateway: NewCommandBusGateway(registry),
	})
	if err != nil {
		t.Fatal(err)
	}
	registerTestActivationCommands(t, registry, manager)
	initializeTestRepository(t, manager, "the8020/dev-core", "Fixture", "fixture@example.test", "Base")
	initializeTestRepository(t, manager, "the8020/dev-skills", "Fixture", "fixture@example.test", "Guidance fixture")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})
	return manager, driver, repository
}

// Stage real package working sources, including newly authored files, without
// carrying host Git metadata, ignored artifacts or environment files.
func nativeStagePackage(t *testing.T, source, destination string) {
	t.Helper()
	files, err := gitCommand(context.Background(), source, nil, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range strings.Split(strings.TrimSuffix(files, "\x00"), "\x00") {
		if file == "" || strings.HasPrefix(filepath.Base(file), ".env") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(source, file))
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(source, file))
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(destination, file), string(body))
		if err := os.Chmod(filepath.Join(destination, file), info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
}

func nativeExec(t *testing.T, driver *RunscDriver, sandbox Sandbox, command string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	output, err := driver.Exec(ctx, sandbox.SandboxID, command)
	if err != nil {
		t.Fatalf("exec %q: %v: %s", command, err, output)
	}
	return string(output)
}

// Export and verify private Git commits through the ordinary sandbox filesystem.
func nativeExportCommit(t *testing.T, d *RunscDriver, sandbox Sandbox, shared, commit string, common ...string) int64 {
	t.Helper()
	ctx := context.Background()
	prefix := "set -e; cd /workspace/packages/the8020/dev-core; "
	ref := "refs/heads/test-export"
	nativeExec(t, d, sandbox, prefix+"git update-ref "+ref+" "+shellQuote(commit))
	path := filepath.Join(t.TempDir(), "private.bundle")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	command := prefix + "git bundle create - " + ref
	for _, base := range common {
		command += " " + shellQuote("^"+base)
	}
	err = d.ExecStream(ctx, sandbox.SandboxID, command, nil, file)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("export native bundle: %v, %v", err, closeErr)
	}
	if _, err := gitCommand(ctx, shared, nil, "-c", "fetch.fsckObjects=true", "fetch", path, ref); err != nil {
		t.Fatal(err)
	}
	head, err := gitCommand(ctx, shared, nil, "rev-parse", "FETCH_HEAD")
	if err != nil || strings.TrimSpace(string(head)) != commit {
		t.Fatalf("exported commit changed: %q, %v", head, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
