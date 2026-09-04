package development

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"the8020/kernel/deployment"

	"the8020/kernel/cbus/core"
)

type recordingActivationSchemaHook struct {
	prepared  []deployment.Candidate
	completed []bool
}

func (h *recordingActivationSchemaHook) Prepare(_ context.Context, candidates []deployment.Candidate) error {
	h.prepared = append([]deployment.Candidate(nil), candidates...)
	return nil
}

func (h *recordingActivationSchemaHook) Complete(_ context.Context, activated bool) error {
	h.completed = append(h.completed, activated)
	return nil
}

type fakeView struct {
	start     SandboxStart
	packages  string
	temporary string
	paused    bool
	running   bool
}

type fakeDriver struct {
	mu       sync.Mutex
	views    map[string]*fakeView
	starts   int
	execs    int
	startErr error
	listWait <-chan struct{}
}

func newFakeDriver() *fakeDriver { return &fakeDriver{views: map[string]*fakeView{}} }

func (d *fakeDriver) List(ctx context.Context) ([]string, error) {
	if d.listWait != nil {
		select {
		case <-d.listWait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := make([]string, 0, len(d.views))
	for id := range d.views {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (d *fakeDriver) Start(ctx context.Context, start SandboxStart) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.startErr != nil {
		return d.startErr
	}
	private, err := os.MkdirTemp(filepath.Dir(start.RootFS), ".fake-packages-")
	if err != nil {
		return err
	}
	if err := os.Remove(private); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(filepath.Dir(start.RootFS), ".fake-temporary-")
	if err != nil {
		return err
	}
	if err := copyDirectory(ctx, start.Packages, private); err != nil {
		_ = os.RemoveAll(temporary)
		return err
	}
	d.starts++
	d.views[start.SandboxID] = &fakeView{start: start, packages: private, temporary: temporary, running: true}
	return nil
}

func (d *fakeDriver) Exec(_ context.Context, id, command string) ([]byte, error) {
	if strings.Contains(command, "/workspace/packages") || strings.HasPrefix(command, "git ") || strings.HasPrefix(command, "set ") {
		output := &boundedBuffer{limit: commandOutputLimit}
		err := d.ExecStream(context.Background(), id, command, nil, output)
		return []byte(output.RawString()), err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.execs++
	view := d.views[id]
	if view == nil || !view.running || view.paused {
		return nil, errors.New("sandbox is not available")
	}
	fields := strings.SplitN(command, " ", 3)
	if len(fields) < 2 {
		return []byte("ok"), nil
	}
	resolve := func(value string) string {
		switch {
		case strings.HasPrefix(value, "packages/"):
			return filepath.Join(view.packages, filepath.FromSlash(strings.TrimPrefix(value, "packages/")))
		case strings.HasPrefix(value, "home/"):
			return filepath.Join(view.start.RootFS, "root", filepath.FromSlash(strings.TrimPrefix(value, "home/")))
		case strings.HasPrefix(value, "system/"):
			return filepath.Join(view.start.RootFS, filepath.FromSlash(strings.TrimPrefix(value, "system/")))
		default:
			return ""
		}
	}
	path := resolve(fields[1])
	if path == "" {
		return nil, errors.New("invalid fake path")
	}
	switch fields[0] {
	case "write":
		if len(fields) != 3 {
			return nil, errors.New("write requires a value")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		return []byte("written"), os.WriteFile(path, []byte(fields[2]), 0o600)
	case "read":
		return os.ReadFile(path)
	case "delete":
		return []byte("deleted"), os.RemoveAll(path)
	case "rename":
		if len(fields) != 3 {
			return nil, errors.New("rename requires a destination")
		}
		destination := resolve(fields[2])
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return nil, err
		}
		return []byte("renamed"), os.Rename(path, destination)
	default:
		return []byte("ok"), nil
	}
}

func (d *fakeDriver) ExecStream(ctx context.Context, id, command string, input io.Reader, output io.Writer) error {
	return d.ExecCommand(ctx, id, []string{"/bin/bash", "-lc", command}, input, output)
}

func (d *fakeDriver) ExecCommand(ctx context.Context, id string, arguments []string, input io.Reader, output io.Writer) error {
	d.mu.Lock()
	d.execs++
	view := d.views[id]
	if view == nil || !view.running || view.paused {
		d.mu.Unlock()
		return errors.New("sandbox is not available")
	}
	packages := view.packages
	sharedPackages := view.start.Packages
	temporary := view.temporary
	d.mu.Unlock()
	for _, packageID := range packageDirectories(sharedPackages) {
		shared := filepath.Join(sharedPackages, filepath.FromSlash(packageID))
		private := filepath.Join(packages, filepath.FromSlash(packageID))
		head, err := gitOutput(shared, "rev-parse", "HEAD")
		if err == nil {
			_, _ = gitCommand(ctx, private, nil, "fetch", "--no-tags", shared, head)
		}
	}
	if len(arguments) == 0 {
		return errors.New("fake sandbox command requires an executable")
	}
	arguments = append([]string(nil), arguments...)
	for index := range arguments {
		arguments[index] = strings.ReplaceAll(arguments[index], "/workspace/packages", packages)
		arguments[index] = strings.ReplaceAll(arguments[index], sandboxActivationIndexRoot, filepath.Join(temporary, "activation-index"))
	}
	process := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
	diagnostics := &boundedBuffer{limit: commandOutputLimit}
	process.Stdin, process.Stdout, process.Stderr = input, output, diagnostics
	if err := process.Run(); err != nil {
		return fmt.Errorf("fake sandbox exec: %w: %s", err, diagnostics.String())
	}
	return nil
}

func (d *fakeDriver) Pause(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.views[id] == nil {
		return os.ErrNotExist
	}
	d.views[id].paused = true
	return nil
}

func (d *fakeDriver) Resume(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.views[id] == nil {
		return os.ErrNotExist
	}
	d.views[id].paused = false
	return nil
}

func (d *fakeDriver) Stop(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.views[id] != nil {
		d.views[id].running = false
	}
	return nil
}

func (d *fakeDriver) Kill(ctx context.Context, id string) error { return d.Stop(ctx, id) }

func (d *fakeDriver) Delete(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if view := d.views[id]; view != nil && view.packages != "" {
		_ = os.RemoveAll(view.packages)
		_ = os.RemoveAll(view.temporary)
	}
	delete(d.views, id)
	return nil
}

func (d *fakeDriver) Running(_ context.Context, id string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.views[id] != nil && d.views[id].running, nil
}

type testPlatform struct {
	root    string
	users   string
	image   string
	record  string
	manager *Manager
	driver  *fakeDriver
}

func newTestPlatform(t *testing.T) testPlatform {
	t.Helper()
	root := t.TempDir()
	packages := filepath.Join(root, "packages")
	users := filepath.Join(root, "users")
	runtimeRoot := filepath.Join(root, "node", "kernel", "runtime", "development")
	image := filepath.Join(root, "node", "images", "development", "rootfs")
	record := filepath.Join(root, "node", "images", "development", "image.json")
	for _, directory := range []string{packages, users, runtimeRoot, image, filepath.Dir(record), filepath.Join(root, "scripts")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(root, "scripts", "activate"), "#!/bin/sh\n")
	writeTestFile(t, filepath.Join(image, "usr", "bin", "base-tool"), "image-default\n")
	for _, id := range []string{"the8020/dev-core", "the8020/demo"} {
		packageRoot := filepath.Join(packages, filepath.FromSlash(id))
		writeTestFile(t, filepath.Join(packageRoot, "package.toml"), "schema = 1\n")
		writeTestFile(t, filepath.Join(packageRoot, "src", "message.ts"), "export const message = \"shared\";\n")
		writeTestFile(t, filepath.Join(packageRoot, "notes.txt"), id+" notes\n")
	}
	if err := writeAtomic(record, []byte(`{"image_digest":"sha256:`+strings.Repeat("1", 64)+`","deno_version":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := newFakeDriver()
	registry := core.NewRegistry(nil)
	manager, err := New(Config{Root: root, PackagesRoot: packages, UsersRoot: users, RuntimeRoot: runtimeRoot, ImageRoot: image, ImageRecord: record, Driver: driver, ActivationGateway: NewCommandBusGateway(registry)})
	if err != nil {
		t.Fatal(err)
	}
	registerTestActivationCommands(t, registry, manager)
	for _, id := range []string{"the8020/dev-core", "the8020/demo"} {
		initializeTestRepository(t, manager, id, "Test Developer", "developer@example.test", "Initial package")
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	return testPlatform{root: root, users: users, image: image, record: record, manager: manager, driver: driver}
}

func registerTestActivationCommands(t *testing.T, registry *core.Registry, manager *Manager) {
	t.Helper()
	commands := testActivationCommands()
	decode := func(request core.Request) ActivationOptions {
		option := func(name string) string {
			value, _ := request.Arguments[name].(string)
			return value
		}
		options := ActivationOptions{Description: option("message"), AuthorName: option("author_name"), AuthorEmail: option("author_email")}
		if selected := option("packages"); selected != "" {
			options.SelectedPackages = strings.Split(selected, ",")
		}
		_ = json.Unmarshal([]byte(option("package_messages")), &options.PackageMessages)
		_ = json.Unmarshal([]byte(option("metadata")), &options.Metadata)
		return options
	}
	if err := registry.Register(commands[0], func(ctx context.Context, request core.Request) (core.Result, error) {
		result, err := manager.Preview(ctx, request.Arguments["user_id"].(string), decode(request))
		return core.Result{"preview": result}, err
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(commands[1], func(ctx context.Context, request core.Request) (core.Result, error) {
		result, err := manager.Activate(ctx, request.Arguments["user_id"].(string), decode(request))
		return core.Result{"activation": result}, err
	}); err != nil {
		t.Fatal(err)
	}
}

func testActivationCommands() []core.Command {
	parameters := []core.Parameter{
		{Name: "user_id", Type: "string", Position: 0, Required: true},
		{Name: "message", Type: "string", Option: "message"},
		{Name: "packages", Type: "string", Option: "packages"},
		{Name: "package_messages", Type: "string", Option: "package-messages"},
		{Name: "author_name", Type: "string", Option: "author-name"},
		{Name: "author_email", Type: "string", Option: "author-email"},
		{Name: "metadata", Type: "string", Option: "metadata"},
	}
	runParameters := append([]core.Parameter(nil), parameters...)
	runParameters[1].Required = true
	return []core.Command{
		{Version: 1, ID: "development.activate.preview", Path: []string{"development", "activate", "preview"}, Summary: "preview", Description: "preview", Parameters: parameters},
		{Version: 1, ID: "development.activate.run", Path: []string{"development", "activate", "run"}, Summary: "activate", Description: "activate", Parameters: runParameters},
	}
}

func initializeTestRepository(t *testing.T, manager *Manager, id, authorName, authorEmail, message string) {
	t.Helper()
	manager.repositoryMu.Lock()
	defer manager.repositoryMu.Unlock()
	path, err := manager.packageRoot(id)
	if err != nil {
		t.Fatal(err)
	}
	operations := []struct {
		arguments   []string
		environment []string
	}{
		{arguments: []string{"init", "-q", "-b", "main"}},
		{arguments: []string{"add", "-A"}},
		{arguments: []string{"commit", "-q", "--no-gpg-sign", "-m", message}, environment: gitIdentity(authorName, authorEmail)},
	}
	for _, operation := range operations {
		if output, err := gitCommand(context.Background(), path, operation.environment, operation.arguments...); err != nil {
			t.Fatalf("initialize test repository %s: %v: %s", id, err, output)
		}
	}
}

func writeTestFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func shell(t *testing.T, manager *Manager, userID, command string) string {
	t.Helper()
	result, err := manager.Shell(context.Background(), userID, command)
	if err != nil {
		t.Fatalf("development sandbox command %q: %v", command, err)
	}
	return result.Output
}

func packageResult(result ActivationResult, id string) ActivationPackageResult {
	for _, item := range result.Packages {
		if item.PackageID == id {
			return item
		}
	}
	return ActivationPackageResult{}
}

func TestEnsureSandboxCreatesAndRestartsDirectly(t *testing.T) {
	platform := newTestPlatform(t)
	first, err := platform.manager.EnsureSandbox(context.Background(), "sshuser")
	if err != nil {
		t.Fatal(err)
	}
	if first != "dev-sshuser" || platform.driver.starts != 1 {
		t.Fatalf("first default sandbox = %q, starts=%d", first, platform.driver.starts)
	}
	second, err := platform.manager.EnsureSandbox(context.Background(), "sshuser")
	if err != nil || second != first || platform.driver.starts != 1 {
		t.Fatalf("reused default sandbox = %q, starts=%d, err=%v", second, platform.driver.starts, err)
	}
	sandbox, err := platform.manager.Inspect("sshuser")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := platform.manager.Stop(context.Background(), sandbox.UserID); err != nil {
		t.Fatal(err)
	}
	restarted, err := platform.manager.EnsureSandbox(context.Background(), "sshuser")
	if err != nil || restarted != first || platform.driver.starts != 2 {
		t.Fatalf("restarted default sandbox = %q, starts=%d, err=%v", restarted, platform.driver.starts, err)
	}
	for owner, wanted := range map[string]string{"alice": "dev-alice", "teamuser": "dev-teamuser"} {
		if got, err := sandboxIDForUser(owner); err != nil || got != wanted {
			t.Errorf("development sandbox ID for %q = %q, want %q", owner, got, wanted)
		}
	}
	for _, owner := range []string{"ab", "Alice", "alice@example", strings.Repeat("a", 33), "Unicode User/管理"} {
		if _, err := platform.manager.EnsureSandbox(context.Background(), owner); err == nil {
			t.Errorf("unsupported development username %q was accepted", owner)
		}
	}
}

func TestSystemRootSurvivesImageUpdateUntilFactoryReset(t *testing.T) {
	platform := newTestPlatform(t)
	sandbox, err := platform.manager.Create(context.Background(), "developer")
	if err != nil {
		t.Fatal(err)
	}
	originalDigest, originalRoot := sandbox.DevelopmentImage, sandbox.SystemPath
	writeTestFile(t, filepath.Join(originalRoot, "usr", "local", "bin", "installed-tool"), "installed\n")
	authorized := []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey developer@test\n")
	writeTestFile(t, filepath.Join(originalRoot, "root", ".ssh", "authorized_keys"), string(authorized))
	if _, err := platform.manager.Stop(context.Background(), sandbox.UserID); err != nil {
		t.Fatal(err)
	}

	latestDigest := "sha256:" + strings.Repeat("2", 64)
	writeTestFile(t, filepath.Join(platform.image, "usr", "bin", "base-tool"), "latest-image\n")
	if err := writeAtomic(platform.record, []byte(`{"image_digest":"`+latestDigest+`","deno_version":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	platform.manager.config.MountProfile = append(platform.manager.config.MountProfile, MountDefinition{
		ID: "cache", Source: "users/<user-id>/dev-sandbox/persistent/cache", Target: "/workspace/cache", Behavior: MountPersistent, Writable: true,
	})
	systemEntriesBefore, err := os.ReadDir(filepath.Join(platform.users, sandbox.UserID, "dev-sandbox", "system"))
	if err != nil {
		t.Fatal(err)
	}
	starts := platform.driver.starts
	restarted, err := platform.manager.Start(context.Background(), sandbox.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.DevelopmentImage != originalDigest || restarted.SystemPath != originalRoot {
		t.Fatalf("image update changed recorded root: digest=%q path=%q", restarted.DevelopmentImage, restarted.SystemPath)
	}
	if platform.driver.starts != starts+1 || platform.driver.views[sandbox.SandboxID].start.RootFS != originalRoot {
		t.Fatal("restart did not mount the retained system root exactly once")
	}
	if got := shell(t, platform.manager, sandbox.UserID, "read system/usr/local/bin/installed-tool"); got != "installed\n" {
		t.Fatalf("retained installed tool = %q", got)
	}
	if got := shell(t, platform.manager, sandbox.UserID, "read system/usr/bin/base-tool"); got != "image-default\n" {
		t.Fatalf("existing root was replaced with latest image contents: %q", got)
	}
	if keys, err := platform.manager.AuthorizedKeys(sandbox.UserID); err != nil || !bytes.Equal(keys, authorized) {
		t.Fatalf("authorized keys did not resolve through retained root: %q, %v", keys, err)
	}
	resolvedCache := false
	for _, mount := range platform.driver.views[sandbox.SandboxID].start.Mounts {
		if mount.ID == "cache" && mount.HostSource == filepath.Join(platform.users, sandbox.UserID, "dev-sandbox", "persistent", "cache") {
			resolvedCache = true
		}
	}
	if !resolvedCache {
		t.Fatal("restart did not resolve the current mount profile")
	}
	systemEntriesAfter, err := os.ReadDir(filepath.Join(platform.users, sandbox.UserID, "dev-sandbox", "system"))
	if err != nil || len(systemEntriesAfter) != len(systemEntriesBefore) {
		t.Fatalf("image update created another system root: before=%d after=%d err=%v", len(systemEntriesBefore), len(systemEntriesAfter), err)
	}
	latestRoot, err := platform.manager.systemRootPath(sandbox.UserID, latestDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(latestRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("existing sandbox received an implicit latest-image root: %v", err)
	}

	sourceReset, err := platform.manager.ResetSource(context.Background(), sandbox.UserID, true)
	if err != nil || sourceReset.SystemPath != originalRoot || sourceReset.DevelopmentImage != originalDigest {
		t.Fatalf("source reset changed retained root: %#v, %v", sourceReset, err)
	}
	if keys, err := platform.manager.AuthorizedKeys(sandbox.UserID); err != nil || !bytes.Equal(keys, authorized) {
		t.Fatalf("source reset changed retained authorized keys: %q, %v", keys, err)
	}

	newSandbox, err := platform.manager.Create(context.Background(), "newdeveloper")
	if err != nil {
		t.Fatal(err)
	}
	if newSandbox.DevelopmentImage != latestDigest {
		t.Fatalf("new sandbox image = %q, want %q", newSandbox.DevelopmentImage, latestDigest)
	}
	if got := shell(t, platform.manager, newSandbox.UserID, "read system/usr/bin/base-tool"); got != "latest-image\n" {
		t.Fatalf("new sandbox image contents = %q", got)
	}

	factoryReset, err := platform.manager.FactoryReset(context.Background(), sandbox.UserID, true)
	if err != nil {
		t.Fatal(err)
	}
	if factoryReset.DevelopmentImage != latestDigest || factoryReset.SystemPath == originalRoot {
		t.Fatalf("factory reset did not select latest image root: %#v", factoryReset)
	}
	if _, err := os.Stat(originalRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("factory reset retained old system root: %v", err)
	}
	if got := shell(t, platform.manager, sandbox.UserID, "read system/usr/bin/base-tool"); got != "latest-image\n" {
		t.Fatalf("factory-reset image contents = %q", got)
	}
}

func TestExistingSystemRootRecordFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, testPlatform, *Sandbox)
	}{
		{
			name: "missing root",
			mutate: func(t *testing.T, _ testPlatform, sandbox *Sandbox) {
				if err := os.RemoveAll(sandbox.SystemPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "inconsistent path",
			mutate: func(t *testing.T, platform testPlatform, sandbox *Sandbox) {
				outside := filepath.Join(platform.root, "outside-root")
				if err := os.MkdirAll(outside, 0o700); err != nil {
					t.Fatal(err)
				}
				sandbox.SystemPath = outside
				if err := platform.manager.saveSandbox(*sandbox); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unsafe symlinked root",
			mutate: func(t *testing.T, platform testPlatform, sandbox *Sandbox) {
				outside := filepath.Join(platform.root, "outside-root")
				if err := os.MkdirAll(outside, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(sandbox.SystemPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, sandbox.SystemPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "incomplete provenance",
			mutate: func(t *testing.T, platform testPlatform, sandbox *Sandbox) {
				sandbox.DevelopmentImage = ""
				if err := platform.manager.saveSandbox(*sandbox); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			platform := newTestPlatform(t)
			sandbox, err := platform.manager.Create(context.Background(), "developer")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := platform.manager.Stop(context.Background(), sandbox.UserID); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, platform, &sandbox)
			latestDigest := "sha256:" + strings.Repeat("3", 64)
			if err := writeAtomic(platform.record, []byte(`{"image_digest":"`+latestDigest+`"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			starts := platform.driver.starts
			if _, err := platform.manager.Start(context.Background(), sandbox.UserID); err == nil || !strings.Contains(err.Error(), "factory reset is required") {
				t.Fatalf("invalid existing storage did not fail with recovery guidance: %v", err)
			}
			if platform.driver.starts != starts {
				t.Fatalf("failed start created a runtime sandbox: %d -> %d", starts, platform.driver.starts)
			}
			latestRoot, err := platform.manager.systemRootPath(sandbox.UserID, latestDigest)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(latestRoot); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed start implicitly initialized replacement root: %v", err)
			}
		})
	}
}

func TestAuthorizedKeysReadsExistingSandboxWithoutLifecycleMutation(t *testing.T) {
	platform := newTestPlatform(t)
	if _, err := platform.manager.AuthorizedKeys("alice"); err == nil {
		t.Fatal("authorized keys were read without an initialized sandbox")
	}
	if platform.driver.starts != 0 {
		t.Fatalf("authorized-key lookup started %d sandboxes", platform.driver.starts)
	}
	if _, err := platform.manager.EnsureSandbox(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	sandbox, err := platform.manager.Inspect("alice")
	if err != nil {
		t.Fatal(err)
	}
	starts := platform.driver.starts
	if _, err := platform.manager.AuthorizedKeys("alice"); err == nil {
		t.Fatal("missing authorized-keys file was accepted")
	}
	if platform.driver.starts != starts {
		t.Fatalf("missing authorized-key lookup changed sandbox starts from %d to %d", starts, platform.driver.starts)
	}
	authorized := []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey alice@test\n")
	writeTestFile(t, filepath.Join(sandbox.SystemPath, "root", ".ssh", "authorized_keys"), string(authorized))
	content, err := platform.manager.AuthorizedKeys("alice")
	if err != nil || !bytes.Equal(content, authorized) {
		t.Fatalf("authorized keys = %q, err=%v", content, err)
	}
	if platform.driver.starts != starts {
		t.Fatalf("authorized-key lookup changed sandbox starts from %d to %d", starts, platform.driver.starts)
	}
}

func TestAuthorizedKeysRejectsUnsafeAndOversizedFiles(t *testing.T) {
	platform := newTestPlatform(t)
	if _, err := platform.manager.EnsureSandbox(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	sandbox, err := platform.manager.Inspect("alice")
	if err != nil {
		t.Fatal(err)
	}
	sshDirectory := filepath.Join(sandbox.SystemPath, "root", ".ssh")
	outsideDirectory := filepath.Join(platform.root, "outside")
	writeTestFile(t, filepath.Join(outsideDirectory, "authorized_keys"), "outside\n")
	if err := os.MkdirAll(filepath.Dir(sshDirectory), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDirectory, sshDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.manager.AuthorizedKeys("alice"); err == nil {
		t.Fatal("authorized keys escaped through a symlinked .ssh directory")
	}
	if err := os.Remove(sshDirectory); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sshDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(sshDirectory, "authorized_keys")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() {
		_, err := platform.manager.AuthorizedKeys("alice")
		read <- err
	}()
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("FIFO authorized keys were accepted")
		}
	case <-time.After(time.Second):
		writer, _ := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if writer != nil {
			_ = writer.Close()
		}
		<-read
		t.Fatal("FIFO authorized keys blocked authentication")
	}
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(sshDirectory, "authorized_keys"), strings.Repeat("x", authorizedKeysLimit+1))
	if _, err := platform.manager.AuthorizedKeys("alice"); err == nil {
		t.Fatal("oversized authorized keys were accepted")
	}
	sandbox.SystemPath = outsideDirectory
	if err := platform.manager.saveSandbox(sandbox); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.manager.AuthorizedKeys("alice"); err == nil {
		t.Fatal("malformed recorded system root was accepted")
	}
}

func TestSandboxPersistenceUsesOneDevSandboxDirectory(t *testing.T) {
	platform := newTestPlatform(t)
	sandbox, err := platform.manager.Create(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	shell(t, platform.manager, sandbox.UserID, "write packages/the8020/dev-core/src/message.ts private")
	if _, err := platform.manager.Stop(context.Background(), sandbox.UserID); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(platform.users, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "dev-sandbox" || !entries[0].IsDir() {
		t.Fatalf("user sandbox storage = %#v, want only dev-sandbox", entries)
	}
	root := filepath.Join(platform.users, "alice", "dev-sandbox")
	for _, relative := range []string{"sandbox.toml", filepath.Join("runtime", "overlay", "state.toml"), "system"} {
		if _, err := os.Stat(filepath.Join(root, relative)); err != nil {
			t.Errorf("missing persisted sandbox path %s: %v", relative, err)
		}
	}
}

func TestSandboxOverlayUsesSharedLowerAndPersistsCheckpoints(t *testing.T) {
	platform := newTestPlatform(t)
	a, err := platform.manager.Create(context.Background(), "developera")
	if err != nil {
		t.Fatal(err)
	}
	b, err := platform.manager.Create(context.Background(), "developerb")
	if err != nil {
		t.Fatal(err)
	}
	if a.SourcePath != b.SourcePath || a.SourcePath != filepath.Join(platform.root, "packages") {
		t.Fatalf("sandbox overlay lowers are not the shared package root: %q %q", a.SourcePath, b.SourcePath)
	}
	if !strings.Contains(a.SystemPath, filepath.Join("users", "developera", "dev-sandbox", "system")) {
		t.Fatalf("system path is not user-owned durable storage: %q", a.SystemPath)
	}
	shell(t, platform.manager, a.UserID, "write packages/the8020/dev-core/src/message.ts private-a")
	shell(t, platform.manager, a.UserID, "write home/.config/editor/config.toml model=test")
	shell(t, platform.manager, a.UserID, "write system/usr/local/bin/private-tool tool")
	if got := shell(t, platform.manager, b.UserID, "read packages/the8020/dev-core/src/message.ts"); strings.Contains(got, "private-a") {
		t.Fatal("sandbox B observed sandbox A's source")
	}
	shared, _ := os.ReadFile(filepath.Join(platform.root, "packages", "the8020", "dev-core", "src", "message.ts"))
	if strings.Contains(string(shared), "private-a") {
		t.Fatal("private edit changed shared packages")
	}
	execs := platform.driver.execs
	time.Sleep(1200 * time.Millisecond)
	if platform.driver.execs != execs {
		t.Fatalf("idle sandbox performed background work: %d -> %d", execs, platform.driver.execs)
	}
	starts := platform.driver.starts
	if _, err := platform.manager.Stop(context.Background(), a.UserID); err != nil {
		t.Fatal(err)
	}
	a, err = platform.manager.Start(context.Background(), a.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if platform.driver.starts != starts+1 {
		t.Fatal("restart did not create exactly one sandbox process")
	}
	for _, proof := range []struct{ command, want string }{
		{"read packages/the8020/dev-core/src/message.ts", "private-a"},
		{"read home/.config/editor/config.toml", "model=test"},
		{"read system/usr/local/bin/private-tool", "tool"},
	} {
		if got := shell(t, platform.manager, a.UserID, proof.command); got != proof.want {
			t.Fatalf("%s = %q, want %q", proof.command, got, proof.want)
		}
	}
}

func TestActivationScansOnlyOnDemandCommitsAndResetsOverlay(t *testing.T) {
	platform := newTestPlatform(t)
	hook := &recordingActivationSchemaHook{}
	platform.manager.SetSchemaDeployment(hook)
	sandbox, err := platform.manager.Create(context.Background(), "developer")
	if err != nil {
		t.Fatal(err)
	}
	shell(t, platform.manager, sandbox.UserID, "write packages/the8020/dev-core/src/message.ts private-a")
	shell(t, platform.manager, sandbox.UserID, "write packages/the8020/demo/notes.txt private-b")
	execs := platform.driver.execs
	preview, err := platform.manager.Preview(context.Background(), sandbox.UserID, ActivationOptions{SelectedPackages: []string{"the8020/dev-core"}})
	if err != nil || len(preview.Packages) != 2 {
		t.Fatalf("preview = %#v, %v", preview, err)
	}
	if platform.driver.execs != execs+1 {
		t.Fatalf("preview used %d sandbox commands, want one batched scan", platform.driver.execs-execs)
	}
	starts := platform.driver.starts
	oldSandbox := sandbox.SandboxID
	result, err := platform.manager.Activate(context.Background(), sandbox.UserID, ActivationOptions{Description: "Common", SelectedPackages: []string{"the8020/dev-core"}, AuthorName: "Developer", AuthorEmail: "developer@example.test", Metadata: map[string]string{"client": "unit-test"}})
	if err != nil || !result.Success || packageResult(result, "the8020/dev-core").Status != "committed" {
		t.Fatalf("activation = %#v, %v", result, err)
	}
	if len(hook.prepared) != 1 || hook.prepared[0].PackageID != "the8020/dev-core" || !slices.Equal(hook.completed, []bool{true}) {
		t.Fatalf("schema activation hook = prepared %#v completed %#v", hook.prepared, hook.completed)
	}
	if !beneath(hook.prepared[0].Root, platform.root) || beneath(hook.prepared[0].Root, filepath.Join(platform.root, "node", "kernel")) {
		t.Fatalf("schema candidate uses protected mount source %q", hook.prepared[0].Root)
	}
	current, _ := platform.manager.Inspect(sandbox.UserID)
	if platform.driver.starts != starts+1 || current.SandboxID != oldSandbox || !result.OverlayReset {
		t.Fatal("activation did not recreate the deterministic sandbox with a clean overlay")
	}
	if got, _ := os.ReadFile(filepath.Join(platform.root, "packages", "the8020", "dev-core", "src", "message.ts")); string(got) != "private-a" {
		t.Fatalf("activated shared source = %q", got)
	}
	commitMessage, err := gitOutput(filepath.Join(platform.root, "packages", "the8020", "dev-core"), "log", "-1", "--pretty=%B")
	metadataAt := strings.Index(commitMessage, "[the8020.activation]")
	if err != nil || metadataAt < 0 {
		t.Fatalf("activation commit metadata = %q, %v", commitMessage, err)
	}
	metadataFile := filepath.Join(t.TempDir(), "activation.toml")
	writeTestFile(t, metadataFile, commitMessage[metadataAt:])
	var metadataDocument struct {
		The8020 struct {
			Activation map[string]string `toml:"activation"`
		} `toml:"the8020"`
	}
	if err := readTOML(metadataFile, &metadataDocument); err != nil || metadataDocument.The8020.Activation["sandbox"] != sandbox.SandboxID || metadataDocument.The8020.Activation["metadata_client"] != "unit-test" {
		t.Fatalf("activation metadata TOML = %#v, %v", metadataDocument, err)
	}
	remaining, err := platform.manager.Preview(context.Background(), sandbox.UserID, ActivationOptions{})
	if err != nil || len(remaining.Packages) != 1 || remaining.Packages[0].PackageID != "the8020/demo" {
		t.Fatalf("remaining changes = %#v, %v", remaining, err)
	}
	result, err = platform.manager.Activate(context.Background(), sandbox.UserID, ActivationOptions{Description: "Fallback", PackageMessages: map[string]string{"the8020/demo": "Override"}})
	if err != nil || !result.Success {
		t.Fatalf("second activation = %#v, %v", result, err)
	}
	message, _ := gitOutput(filepath.Join(platform.root, "packages", "the8020", "demo"), "log", "-1", "--pretty=%s")
	if message != "Override" {
		t.Fatalf("package message = %q", message)
	}
	author, _ := gitOutput(filepath.Join(platform.root, "packages", "the8020", "demo"), "log", "-1", "--pretty=%an <%ae>")
	if author != "developer <developer@development.local>" {
		t.Fatalf("default activation author = %q", author)
	}
}

func TestActivationCapturesRenamesDeletionsAndBinaryButExcludesIgnoredFiles(t *testing.T) {
	platform := newTestPlatform(t)
	shared := filepath.Join(platform.root, "packages", "the8020", "dev-core")
	writeTestFile(t, filepath.Join(shared, ".gitignore"), "ignored.dat\n")
	if output, err := gitCommand(context.Background(), shared, gitIdentity("Test Developer", "developer@example.test"), "add", ".gitignore"); err != nil {
		t.Fatalf("stage ignore file: %v: %s", err, output)
	}
	if output, err := gitCommand(context.Background(), shared, gitIdentity("Test Developer", "developer@example.test"), "commit", "-q", "--no-gpg-sign", "-m", "Ignore generated data"); err != nil {
		t.Fatalf("commit ignore file: %v: %s", err, output)
	}
	sandbox, err := platform.manager.Create(context.Background(), "developer")
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(platform.driver.views[sandbox.SandboxID].packages, "the8020", "dev-core")
	if err := os.Rename(filepath.Join(private, "notes.txt"), filepath.Join(private, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(private, "src", "message.ts")); err != nil {
		t.Fatal(err)
	}
	binary := []byte{0, 1, 2, 3, 0xff}
	if err := os.WriteFile(filepath.Join(private, "binary.dat"), binary, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "ignored.dat"), []byte("generated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unusualPath := "line\nand\ttab.txt"
	if err := os.WriteFile(filepath.Join(private, unusualPath), []byte("unusual\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	execs := platform.driver.execs
	preview, err := platform.manager.Preview(context.Background(), sandbox.UserID, ActivationOptions{})
	if err != nil || len(preview.Packages) != 1 {
		t.Fatalf("preview = %#v, %v", preview, err)
	}
	if platform.driver.execs != execs+1 {
		t.Fatalf("preview used %d sandbox commands, want one batched scan", platform.driver.execs-execs)
	}
	item := preview.Packages[0]
	if item.PackageID != "the8020/dev-core" || item.ChangedFiles != 4 || item.AddedRows != 1 || item.RemovedRows != 1 {
		t.Fatalf("package summary = %#v", item)
	}
	files := map[string]string{}
	for _, file := range item.Files {
		files[file.Path] = file.Change
	}
	wantFiles := map[string]string{
		"binary.dat":     "new",
		unusualPath:      "new",
		"renamed.txt":    "renamed from notes.txt",
		"src/message.ts": "deleted",
	}
	if !maps.Equal(files, wantFiles) {
		t.Fatalf("files = %#v, want %#v", files, wantFiles)
	}

	index, _ := sandboxIndexPaths("the8020/dev-core")
	index = strings.Replace(index, sandboxActivationIndexRoot, filepath.Join(platform.driver.views[sandbox.SandboxID].temporary, "activation-index"), 1)
	if err := os.WriteFile(index, []byte("corrupt index"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.manager.Preview(context.Background(), sandbox.UserID, ActivationOptions{}); err != nil {
		t.Fatalf("preview did not rebuild a disposable corrupt index: %v", err)
	}

	result, err := platform.manager.Activate(context.Background(), sandbox.UserID, ActivationOptions{Description: "Capture every Git change type"})
	if err != nil || !result.Success {
		t.Fatalf("activation = %#v, %v", result, err)
	}
	if contents, err := os.ReadFile(filepath.Join(shared, "binary.dat")); err != nil || !bytes.Equal(contents, binary) {
		t.Fatalf("activated binary = %v, %v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(shared, "ignored.dat")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("activation published ignored artifact: %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(shared, "renamed.txt")); err != nil || string(contents) != "the8020/dev-core notes\n" {
		t.Fatalf("activated rename = %q, %v", contents, err)
	}
	if contents, err := os.ReadFile(filepath.Join(shared, unusualPath)); err != nil || string(contents) != "unusual\n" {
		t.Fatalf("activated unusual path = %q, %v", contents, err)
	}
	for _, path := range []string{"notes.txt", filepath.Join("src", "message.ts")} {
		if _, err := os.Stat(filepath.Join(shared, path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("activated deletion retained %s: %v", path, err)
		}
	}
}

func TestActivationWarmIndexDropsNewlyIgnoredArtifact(t *testing.T) {
	platform := newTestPlatform(t)
	sandbox, err := platform.manager.Create(context.Background(), "developer")
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(platform.driver.views[sandbox.SandboxID].packages, "the8020", "dev-core")
	writeTestFile(t, filepath.Join(private, "generated.dat"), "generated\n")
	preview, err := platform.manager.Preview(context.Background(), sandbox.UserID, ActivationOptions{})
	if err != nil || len(preview.Packages) != 1 || preview.Packages[0].ChangedFiles != 1 || preview.Packages[0].Files[0].Path != "generated.dat" {
		t.Fatalf("preview before ignore = %#v, %v", preview, err)
	}
	writeTestFile(t, filepath.Join(private, ".gitignore"), "generated.dat\n")
	preview, err = platform.manager.Preview(context.Background(), sandbox.UserID, ActivationOptions{})
	if err != nil || len(preview.Packages) != 1 || preview.Packages[0].ChangedFiles != 1 || preview.Packages[0].Files[0].Path != ".gitignore" {
		t.Fatalf("preview after ignore = %#v, %v", preview, err)
	}
	result, err := platform.manager.Activate(context.Background(), sandbox.UserID, ActivationOptions{Description: "Ignore generated artifacts"})
	if err != nil || !result.Success {
		t.Fatalf("activation = %#v, %v", result, err)
	}
	shared := filepath.Join(platform.root, "packages", "the8020", "dev-core")
	if contents, err := os.ReadFile(filepath.Join(shared, ".gitignore")); err != nil || string(contents) != "generated.dat\n" {
		t.Fatalf("activated ignore rules = %q, %v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(shared, "generated.dat")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("activation published newly ignored artifact: %v", err)
	}
}

func TestActivationStillCapturesTrackedFileMatchedByIgnoreRule(t *testing.T) {
	platform := newTestPlatform(t)
	shared := filepath.Join(platform.root, "packages", "the8020", "dev-core")
	writeTestFile(t, filepath.Join(shared, ".gitignore"), "tracked.dat\n")
	writeTestFile(t, filepath.Join(shared, "tracked.dat"), "shared\n")
	if output, err := gitCommand(context.Background(), shared, nil, "add", ".gitignore"); err != nil {
		t.Fatalf("stage ignore rule: %v: %s", err, output)
	}
	if output, err := gitCommand(context.Background(), shared, nil, "add", "-f", "tracked.dat"); err != nil {
		t.Fatalf("stage tracked ignored file: %v: %s", err, output)
	}
	if output, err := gitCommand(context.Background(), shared, gitIdentity("Test Developer", "developer@example.test"), "commit", "-q", "--no-gpg-sign", "-m", "Track ignored file"); err != nil {
		t.Fatalf("commit tracked ignored file: %v: %s", err, output)
	}
	sandbox, err := platform.manager.Create(context.Background(), "developer")
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(platform.driver.views[sandbox.SandboxID].packages, "the8020", "dev-core")
	writeTestFile(t, filepath.Join(private, "tracked.dat"), "private\n")
	preview, err := platform.manager.Preview(context.Background(), sandbox.UserID, ActivationOptions{})
	if err != nil || len(preview.Packages) != 1 || preview.Packages[0].ChangedFiles != 1 || preview.Packages[0].Files[0].Path != "tracked.dat" {
		t.Fatalf("tracked ignored preview = %#v, %v", preview, err)
	}
	result, err := platform.manager.Activate(context.Background(), sandbox.UserID, ActivationOptions{Description: "Update tracked ignored file"})
	if err != nil || !result.Success {
		t.Fatalf("activation = %#v, %v", result, err)
	}
	if contents, err := os.ReadFile(filepath.Join(shared, "tracked.dat")); err != nil || string(contents) != "private\n" {
		t.Fatalf("activated tracked ignored file = %q, %v", contents, err)
	}
}

func TestActivationCaptureScanWriterAcceptsOnlyChangedMarkers(t *testing.T) {
	packages := []sandboxPackageScan{{PackageID: "the8020/a", Base: strings.Repeat("a", 40)}, {PackageID: "the8020/b", Base: strings.Repeat("b", 40)}}
	writer := &activationScanWriter{packages: packages, changes: []packageChanges{}}
	if _, err := writer.Write([]byte("changed\x00\x00\x00")); err != nil {
		t.Fatal(err)
	}
	if err := writer.finish(); err != nil || len(writer.changes) != 1 || writer.changes[0].PackageID != "the8020/a" || len(writer.changes[0].Files) != 0 {
		t.Fatalf("capture markers = %#v, %v", writer.changes, err)
	}
	malformed := &activationScanWriter{packages: packages[:1], changes: []packageChanges{}}
	_, _ = malformed.Write([]byte("unexpected\x00\x00"))
	if err := malformed.finish(); err == nil {
		t.Fatal("malformed capture marker was accepted")
	}
}

func TestParseRawNumstatRejectsMalformedRecords(t *testing.T) {
	for _, value := range [][]byte{
		[]byte("not terminated"),
		[]byte(":100644 100644 abc def M\x00"),
		[]byte(":100644 100644 abc def M\x00file.ts\x00bad numstat\x00"),
	} {
		if _, _, _, err := parseRawNumstat(value); err == nil {
			t.Fatalf("malformed Git output was accepted: %q", value)
		}
	}
}

func TestActivationRebasesPrivateOverlayOnCurrentSharedSource(t *testing.T) {
	platform := newTestPlatform(t)
	a, _ := platform.manager.Create(context.Background(), "developera")
	b, _ := platform.manager.Create(context.Background(), "developerb")
	shell(t, platform.manager, b.UserID, "write packages/the8020/dev-core/src/message.ts private-b")
	shell(t, platform.manager, a.UserID, "write packages/the8020/dev-core/src/message.ts private-a")
	if _, err := platform.manager.Activate(context.Background(), a.UserID, ActivationOptions{Description: "Advance A"}); err != nil {
		t.Fatal(err)
	}
	result, err := platform.manager.Activate(context.Background(), b.UserID, ActivationOptions{Description: "Conflict B"})
	if err != nil || !result.Success {
		t.Fatalf("rebased activation = %#v, %v", result, err)
	}
	contents, _ := os.ReadFile(filepath.Join(platform.root, "packages", "the8020", "dev-core", "src", "message.ts"))
	if string(contents) != "private-b" {
		t.Fatalf("second overlay activation = %q", contents)
	}
	preview, err := platform.manager.Preview(context.Background(), b.UserID, ActivationOptions{})
	if err != nil || len(preview.Packages) != 0 {
		t.Fatalf("second overlay was not reset: %#v, %v", preview, err)
	}
	encoded, err := json.Marshal(preview)
	if err != nil || !strings.Contains(string(encoded), `"packages":[]`) {
		t.Fatalf("empty activation preview JSON = %s, %v", encoded, err)
	}
}

func TestActivationPreviewReportsChangesBlockedByDirtySharedRepository(t *testing.T) {
	platform := newTestPlatform(t)
	sandbox, err := platform.manager.Create(context.Background(), "developer")
	if err != nil {
		t.Fatal(err)
	}
	shell(t, platform.manager, sandbox.UserID, "write packages/the8020/dev-core/src/message.ts private")
	writeTestFile(t, filepath.Join(platform.root, "packages", "the8020", "dev-core", "host-only.txt"), "dirty shared worktree\n")

	preview, err := platform.manager.Preview(context.Background(), sandbox.UserID, ActivationOptions{})
	if err != nil || len(preview.Packages) != 1 {
		t.Fatalf("blocked preview = %#v, %v", preview, err)
	}
	if item := preview.Packages[0]; item.PackageID != "the8020/dev-core" || item.ActivationReady || item.ChangedFiles != 1 {
		t.Fatalf("blocked package preview = %#v", item)
	}
	result, err := platform.manager.Activate(context.Background(), sandbox.UserID, ActivationOptions{Description: "Must remain private"})
	if err == nil || result.Success || packageResult(result, "the8020/dev-core").Status != "failed" {
		t.Fatalf("blocked activation = %#v, %v", result, err)
	}
}

func TestResetBoundaries(t *testing.T) {
	platform := newTestPlatform(t)
	sandbox, _ := platform.manager.Create(context.Background(), "developer")
	shell(t, platform.manager, sandbox.UserID, "write packages/the8020/dev-core/new.txt source")
	shell(t, platform.manager, sandbox.UserID, "write home/proof home")
	shell(t, platform.manager, sandbox.UserID, "write system/proof system")
	userMarker := filepath.Join(platform.users, "developer", "profile.json")
	writeTestFile(t, userMarker, "preserve\n")

	reset, err := platform.manager.ResetSource(context.Background(), sandbox.UserID, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(reset.SourcePath, "the8020", "dev-core", "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source reset retained private package changes")
	}
	for _, path := range []string{filepath.Join(reset.SystemPath, "root", "proof"), filepath.Join(reset.SystemPath, "proof")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("source reset removed durable user storage %s: %v", path, err)
		}
	}
	factory, err := platform.manager.FactoryReset(context.Background(), sandbox.UserID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(factory.SystemPath, "root", "proof"), filepath.Join(factory.SystemPath, "proof")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("factory reset retained %s", path)
		}
	}
	if contents, err := os.ReadFile(userMarker); err != nil || string(contents) != "preserve\n" {
		t.Fatalf("factory reset changed unrelated user data: %q, %v", contents, err)
	}
}

func TestFailedStartAndBoundedDiagnostics(t *testing.T) {
	platform := newTestPlatform(t)
	platform.driver.startErr = errors.New("sandbox start failed")
	sandbox, err := platform.manager.Create(context.Background(), "failedstart")
	if err == nil || sandbox.State != StateFailed {
		t.Fatalf("failed sandbox = %#v, %v", sandbox, err)
	}
	buffer := &boundedBuffer{limit: 8}
	_, _ = buffer.Write([]byte("0123456789abcdef"))
	if got := buffer.String(); got != "01234567\n[output truncated]" {
		t.Fatalf("bounded output = %q", got)
	}
}

func TestCopySystemRootCreatesPortableRuntimeMountPoints(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "image")
	destination := filepath.Join(root, "state", "rootfs")
	writeTestFile(t, filepath.Join(source, "usr", "bin", "base-tool"), "base\n")
	for _, name := range []string{"proc", "sys"} {
		path := filepath.Join(source, name)
		if err := os.Mkdir(path, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := copySystemRoot(context.Background(), source, destination); err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Stat(destination)
	if err != nil || rootInfo.Mode().Perm() != 0o755 {
		t.Fatalf("copied system root = %#v, %v", rootInfo, err)
	}
	contents, err := os.ReadFile(filepath.Join(destination, "usr", "bin", "base-tool"))
	if err != nil || string(contents) != "base\n" {
		t.Fatalf("copied system file = %q, %v", contents, err)
	}
	for _, name := range []string{"proc", "sys"} {
		info, err := os.Stat(filepath.Join(destination, name))
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o755 {
			t.Fatalf("runtime mount point %s = %#v, %v", name, info, err)
		}
	}
}

func TestInheritedCleanupNeverGatesStartup(t *testing.T) {
	root := t.TempDir()
	packages := filepath.Join(root, "packages")
	users := filepath.Join(root, "users")
	runtimeRoot := filepath.Join(root, "node", "kernel", "runtime", "development")
	image := filepath.Join(root, "node", "images", "development", "rootfs")
	record := filepath.Join(root, "node", "images", "development", "image.json")
	for _, directory := range []string{packages, users, runtimeRoot, image, filepath.Dir(record), filepath.Join(root, "scripts")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(root, "scripts", "activate"), "#!/bin/sh\n")
	writeTestFile(t, filepath.Join(image, "base"), "base")
	if err := writeAtomic(record, []byte(`{"image_digest":"sha256:test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sandbox := Sandbox{Schema: sandboxSchema, UserID: "developer", SandboxID: "dev-developer", State: StateReady, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Token: "token"}
	sandboxRoot := filepath.Join(users, "developer", "dev-sandbox")
	if err := os.MkdirAll(sandboxRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeTOML(filepath.Join(sandboxRoot, "sandbox.toml"), sandbox, 0o600); err != nil {
		t.Fatal(err)
	}
	wait := make(chan struct{})
	driver := newFakeDriver()
	driver.listWait = wait
	driver.views["dev-alice"] = &fakeView{start: SandboxStart{SandboxID: "dev-alice"}, running: true}
	started := time.Now()
	manager, err := New(Config{Root: root, PackagesRoot: packages, UsersRoot: users, RuntimeRoot: runtimeRoot, ImageRoot: image, ImageRecord: record, Driver: driver})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("development cleanup gated startup for %s", elapsed)
	}
	inspected, err := manager.Inspect(sandbox.UserID)
	if err != nil || inspected.State != StateStopped {
		t.Fatalf("stale sandbox was not normalized lazily: %#v, %v", inspected, err)
	}
	alice, err := manager.Create(context.Background(), "alice")
	if err != nil || alice.SandboxID != "dev-alice" {
		t.Fatalf("deterministic development sandbox = %#v, %v", alice, err)
	}
	close(wait)
	select {
	case <-manager.cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("inherited cleanup did not finish")
	}
	if running, err := driver.Running(context.Background(), "dev-alice"); err != nil || !running {
		t.Fatalf("inherited cleanup deleted the current deterministic sandbox: running=%v err=%v", running, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDevelopmentSpecOverlaysOnlySandboxPackages(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"rootfs", "packages", "bundle"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	start := SandboxStart{UserID: "alice", SandboxID: "dev-alice", Packages: filepath.Join(root, "packages"), RootFS: filepath.Join(root, "rootfs"), Mounts: []SandboxMount{
		{MountDefinition: MountDefinition{ID: "packages", Target: "/workspace/packages", Behavior: MountSandboxSource, Writable: true}, HostSource: filepath.Join(root, "packages")},
		{MountDefinition: MountDefinition{ID: "temporary", Target: "/tmp", Behavior: MountEphemeral, Writable: true}},
	}}
	spec := developmentSpec(start, filepath.Join(root, "bundle"))
	if spec.Root.Path != start.RootFS || spec.Root.Readonly {
		t.Fatalf("development root = %#v", spec.Root)
	}
	prefix := "dev.gvisor.spec.mount.packages."
	if spec.Annotations[prefix+"source"] != start.Packages || spec.Annotations[prefix+"type"] != "bind" || spec.Annotations[prefix+"share"] != "container" {
		t.Fatalf("development package overlay annotations = %#v", spec.Annotations)
	}
	if len(spec.Process.Args) != 2 || spec.Process.Args[1] != "/opt/development/sandbox.sh" {
		t.Fatalf("development init = %#v", spec.Process.Args)
	}
	environment := strings.Join(spec.Process.Env, "\n")
	if !strings.Contains(environment, "HOME=/root") || !strings.Contains(environment, "USER=root") || !strings.Contains(environment, "LOGNAME=root") {
		t.Fatalf("development root environment = %#v", spec.Process.Env)
	}
	runOptions := ""
	for _, mount := range spec.Mounts {
		if mount.Destination == "/run" {
			runOptions = strings.Join(mount.Options, ",")
			break
		}
	}
	if !strings.Contains(runOptions, "size=65536k") {
		t.Fatalf("development /run mount options = %q", runOptions)
	}
	driver := &RunscDriver{config: RunscConfig{RuntimeRoot: filepath.Join(root, "runtime"), SandboxRoot: filepath.Join(root, "sandboxes"), LogRoot: filepath.Join(root, "logs")}}
	flags := strings.Join(driver.flags(start.SandboxID, "run"), " ")
	if !strings.Contains(flags, "--directfs=true") || !strings.Contains(flags, "--overlay2=none") || strings.Contains(flags, "overlay2=all") || strings.Contains(flags, "overlay2=root") || strings.Contains(flags, "rootfs-tar") {
		t.Fatalf("development driver filesystem flags = %s", flags)
	}
}

func TestRootlessDevelopmentCommandsMapPackageOwnershipIDs(t *testing.T) {
	driver := &RunscDriver{config: RunscConfig{RunscPath: "/bin/true", Rootless: true}}
	command := driver.commandContext(context.Background(), "--version")
	attributes := command.SysProcAttr
	if attributes == nil || attributes.Cloneflags&syscall.CLONE_NEWUSER == 0 || attributes.Cloneflags&syscall.CLONE_NEWNS == 0 {
		t.Fatalf("rootless command namespaces = %#v", attributes)
	}
	wantMapping := syscall.SysProcIDMap{ContainerID: 0, HostID: 0, Size: rootlessIDMapSize}
	if len(attributes.UidMappings) != 1 || attributes.UidMappings[0] != wantMapping {
		t.Fatalf("rootless UID mappings = %#v", attributes.UidMappings)
	}
	if len(attributes.GidMappings) != 1 || attributes.GidMappings[0] != wantMapping {
		t.Fatalf("rootless GID mappings = %#v", attributes.GidMappings)
	}
	if attributes.Credential == nil || attributes.Credential.Uid != 0 || attributes.Credential.Gid != 0 || attributes.Pdeathsig != syscall.SIGKILL || !attributes.Setsid {
		t.Fatalf("rootless process attributes = %#v", attributes)
	}

	driver.config.Rootless = false
	if attributes := driver.commandContext(context.Background(), "--version").SysProcAttr; attributes != nil {
		t.Fatalf("rootful command unexpectedly enters a caller user namespace: %#v", attributes)
	}
}

func TestSandboxNetworkFilesAreReadableByPackageAccounts(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "resolv.conf")
	writeTestFile(t, source, "nameserver 192.0.2.1\n")
	if err := os.Chmod(source, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copySandboxNetworkFile(source, destination); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("sandbox network file = %#v, %v", info, err)
	}
	contents, err := os.ReadFile(destination)
	if err != nil || string(contents) != "nameserver 192.0.2.1\n" {
		t.Fatalf("sandbox network file contents = %q, %v", contents, err)
	}
}
