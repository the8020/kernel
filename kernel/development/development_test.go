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
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"

	"the8020/kernel/identity"
	"time"

	"the8020/kernel/deployment"

	"the8020/kernel/cbus/core"
)

type recordingActivationSchemaHook struct {
	prepared  []deployment.Candidate
	completed []bool
}

func (h *recordingActivationSchemaHook) Prepare(_ context.Context, _ string, candidates []deployment.Candidate) error {
	h.prepared = append([]deployment.Candidate(nil), candidates...)
	return nil
}

func (h *recordingActivationSchemaHook) Complete(_ context.Context, _ string, activated bool) error {
	h.completed = append(h.completed, activated)
	return nil
}

type fakeView struct {
	start     SandboxStart
	packages  string
	temporary string
	running   bool
}

type fakeDriver struct {
	mu        sync.Mutex
	views     map[string]*fakeView
	starts    int
	execs     int
	startErr  error
	deleteErr error
	listWait  <-chan struct{}
	stopWait  <-chan struct{}
	stops     chan string
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
	if view == nil || !view.running {
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
	if view == nil || !view.running {
		d.mu.Unlock()
		return errors.New("sandbox is not available")
	}
	packages := view.packages
	sharedPackages := view.start.Packages
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
	}
	process := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
	diagnostics := &boundedBuffer{limit: commandOutputLimit}
	process.Stdin, process.Stdout, process.Stderr = input, output, diagnostics
	if err := process.Run(); err != nil {
		return fmt.Errorf("fake sandbox exec: %w: %s", err, diagnostics.String())
	}
	return nil
}

func (d *fakeDriver) Stop(ctx context.Context, id string) error {
	if d.stops != nil {
		d.stops <- id
	}
	if d.stopWait != nil {
		select {
		case <-d.stopWait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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
	if d.deleteErr != nil {
		return d.deleteErr
	}
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
	installTestDevelopmentAssets(t, root)
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
	registrations := make([]core.Registration, len(commands))
	for index, command := range commands {
		registrations[index] = core.Registration{Command: command, Handler: func(ctx context.Context, request core.Request) (core.Execution, error) {
			// A package command returns through a separate runtime callback: only
			// cancellation, not Go context values, crosses that process boundary.
			callback, cancel := context.WithCancel(context.Background())
			stop := context.AfterFunc(ctx, cancel)
			defer stop()
			defer cancel()
			ctx = callback
			var err error
			request.Arguments, err = core.ParseKernelArguments(command, request.Argv)
			if err != nil {
				return core.Execution{}, err
			}
			userID := request.Arguments["user_id"].(string)
			if index == 0 {
				result, err := manager.Preview(ctx, userID, decode(request))
				return core.Execution{Result: core.Result{"preview": result}}, err
			}
			result, err := manager.Activate(ctx, userID, decode(request))
			// Match the package command: structured activation failures remain results.
			if result.Status != "" {
				err = nil
			}
			return core.Execution{Result: core.Result{"activation": result}}, err
		}}
	}
	if err := registry.ReplacePackages(registrations, nil); err != nil {
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
		{Version: 1, ID: "test-preview", Name: "dev-core.activate.preview", Kind: core.CommandKindPackage, Summary: "preview", Description: "preview", Parameters: parameters},
		{Version: 1, ID: "test-run", Name: "dev-core.activate.run", Kind: core.CommandKindPackage, Summary: "activate", Description: "activate", Parameters: runParameters},
	}
}

func TestCommandBusGatewayUsesCurrentPackageCommand(t *testing.T) {
	registry := core.NewRegistry(nil)
	gateway := NewCommandBusGateway(registry)
	options := ActivationOptions{Description: "Fix a label\nKeep the quoted 'value'", SelectedPackages: []string{"the8020/demo"}}
	for _, generation := range []string{"first", "replacement"} {
		registrations := []core.Registration{}
		for index, command := range testActivationCommands() {
			command.ID += "-" + generation
			registrations = append(registrations, core.Registration{Command: command, Handler: func(_ context.Context, request core.Request) (core.Execution, error) {
				want := []string{"developer", "--message", options.Description, "--packages", "the8020/demo"}
				if request.Arguments != nil || !slices.Equal(request.Argv, want) || request.CatalogRevision != registry.Catalog().Revision {
					t.Errorf("package command request = %#v", request)
				}
				if index == 0 {
					return core.Execution{Result: core.Result{"preview": ActivationPreview{}}}, nil
				}
				return core.Execution{Result: core.Result{"activation": ActivationResult{Success: true, Status: "committed"}}}, nil
			}})
		}
		if err := registry.ReplacePackages(registrations, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := gateway.Preview(context.Background(), "developer", options); err != nil {
			t.Fatal(err)
		}
		if result, err := gateway.Activate(context.Background(), "developer", options); err != nil || !result.Success {
			t.Fatalf("activate = %#v, %v", result, err)
		}
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

func TestShutdownOverlapsIndependentSandboxes(t *testing.T) {
	platform := newTestPlatform(t)
	m := platform.manager
	sandboxes := map[string]Sandbox{}
	for _, user := range []string{"alice", "bravo"} {
		if _, err := m.EnsureSandbox(context.Background(), user); err != nil {
			t.Fatal(err)
		}
		sandbox, err := m.Inspect(user)
		if err != nil {
			t.Fatal(err)
		}
		sandboxes[sandbox.SandboxID] = sandbox
		shell(t, m, user, "printf 'private-"+user+"\\n' >/workspace/packages/the8020/dev-core/notes.txt")
	}
	waiting := make(chan struct{})
	release := sync.OnceFunc(func() { close(waiting) })
	platform.driver.stops, platform.driver.stopWait = make(chan string, 2), waiting
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	closed := make(chan struct{})
	var closeErr error
	go func() { closeErr = m.Close(ctx); close(closed) }()
	defer func() { release(); <-closed }()
	for range sandboxes {
		select {
		case <-platform.driver.stops:
		case <-time.After(time.Second):
			t.Fatal("one sandbox's graceful stop blocked another sandbox's cleanup")
		}
	}
	release()
	<-closed
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	for _, sandbox := range sandboxes {
		current, err := m.Inspect(sandbox.UserID)
		if err != nil || current.State != StateStopped {
			t.Fatalf("sandbox was not durably stopped: %+v: %v", current, err)
		}
		if _, owned := m.owned.Load(sandbox.SandboxID); owned {
			t.Fatal("stopped sandbox retained live ownership")
		}
	}
}

func TestEnsureSandboxCreatesAndRestartsDirectly(t *testing.T) {
	platform := newTestPlatform(t)
	first, err := platform.manager.EnsureSandbox(context.Background(), "sshuser")
	if err != nil {
		t.Fatal(err)
	}
	if !identity.Is(first, "sbx") || platform.driver.starts != 1 {
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
	for _, owner := range []string{"alice", "teamuser"} {
		got, err := platform.manager.EnsureSandbox(context.Background(), owner)
		if err != nil || !identity.Is(got, "sbx") || got == first {
			t.Errorf("development sandbox for %q = %q, err=%v", owner, got, err)
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
	for _, relative := range []string{"sandbox.toml", "system"} {
		if _, err := os.Stat(filepath.Join(root, relative)); err != nil {
			t.Errorf("missing persisted sandbox path %s: %v", relative, err)
		}
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
	installTestDevelopmentAssets(t, root)
	writeTestFile(t, filepath.Join(image, "base"), "base")
	if err := writeAtomic(record, []byte(`{"image_digest":"sha256:test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sandbox := Sandbox{Schema: sandboxSchema, UserID: "developer", SandboxID: "sbx-abcdefghij", State: StateReady, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Token: "token"}
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
	driver.views["sbx-klmnopqrst"] = &fakeView{start: SandboxStart{SandboxID: "sbx-klmnopqrst"}, running: true}
	aliceRecord := sandbox
	aliceRecord.UserID, aliceRecord.SandboxID = "alice", "sbx-klmnopqrst"
	if err := os.MkdirAll(filepath.Join(users, "alice", "dev-sandbox", "runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeTOML(filepath.Join(users, "alice", "dev-sandbox", "sandbox.toml"), aliceRecord, 0o600); err != nil {
		t.Fatal(err)
	}
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
	aliceRecord.State = StateCreating
	if err := manager.prepareSandboxStorage(context.Background(), &aliceRecord, "sha256:test"); err != nil {
		t.Fatal(err)
	}
	if err := manager.saveSandbox(aliceRecord); err != nil {
		t.Fatal(err)
	}
	alice, err := manager.EnsureSandbox(context.Background(), "alice")
	if err != nil || alice != "sbx-klmnopqrst" {
		t.Fatalf("retained development sandbox = %#v, %v", alice, err)
	}
	close(wait)
	select {
	case <-manager.cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("inherited cleanup did not finish")
	}
	if running, err := driver.Running(context.Background(), "sbx-klmnopqrst"); err != nil || !running {
		t.Fatalf("inherited cleanup deleted the current registered sandbox: running=%v err=%v", running, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDevelopmentSpecUsesNativeWorkspace(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"rootfs", "packages", "bundle"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	start := SandboxStart{UserID: "alice", SandboxID: "sbx-klmnopqrst", Packages: filepath.Join(root, "packages"), RootFS: filepath.Join(root, "rootfs"), Mounts: []SandboxMount{
		{MountDefinition: MountDefinition{ID: "packages", Target: "/workspace/packages", Behavior: MountSandboxSource, Writable: true}, HostSource: filepath.Join(root, "packages")},
		{MountDefinition: MountDefinition{ID: "temporary", Target: "/tmp", Behavior: MountEphemeral, Writable: true}},
	}}
	spec := developmentSpec(start, filepath.Join(root, "bundle"))
	if spec.Root.Path != start.RootFS || spec.Root.Readonly {
		t.Fatalf("development root = %#v", spec.Root)
	}
	if spec.Annotations["the8020.workspace.lower"] != start.Packages || spec.Annotations["the8020.workspace.control"] != "true" || spec.Annotations["the8020.workspace.git"] != "true" {
		t.Fatalf("development workspace annotations = %#v", spec.Annotations)
	}
	if len(spec.Process.Args) != 2 || spec.Process.Args[1] != "/workspace/scripts/development-init.sh" {
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
	driver := &RunscDriver{config: RunscConfig{RuntimeRoot: filepath.Join(root, "runtime"), SandboxRoot: filepath.Join(root, "sandboxes")}}
	flags := strings.Join(driver.flags("sbx-0123456789", "run"), " ")
	if !strings.Contains(flags, "--directfs=false") || !strings.Contains(flags, "--overlay2=none") || strings.Contains(flags, "overlay2=all") || strings.Contains(flags, "overlay2=root") || strings.Contains(flags, "rootfs-tar") {
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

func TestDevelopmentIDCollisionPreservesOwner(t *testing.T) {
	platform := newTestPlatform(t)
	first, err := platform.manager.Create(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	conflict := first
	conflict.UserID = "bobby"
	if err := platform.manager.registerSandbox(conflict); err == nil {
		t.Fatal("duplicate retained sandbox ID registered")
	}
	if err := platform.manager.startLocked(context.Background(), &conflict); err == nil {
		t.Fatal("foreign live sandbox reused")
	}
	if !platform.manager.HasSandbox(first.SandboxID) {
		t.Fatal("collision removed the original owner")
	}
	if running, err := platform.driver.Running(context.Background(), first.SandboxID); err != nil || !running {
		t.Fatalf("original sandbox disrupted: %v %v", running, err)
	}
	reset, err := platform.manager.FactoryReset(context.Background(), "alice", true)
	if err != nil {
		t.Fatal(err)
	}
	if reset.SandboxID == first.SandboxID || !identity.Is(reset.SandboxID, "sbx") {
		t.Fatal("factory reset must allocate a new sandbox identity")
	}
	if platform.manager.sandboxRoot(reset) != platform.manager.sandboxRoot(first) {
		t.Fatal("factory reset changed user storage ownership")
	}
}
