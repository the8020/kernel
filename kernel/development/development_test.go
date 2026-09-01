package development

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"the8020/kernel/cbus/core"
)

type fakeView struct {
	start   SandboxStart
	paused  bool
	running bool
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

func (d *fakeDriver) Start(_ context.Context, start SandboxStart) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.startErr != nil {
		return d.startErr
	}
	d.starts++
	d.views[start.SandboxID] = &fakeView{start: start, running: true}
	return nil
}

func (d *fakeDriver) Exec(_ context.Context, id, command string) ([]byte, error) {
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
			return filepath.Join(view.start.Packages, filepath.FromSlash(strings.TrimPrefix(value, "packages/")))
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
	for _, directory := range []string{packages, users, runtimeRoot, image, filepath.Dir(record)} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(image, "usr", "bin", "base-tool"), "image-default\n")
	for _, id := range []string{"the8020/dev-core", "the8020/demo"} {
		packageRoot := filepath.Join(packages, filepath.FromSlash(id))
		writeTestFile(t, filepath.Join(packageRoot, "package.toml"), "schema = 1\n")
		writeTestFile(t, filepath.Join(packageRoot, "src", "message.ts"), "export const message = \"shared\";\n")
		writeTestFile(t, filepath.Join(packageRoot, "notes.txt"), id+" notes\n")
	}
	if err := writeAtomic(record, []byte(`{"image_digest":"sha256:`+strings.Repeat("1", 64)+`","codex_version":"test","deno_version":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := newFakeDriver()
	registry := core.NewRegistry(nil)
	manager, err := New(Config{Root: root, PackagesRoot: packages, ConfigRoot: filepath.Join(root, "config"), UsersRoot: users, RuntimeRoot: runtimeRoot, ImageRoot: image, ImageRecord: record, Driver: driver, ActivationGateway: NewCommandBusGateway(registry)})
	if err != nil {
		t.Fatal(err)
	}
	registerTestActivationCommands(t, registry, manager)
	for _, id := range []string{"the8020/dev-core", "the8020/demo"} {
		if _, err := manager.InitializeRepository(context.Background(), id, "Test Developer", "developer@example.test", "Initial package"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	return testPlatform{root: root, users: users, manager: manager, driver: driver}
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
		result, err := manager.Preview(ctx, request.Arguments["workspace_id"].(string), decode(request))
		return core.Result{"preview": result}, err
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(commands[1], func(ctx context.Context, request core.Request) (core.Result, error) {
		result, err := manager.Activate(ctx, request.Arguments["workspace_id"].(string), decode(request))
		return core.Result{"activation": result}, err
	}); err != nil {
		t.Fatal(err)
	}
}

func testActivationCommands() []core.Command {
	parameters := []core.Parameter{
		{Name: "workspace_id", Type: "string", Position: 0, Required: true},
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

func writeTestFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func shell(t *testing.T, manager *Manager, workspace, command string) string {
	t.Helper()
	result, err := manager.Shell(context.Background(), workspace, command)
	if err != nil {
		t.Fatal(err)
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

func TestEnsureDefaultSandboxCreatesAndRestartsDirectly(t *testing.T) {
	platform := newTestPlatform(t)
	first, err := platform.manager.EnsureDefaultSandbox(context.Background(), "sshuser")
	if err != nil {
		t.Fatal(err)
	}
	if first != "dev-sshuser" || platform.driver.starts != 1 {
		t.Fatalf("first default sandbox = %q, starts=%d", first, platform.driver.starts)
	}
	second, err := platform.manager.EnsureDefaultSandbox(context.Background(), "sshuser")
	if err != nil || second != first || platform.driver.starts != 1 {
		t.Fatalf("reused default sandbox = %q, starts=%d, err=%v", second, platform.driver.starts, err)
	}
	workspace, err := platform.manager.Inspect(workspaceID("sshuser"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := platform.manager.Stop(context.Background(), workspace.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	restarted, err := platform.manager.EnsureDefaultSandbox(context.Background(), "sshuser")
	if err != nil || restarted != first || platform.driver.starts != 2 {
		t.Fatalf("restarted default sandbox = %q, starts=%d, err=%v", restarted, platform.driver.starts, err)
	}
	for owner, wanted := range map[string]string{"alice": "dev-alice", "teamuser": "dev-teamuser"} {
		if got, err := sandboxIDForUser(owner); err != nil || got != wanted {
			t.Errorf("development sandbox ID for %q = %q, want %q", owner, got, wanted)
		}
	}
	for _, owner := range []string{"ab", "Alice", "alice@example", strings.Repeat("a", 33), "Unicode User/管理"} {
		if _, err := platform.manager.EnsureDefaultSandbox(context.Background(), owner); err == nil {
			t.Errorf("unsupported development username %q was accepted", owner)
		}
	}
}

func TestNativeStoragePersistsWithoutBackgroundWork(t *testing.T) {
	platform := newTestPlatform(t)
	a, err := platform.manager.Create(context.Background(), "developera")
	if err != nil {
		t.Fatal(err)
	}
	b, err := platform.manager.Create(context.Background(), "developerb")
	if err != nil {
		t.Fatal(err)
	}
	if a.SourcePath == b.SourcePath || !strings.Contains(a.SourcePath, filepath.Join("workspaces", a.WorkspaceID, "source", "packages")) {
		t.Fatalf("workspace source paths are not private durable storage: %q %q", a.SourcePath, b.SourcePath)
	}
	if !strings.Contains(a.SystemPath, filepath.Join("users", "developera", "system")) {
		t.Fatalf("system path is not user-owned durable storage: %q", a.SystemPath)
	}
	shell(t, platform.manager, a.WorkspaceID, "write packages/the8020/dev-core/src/message.ts private-a")
	shell(t, platform.manager, a.WorkspaceID, "write home/.codex/config.toml model=test")
	shell(t, platform.manager, a.WorkspaceID, "write system/usr/local/bin/private-tool tool")
	if got := shell(t, platform.manager, b.WorkspaceID, "read packages/the8020/dev-core/src/message.ts"); strings.Contains(got, "private-a") {
		t.Fatal("workspace B observed workspace A's source")
	}
	shared, _ := os.ReadFile(filepath.Join(platform.root, "packages", "the8020", "dev-core", "src", "message.ts"))
	if strings.Contains(string(shared), "private-a") {
		t.Fatal("private edit changed shared packages")
	}
	execs := platform.driver.execs
	time.Sleep(1200 * time.Millisecond)
	if platform.driver.execs != execs {
		t.Fatalf("idle workspace performed background sandbox work: %d -> %d", execs, platform.driver.execs)
	}
	starts := platform.driver.starts
	if _, err := platform.manager.Stop(context.Background(), a.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	a, err = platform.manager.Start(context.Background(), a.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if platform.driver.starts != starts+1 {
		t.Fatal("restart did not create exactly one sandbox process")
	}
	for _, proof := range []struct{ command, want string }{
		{"read packages/the8020/dev-core/src/message.ts", "private-a"},
		{"read home/.codex/config.toml", "model=test"},
		{"read system/usr/local/bin/private-tool", "tool"},
	} {
		if got := shell(t, platform.manager, a.WorkspaceID, proof.command); got != proof.want {
			t.Fatalf("%s = %q, want %q", proof.command, got, proof.want)
		}
	}
}

func TestActivationScansOnlyOnDemandAndDoesNotRecreateSandbox(t *testing.T) {
	platform := newTestPlatform(t)
	workspace, err := platform.manager.Create(context.Background(), "developer")
	if err != nil {
		t.Fatal(err)
	}
	shell(t, platform.manager, workspace.WorkspaceID, "write packages/the8020/dev-core/src/message.ts private-a")
	shell(t, platform.manager, workspace.WorkspaceID, "write packages/the8020/demo/notes.txt private-b")
	preview, err := platform.manager.Preview(context.Background(), workspace.WorkspaceID, ActivationOptions{SelectedPackages: []string{"the8020/dev-core"}})
	if err != nil || len(preview.Packages) != 2 {
		t.Fatalf("preview = %#v, %v", preview, err)
	}
	starts := platform.driver.starts
	oldSandbox := workspace.ActiveSandboxID
	result, err := platform.manager.Activate(context.Background(), workspace.WorkspaceID, ActivationOptions{Description: "Common", SelectedPackages: []string{"the8020/dev-core"}, AuthorName: "Developer", AuthorEmail: "developer@example.test"})
	if err != nil || !result.Success || packageResult(result, "the8020/dev-core").Status != "committed" {
		t.Fatalf("activation = %#v, %v", result, err)
	}
	current, _ := platform.manager.Inspect(workspace.WorkspaceID)
	if platform.driver.starts != starts || current.ActiveSandboxID != oldSandbox {
		t.Fatal("activation recreated a sandbox despite direct durable source storage")
	}
	if got, _ := os.ReadFile(filepath.Join(platform.root, "packages", "the8020", "dev-core", "src", "message.ts")); string(got) != "private-a" {
		t.Fatalf("activated shared source = %q", got)
	}
	remaining, err := platform.manager.Preview(context.Background(), workspace.WorkspaceID, ActivationOptions{})
	if err != nil || len(remaining.Packages) != 1 || remaining.Packages[0].PackageID != "the8020/demo" {
		t.Fatalf("remaining changes = %#v, %v", remaining, err)
	}
	writeTestFile(t, filepath.Join(workspace.SystemPath, "root", ".gitconfig"), "[user]\n\tname = Root Developer\n\temail = root@example.test\n")
	result, err = platform.manager.Activate(context.Background(), workspace.WorkspaceID, ActivationOptions{Description: "Fallback", PackageMessages: map[string]string{"the8020/demo": "Override"}})
	if err != nil || !result.Success {
		t.Fatalf("second activation = %#v, %v", result, err)
	}
	message, _ := gitOutput(filepath.Join(platform.root, "packages", "the8020", "demo"), "log", "-1", "--pretty=%s")
	if message != "Override" {
		t.Fatalf("package message = %q", message)
	}
	author, _ := gitOutput(filepath.Join(platform.root, "packages", "the8020", "demo"), "log", "-1", "--pretty=%an <%ae>")
	if author != "Root Developer <root@example.test>" {
		t.Fatalf("activation author from root home = %q", author)
	}
}

func TestActivationConflictPersistsInNativeSource(t *testing.T) {
	platform := newTestPlatform(t)
	a, _ := platform.manager.Create(context.Background(), "developera")
	b, _ := platform.manager.Create(context.Background(), "developerb")
	shell(t, platform.manager, b.WorkspaceID, "write packages/the8020/dev-core/src/message.ts private-b")
	shell(t, platform.manager, a.WorkspaceID, "write packages/the8020/dev-core/src/message.ts private-a")
	if _, err := platform.manager.Activate(context.Background(), a.WorkspaceID, ActivationOptions{Description: "Advance A"}); err != nil {
		t.Fatal(err)
	}
	oldSandbox := b.ActiveSandboxID
	result, err := platform.manager.Activate(context.Background(), b.WorkspaceID, ActivationOptions{Description: "Conflict B"})
	if err == nil || result.Status != "conflicted" {
		t.Fatalf("conflict = %#v, %v", result, err)
	}
	current, _ := platform.manager.Inspect(b.WorkspaceID)
	if current.ActiveSandboxID != oldSandbox || current.State != StateConflicted {
		t.Fatalf("conflicted workspace = %#v", current)
	}
	contents, _ := os.ReadFile(filepath.Join(b.SourcePath, "the8020", "dev-core", "src", "message.ts"))
	if !strings.Contains(string(contents), "<<<<<<<") {
		t.Fatalf("conflict markers were not persisted: %q", contents)
	}
	shell(t, platform.manager, b.WorkspaceID, "write packages/the8020/dev-core/src/message.ts resolved-b")
	if _, err := platform.manager.Activate(context.Background(), b.WorkspaceID, ActivationOptions{Description: "Resolve B"}); err != nil {
		t.Fatal(err)
	}
}

func TestResetBoundariesAndRepositoryInspectionLock(t *testing.T) {
	platform := newTestPlatform(t)
	workspace, _ := platform.manager.Create(context.Background(), "developer")
	shell(t, platform.manager, workspace.WorkspaceID, "write packages/the8020/dev-core/new.txt source")
	shell(t, platform.manager, workspace.WorkspaceID, "write home/proof home")
	shell(t, platform.manager, workspace.WorkspaceID, "write system/proof system")
	userMarker := filepath.Join(platform.users, "developer", "profile.json")
	writeTestFile(t, userMarker, "preserve\n")

	workspaceLock := platform.manager.workspaceLock(workspace.WorkspaceID)
	workspaceLock.Lock()
	inspected := make(chan error, 1)
	go func() {
		_, err := platform.manager.InspectRepository("the8020/dev-core")
		inspected <- err
	}()
	select {
	case err := <-inspected:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("repository inspection waited for the workspace lifecycle lock")
	}
	workspaceLock.Unlock()

	reset, err := platform.manager.ResetSource(context.Background(), workspace.WorkspaceID, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(reset.SourcePath, "the8020", "dev-core", "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source reset retained private source")
	}
	for _, path := range []string{filepath.Join(reset.SystemPath, "root", "proof"), filepath.Join(reset.SystemPath, "proof")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("source reset removed durable user storage %s: %v", path, err)
		}
	}
	factory, err := platform.manager.FactoryReset(context.Background(), workspace.WorkspaceID, true)
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
	workspace, err := platform.manager.Create(context.Background(), "failedstart")
	if err == nil || workspace.ActiveSandboxID != "" {
		t.Fatalf("failed workspace = %#v, %v", workspace, err)
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

func TestInheritedCleanupNeverGatesStartupOrScansWorkspaces(t *testing.T) {
	root := t.TempDir()
	packages := filepath.Join(root, "packages")
	users := filepath.Join(root, "users")
	runtimeRoot := filepath.Join(root, "node", "kernel", "runtime", "development")
	image := filepath.Join(root, "node", "images", "development", "rootfs")
	record := filepath.Join(root, "node", "images", "development", "image.json")
	for _, directory := range []string{packages, users, runtimeRoot, image, filepath.Dir(record)} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(image, "base"), "base")
	if err := writeAtomic(record, []byte(`{"image_digest":"sha256:test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := Workspace{Schema: workspaceSchema, WorkspaceID: workspaceID("developer"), OwnerUserID: "developer", ActiveSandboxID: "dev-old12345", State: StateReady, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Token: "token", MountProfile: DefaultMountProfile()}
	workspaceRoot := filepath.Join(users, "developer", "workspaces", workspace.WorkspaceID)
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeTOML(filepath.Join(workspaceRoot, "workspace.toml"), workspace, 0o600); err != nil {
		t.Fatal(err)
	}
	wait := make(chan struct{})
	driver := newFakeDriver()
	driver.listWait = wait
	driver.views["dev-alice"] = &fakeView{start: SandboxStart{SandboxID: "dev-alice"}, running: true}
	started := time.Now()
	manager, err := New(Config{Root: root, PackagesRoot: packages, ConfigRoot: filepath.Join(root, "config"), UsersRoot: users, RuntimeRoot: runtimeRoot, ImageRoot: image, ImageRecord: record, Driver: driver})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("development cleanup gated startup for %s", elapsed)
	}
	inspected, err := manager.Inspect(workspace.WorkspaceID)
	if err != nil || inspected.ActiveSandboxID != "" || inspected.State != StateStopped {
		t.Fatalf("stale workspace was not normalized lazily: %#v, %v", inspected, err)
	}
	alice, err := manager.Create(context.Background(), "alice")
	if err != nil || alice.ActiveSandboxID != "dev-alice" {
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

func TestDevelopmentSpecUsesNativeRootWithoutOverlay(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"rootfs", "packages", "bundle"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	start := SandboxStart{WorkspaceID: "workspace", SandboxID: "dev-alice", Packages: filepath.Join(root, "packages"), RootFS: filepath.Join(root, "rootfs"), Mounts: []SandboxMount{
		{MountDefinition: MountDefinition{ID: "packages", Target: "/workspace/packages", Behavior: MountWorkspaceSource, Writable: true}, HostSource: filepath.Join(root, "packages")},
		{MountDefinition: MountDefinition{ID: "temporary", Target: "/tmp", Behavior: MountEphemeral, Writable: true}},
	}}
	spec := developmentSpec(start, filepath.Join(root, "bundle"))
	if spec.Root.Path != start.RootFS || spec.Root.Readonly || len(spec.Annotations) != 0 {
		t.Fatalf("development root = %#v", spec.Root)
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
	if !strings.Contains(flags, "--overlay2=none") || strings.Contains(flags, "overlay2=all") || strings.Contains(flags, "overlay2=root") || strings.Contains(flags, "rootfs-tar") {
		t.Fatalf("development driver still configures snapshots or overlays: %s", flags)
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
