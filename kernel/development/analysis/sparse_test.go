//go:build workflowanalysis

// run.py overlays this fixture and mount specification into the real driver.
// It does not replace production activation or qualify its missing semantics.
package development

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	platformconsole "the8020/kernel/console"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/sandbox/backend"
)

type analysisSparseDriver struct {
	*RunscDriver
	storage, shared  string
	listener         *net.UnixListener
	control          net.Conn
	controlMu        sync.Mutex
	loseReleaseReply bool // Inject one lost reply after the Gofer completed release.
}

// Wire metadata shared with the disposable Gofer, without file payloads.
func analysisObjectID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) != 40 && len(value) != 64 {
		return "", errors.New("invalid Git object ID length")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", errors.New("invalid Git object ID")
	}
	return value, nil
}

type analysisFileReference struct {
	Source, Package, Blob, Version string
	Mode                           uint32
}

func (d *analysisSparseDriver) fileReferences(ctx context.Context, name string) (map[string]analysisFileReference, error) {
	parts := strings.SplitN(name, "/", 3)
	if !validRelative(name) || filepath.ToSlash(filepath.Clean(name)) != name {
		return nil, errors.New("invalid rename source")
	}
	if len(parts) == 1 {
		refs := map[string]analysisFileReference{}
		for _, id := range packageDirectories(d.shared) {
			if !strings.HasPrefix(id, name+"/") {
				continue
			}
			files, err := d.fileReferences(ctx, id)
			if err != nil {
				return nil, err
			}
			for path, ref := range files {
				refs[path] = ref
			}
			if len(refs) > 4096 {
				return nil, errors.New("rename exceeds 4,096 files")
			}
		}
		return refs, nil
	}
	id := parts[0] + "/" + parts[1]
	relative := "."
	if len(parts) == 3 {
		relative = parts[2]
	}
	if _, err := workspacepackages.ParsePackageID(id); err != nil {
		return nil, err
	}
	validate, release, err := workspacepackages.ObserveSources(ctx, d.shared, []string{id})
	if err != nil {
		return nil, err
	}
	defer release()
	if err := d.retainGitObjects(ctx, id); err != nil {
		return nil, err
	}
	root := filepath.Join(d.shared, id)
	if _, err := gitCommand(ctx, root, nil, "--literal-pathspecs", "diff", "--quiet", "HEAD", "--", relative); err != nil {
		return nil, fmt.Errorf("rename requires published source: %w", err)
	}
	entries, err := gitCommand(ctx, root, nil, "--literal-pathspecs", "ls-tree", "-r", "-z", "HEAD", "--", relative)
	if err != nil {
		return nil, err
	}
	refs := map[string]analysisFileReference{}
	for _, entry := range strings.Split(entries, "\x00") {
		if entry == "" {
			continue
		}
		header, relative, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 || fields[1] != "blob" || !validRelative(relative) {
			return nil, errors.New("unsupported rename source")
		}
		var s unix.Stat_t
		if err := unix.Lstat(filepath.Join(root, relative), &s); err != nil {
			return nil, err
		}
		refs[id+"/"+relative] = analysisFileReference{Source: id + "/" + relative, Package: id, Blob: fields[2], Mode: s.Mode,
			Version: fmt.Sprintf("%d:%d:%d:%d:%d:%d:%d:%d:%d", unix.Major(s.Dev), unix.Minor(s.Dev), s.Ino, s.Mode, s.Size, s.Mtim.Sec, s.Mtim.Nsec, s.Ctim.Sec, s.Ctim.Nsec)}
		if len(refs) > 4096 {
			return nil, errors.New("rename exceeds 4,096 files")
		}
	}
	return refs, validate()
}

func (d *analysisSparseDriver) materializeReference(ctx context.Context, ref analysisFileReference) (string, error) {
	if _, err := workspacepackages.ParsePackageID(ref.Package); err != nil {
		return "", err
	}
	if _, err := analysisObjectID(ref.Blob); err != nil {
		return "", err
	}
	mode := os.FileMode(ref.Mode & 0777)
	kind := ref.Mode & unix.S_IFMT
	if kind != unix.S_IFREG && kind != unix.S_IFLNK {
		return "", errors.New("unsupported referenced content")
	}
	name := filepath.Join(".reference-data", ref.Package, fmt.Sprintf("%s-%o", ref.Blob, ref.Mode))
	filename := filepath.Join(d.storage, "snapshots", name)
	if _, err := os.Lstat(filename); err == nil {
		return name, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return "", err
	}
	// Only kernel-owned retained objects are passed to host Git. Developer Git
	// configuration and hooks are never read at this boundary.
	command := exec.CommandContext(ctx, "git", "--git-dir="+filepath.Join(d.storage, "borrowed", ref.Package), "cat-file", "blob", ref.Blob)
	if kind == unix.S_IFLNK {
		body := &boundedBuffer{limit: 4096}
		command.Stdout = body
		if err := command.Run(); err != nil || body.truncated {
			return "", fmt.Errorf("read referenced link: %v", err)
		}
		return name, os.Symlink(body.String(), filename)
	}
	file, err := os.CreateTemp(filepath.Dir(filename), ".content-")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	command.Stdout = file
	err = errors.Join(command.Run(), file.Chmod(mode), file.Sync(), file.Close())
	if err != nil {
		return "", err
	}
	return name, os.Rename(file.Name(), filename)
}

func developmentSpec(start SandboxStart, bundle string) specs.Spec {
	spec := analysisOriginalDevelopmentSpec(start, bundle)
	spec.Annotations["workflow.probe.lower"] = start.Packages
	spec.Annotations["workflow.probe.control"] = "true"
	spec.Annotations["workflow.probe.git"] = "true"
	for i := range spec.Mounts {
		if spec.Mounts[i].Destination == "/workspace/packages" {
			spec.Mounts[i].Options = append(spec.Mounts[i].Options, "overlayfs_stale_read", "dcache=0")
		}
	}
	return spec
}

func (d *analysisSparseDriver) Start(ctx context.Context, start SandboxStart) error {
	// Private Git writes are separate from both source copy-up and the retained
	// object links. The latter are readable only; developers cannot modify an
	// inode shared with authoritative Git. Neither mount contains a checkout.
	for _, name := range []string{"git", "borrowed"} {
		if err := os.MkdirAll(filepath.Join(d.storage, name), 0700); err != nil {
			return err
		}
	}
	start.Mounts = append(start.Mounts, SandboxMount{MountDefinition: MountDefinition{
		ID: "private-git", Target: "/workspace/git/private", Behavior: MountPersistent, Writable: true, Executable: true,
	}, HostSource: filepath.Join(d.storage, "git")}, SandboxMount{MountDefinition: MountDefinition{
		ID: "borrowed-git", Target: "/workspace/git/borrowed", Behavior: MountReadOnly,
	}, HostSource: filepath.Join(d.storage, "borrowed")}, SandboxMount{MountDefinition: MountDefinition{
		ID: "shared-git", Target: "/workspace/git/shared", Behavior: MountReadOnly,
	}, HostSource: d.shared})
	for i := range start.Mounts {
		if start.Mounts[i].Behavior == MountSandboxSource {
			start.Mounts[i].HostSource = d.storage
			start.Mounts[i].Behavior = MountPersistent
		}
	}
	if d.control != nil {
		d.control.Close()
	}
	gitSocket := filepath.Join(d.storage, "git.sock")
	if err := os.Remove(gitSocket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	gitListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: gitSocket, Net: "unix"})
	if err != nil {
		return err
	}
	defer gitListener.Close()
	gitReady := make(chan struct{})
	go d.serveGitInitialization(gitListener, gitReady)
	if err := d.RunscDriver.Start(ctx, start); err != nil {
		return err
	}
	if err := d.listener.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return err
	}
	d.control, err = d.listener.Accept()
	if err == nil {
		select {
		case <-gitReady:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

// The Gofer asks before exposing a newly discovered shared .git directory.
// Git stays in its existing kernel owner; no developer command needs a wrapper.
func (d *analysisSparseDriver) serveGitInitialization(listener *net.UnixListener, ready chan<- struct{}) {
	conn, err := listener.AcceptUnix()
	close(ready)
	if err != nil {
		return
	}
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024), 8<<20)
	for scanner.Scan() {
		if len(scanner.Bytes()) != 0 && scanner.Bytes()[0] == '{' {
			var request struct {
				Action, Path string
				Reference    analysisFileReference
			}
			var response struct {
				Error      string
				References map[string]analysisFileReference
				Path       string
			}
			err := json.Unmarshal(scanner.Bytes(), &request)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err == nil {
				switch request.Action {
				case "references":
					response.References, err = d.fileReferences(ctx, request.Path)
				case "materialize":
					response.Path, err = d.materializeReference(ctx, request.Reference)
				default:
					err = errors.New("unknown content request")
				}
			}
			cancel()
			if err != nil {
				response.Error = err.Error()
			}
			if json.NewEncoder(conn).Encode(response) != nil {
				return
			}
			continue
		}
		var id string
		err := json.Unmarshal(scanner.Bytes(), &id)
		if err == nil {
			_, err = workspacepackages.ParsePackageID(id)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err == nil {
			err = d.initializeGit(ctx, id)
		}
		cancel()
		message := ""
		if err != nil {
			message = err.Error()
			if len(message) > 2048 {
				message = message[:2048]
			}
		}
		if json.NewEncoder(conn).Encode(message) != nil {
			return
		}
	}
}

func (d *analysisSparseDriver) initializeGit(ctx context.Context, id string) error {
	validate, release, err := workspacepackages.ObserveSources(ctx, d.shared, []string{id})
	if err != nil {
		return err
	}
	defer release()
	return d.initializeGitOwned(ctx, id, validate)
}

// A publishing caller already holds the source lock; readers validate their
// observed source before installing metadata into the private workspace.
func (d *analysisSparseDriver) initializeGitOwned(ctx context.Context, id string, validate func() error) error {
	if err := d.retainGitObjects(ctx, id); err != nil {
		return err
	}
	private, err := os.OpenRoot(filepath.Join(d.storage, "git"))
	if err != nil {
		return err
	}
	defer private.Close()
	if info, err := private.Lstat(id + "/.git"); os.IsNotExist(err) {
		// Build outside every developer-visible mount. A running developer may
		// change private Git paths, so host Git must never initialize in them.
		temporary, err := os.MkdirTemp(d.storage, ".git-init-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(temporary)
		gitRoot := filepath.Join(temporary, "repository")
		if _, err := gitCommand(ctx, d.storage, nil, "-c", "init.templateDir=", "clone", "--shared", "--no-checkout", filepath.Join(d.shared, id), gitRoot); err != nil {
			return err
		}
		// The .git reference determines the worktree, including after mv.
		for _, args := range [][]string{{"read-tree", "HEAD"}, {"config", "remote.origin.url", "/workspace/git/shared/" + id}} {
			if _, err := gitCommand(ctx, gitRoot, nil, args...); err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(gitRoot, ".git/objects/info/alternates"), []byte("/workspace/git/borrowed/"+id+"/objects\n"), 0600); err != nil {
			return err
		}
		if validate != nil {
			if err := validate(); err != nil {
				return err
			}
		}
		if err := private.MkdirAll(id, 0700); err != nil {
			return err
		}
		parent, err := private.Open(id)
		if err != nil {
			return err
		}
		defer parent.Close()
		staged, err := os.Open(gitRoot)
		if err != nil {
			return err
		}
		defer staged.Close()
		if err := unix.Renameat2(int(staged.Fd()), ".git", int(parent.Fd()), ".git", unix.RENAME_NOREPLACE); err != nil {
			return err
		}
		if err := parent.Sync(); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("private Git metadata must be an ordinary directory: %s", id)
	} else if validate != nil {
		if err := validate(); err != nil {
			return err
		}
	}
	return analysisGitReference(d.storage, id)
}

// The caller locks publication or validates its source read before using these
// retained objects. Hardlink files without copying or replacing retained names.
// This prototype retains the links for the workspace lifetime. Shared/user
// roots must support hardlinks; cross-filesystem storage fails explicitly.
func (d *analysisSparseDriver) retainGitObjects(ctx context.Context, id string) error {
	source := filepath.Join(d.shared, id, ".git/objects")
	if alternates, err := os.ReadFile(filepath.Join(source, "info/alternates")); err == nil && len(alternates) != 0 {
		return errors.New("shared Git alternates require independent object retention")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	target := filepath.Join(d.storage, "borrowed", id, "objects")
	entries := 0
	if err := filepath.WalkDir(source, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// ponytail: a bounded metadata walk per selected package; use the
		// shared Git publisher's changed-object list if this becomes costly.
		entries++
		if entries > 100000 {
			return errors.New("Git object retention exceeds 100,000 entries")
		}
		relative, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0700)
		}
		if !entry.Type().IsRegular() || strings.HasSuffix(name, ".promisor") {
			return fmt.Errorf("unsupported shared Git object entry: %s", relative)
		}
		if err := os.Link(name, destination); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(target), "HEAD")); errors.Is(err, os.ErrNotExist) {
		format, err := gitCommand(ctx, filepath.Join(d.shared, id), nil, "rev-parse", "--show-object-format")
		if err != nil {
			return err
		}
		_, err = gitCommand(ctx, d.storage, nil, "init", "--bare", "--template=", "--object-format="+strings.TrimSpace(format), filepath.Dir(target))
		return err
	} else {
		return err
	}
}

func (d *analysisSparseDriver) isSharedGitObject(name string, info os.FileInfo) bool {
	relative, err := filepath.Rel(filepath.Join(d.storage, "borrowed"), name)
	parts := strings.SplitN(relative, string(filepath.Separator), 3)
	if err != nil || len(parts) != 3 || parts[0] == ".." {
		return false
	}
	original, err := os.Stat(filepath.Join(d.shared, parts[0], parts[1], ".git", parts[2]))
	return err == nil && os.SameFile(info, original)
}

// Install a standard Git directory reference without overwriting private edits
// or resurrecting a reference removed through the filesystem. Confine all
// access to the private upper: package parents may contain user-made symlinks.
func analysisGitReference(storage, id string) error {
	upper, err := os.OpenRoot(filepath.Join(storage, "upper"))
	if err != nil {
		return err
	}
	defer upper.Close()
	// Initializing retained Git for a deletion must not recreate its source root.
	if _, err := upper.Lstat(id); os.IsNotExist(err) {
		for name := id; name != "."; name = filepath.Dir(name) {
			marker := fmt.Sprintf(".directories/%x", sha256.Sum256([]byte(name)))
			if _, err := os.Stat(filepath.Join(storage, "snapshots", marker)); err == nil {
				return nil
			} else if !os.IsNotExist(err) {
				return err
			}
		}
	}
	name := id + "/.git"
	if _, err := upper.Lstat(name); err == nil || errors.Is(err, syscall.ENOTDIR) {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if marker, err := os.Lstat(filepath.Join(storage, "deleted", name)); err == nil && !marker.IsDir() || errors.Is(err, syscall.ENOTDIR) {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := upper.MkdirAll(id, 0700); err != nil {
		return err
	}
	suffix, err := randomHex(12)
	if err != nil {
		return err
	}
	temporary := ".git-reference-" + suffix
	file, err := upper.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer upper.Remove(temporary)
	_, writeErr := file.WriteString("gitdir: /workspace/git/private/" + id + "/.git\n")
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := upper.Link(temporary, name); err != nil {
		return err
	}
	parent, err := upper.Open(id)
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}

func (d *analysisSparseDriver) request(t *testing.T, action, path, id string) map[string]any {
	t.Helper()
	response, err := d.exchange(context.Background(), action, path, id)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func (d *analysisSparseDriver) exchange(ctx context.Context, action, path, id string) (map[string]any, error) {
	return d.exchangeControl(ctx, map[string]any{"action": action, "path": path, "id": id})
}

func (d *analysisSparseDriver) exchangeControl(ctx context.Context, request map[string]any) (map[string]any, error) {
	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	deadline := time.Now().Add(15 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := d.control.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if err := json.NewEncoder(d.control).Encode(request); err != nil {
		return nil, err
	}
	var response map[string]any
	if err := json.NewDecoder(io.LimitReader(d.control, 4<<20)).Decode(&response); err != nil {
		return nil, err
	}
	if response["error"] != nil {
		return nil, fmt.Errorf("%s %s: %v", request["action"], request["path"], response)
	}
	if request["action"] == "release" && d.loseReleaseReply {
		d.loseReleaseReply = false
		return nil, fmt.Errorf("injected lost capture-release reply")
	}
	return response, nil
}

func analysisSparseRuntime(t *testing.T, userID string, assets int) (*Manager, *analysisSparseDriver, Sandbox, string) {
	t.Helper()
	m, d, shared := analysisRuntime(t)
	ctx := context.Background()
	source, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	storage := filepath.Join(m.sandboxRootForUser(userID), "workspace")
	for _, name := range []string{"lower", "upper", "base", "deleted", "snapshots"} {
		if err := os.MkdirAll(filepath.Join(storage, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(storage, "control.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	sparse := &analysisSparseDriver{RunscDriver: d, storage: storage, shared: m.config.PackagesRoot, listener: listener}
	t.Cleanup(func() {
		if sparse.control != nil {
			sparse.control.Close()
		}
		listener.Close()
	})
	// The real driver already creates the private user/mount namespace. Bind the
	// authoritative sources read-only before runsc confines the custom Gofer.
	wrapper := filepath.Join(m.config.Root, "sparse-runsc")
	script := "#!/bin/sh\nset -eu\nmount --bind " + shellQuote(sparse.shared) + " " + shellQuote(filepath.Join(storage, "lower")) +
		"\nmount -o remount,bind,ro " + shellQuote(filepath.Join(storage, "lower")) +
		"\nexec " + shellQuote(filepath.Join(source, ".development/workflow-gofer-build/runsc")) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	d.config.RunscPath = wrapper
	m.driver = sparse
	for i := 0; i < assets; i++ {
		asset := filepath.Join(shared, "assets", fmt.Sprintf("%d.bin", i))
		if err := os.MkdirAll(filepath.Dir(asset), 0700); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(asset)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.CopyN(f, rand.Reader, 1<<20)
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil {
			t.Fatalf("asset fixture: %v, %v", copyErr, closeErr)
		}
	}
	if _, err := gitCommand(ctx, shared, nil, "add", "assets"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-m", "Tracked random assets"); err != nil {
		t.Fatal(err)
	}
	sandbox, err := m.Create(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Delete(context.Background(), sandbox.UserID) })
	buildClient := exec.CommandContext(ctx, filepath.Join(source, ".development/toolchains/go/bin/go"), "build", "-o",
		filepath.Join(sandbox.SystemPath, "root/metadata-client"), filepath.Join(source, "kernel/development/analysis/native_probe.go"))
	buildClient.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := buildClient.CombinedOutput(); err != nil {
		t.Fatalf("build native metadata regression: %v: %s", err, output)
	}
	return m, sparse, sandbox, shared
}

func TestWorkflowAnalysisSparse(t *testing.T) {
	m, sparse, sandbox, shared := analysisSparseRuntime(t, "sparse", 4)
	d, storage := sparse.RunscDriver, sparse.storage
	ctx := context.Background()
	base, err := gitOutput(shared, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	prefix := "set -e; cd /workspace/packages/the8020/dev-core; "
	if entries, err := os.ReadDir(filepath.Join(storage, "git")); err != nil || len(entries) != 0 {
		t.Fatalf("sandbox startup initialized package Git metadata: %v: %v", entries, err)
	}
	if got := analysisExec(t, d, sandbox, prefix+"test ! -e /workspace/borrowed-git; test ! -e /workspace/shared-git; printf '%s\\n' /workspace/git/*; git rev-parse --absolute-git-dir; git remote get-url origin"); got != "/workspace/git/borrowed\n/workspace/git/private\n/workspace/git/shared\n/workspace/git/private/the8020/dev-core/.git\n/workspace/git/shared/the8020/dev-core\n" {
		t.Fatalf("unexpected grouped Git layout: %q", got)
	}
	if _, err := os.Stat(filepath.Join(m.config.PackagesRoot, ".meta/activation-locks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sandbox startup or ordinary Git created publication locks: %v", err)
	}
	failure, failureErr := m.config.ActivationGateway.Activate(ctx, sandbox.UserID, ActivationOptions{})
	if failureErr != nil || failure.Success || failure.Error != "activation description is required" {
		t.Fatalf("activation result lost its preflight error: %+v: %v", failure, failureErr)
	}
	failedSandbox, err := m.Inspect(sandbox.UserID)
	if err != nil || failedSandbox.LastActivationResult == nil || failedSandbox.LastActivationResult.Error != failure.Error {
		t.Fatalf("stored activation result lost its preflight error: %+v: %v", failedSandbox.LastActivationResult, err)
	}
	record := map[string]any{"full_workflow_qualified": false, "goos": runtime.GOOS, "goarch": runtime.GOARCH,
		"asset_bytes": 4 << 20, "tracked_assets": 4, "package_dentry_cache": 0,
		"activation_owner_replaced": false, "schema_hooks_qualified": false,
		"scan_index_refresh_overlay": true, "external_private_git_metadata_mount": true, "native_gitdir_files": true,
		"activation_preflight_error_retained_in_gateway_and_saved_state": true,
		"go_version": runtime.Version(), "gvisor_sdk": os.Getenv("WORKFLOW_SPARSE_SDK"), "observed_at": time.Now().UTC()}
	var sdkFix map[string]string
	if err := json.Unmarshal([]byte(os.Getenv("WORKFLOW_SPARSE_SDK_FIX")), &sdkFix); err != nil {
		t.Fatal(err)
	}
	record["sdk_setstat_fix"] = sdkFix
	hashes := map[string]string{}
	for _, name := range []string{"analysis/sparse_test.go", "analysis/gofer_probe.go", "analysis/gofer_probe.py", "analysis/native_test.go", "analysis/native_probe.go", "analysis/run.py", "analysis/activation-transaction.patch", "rootless.go", "activation.go", "model.go", "manager.go", "gateway.go", "../../defaults/scripts/activate", "../../defaults/scripts/activate.ts"} {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		hashes[name] = fmt.Sprintf("%x", sha256.Sum256(body))
	}
	record["source_sha256"] = hashes
	var fs unix.Statfs_t
	if err := unix.Statfs(shared, &fs); err != nil {
		t.Fatal(err)
	}
	record["host_statfs_type"] = fmt.Sprintf("0x%x", fs.Type)
	storageStages := map[string]int64{}
	largePrivateFiles := map[string]int64{}
	measureStorage := func(stage string) {
		t.Helper()
		var bytes int64
		if err := filepath.WalkDir(storage, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == filepath.Join(storage, "lower") {
				return filepath.SkipDir
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				if sparse.isSharedGitObject(path, info) {
					return nil
				}
				bytes += info.Size()
				if info.Size() >= 1<<20 {
					relative, _ := filepath.Rel(storage, path)
					largePrivateFiles[relative] = info.Size()
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		storageStages[stage] = bytes
	}
	measureStorage("initialized")
	analysisExec(t, d, sandbox, prefix+"/root/metadata-client gitdir-file")
	record["native_gitdir_file_rename"] = true
	record["native_versions"] = analysisExec(t, d, sandbox, "deno --version; git --version")
	directoryBefore := analysisExec(t, d, sandbox, prefix+`stat -c '%d:%i:%a' .; cat "$(git rev-parse --git-path objects/info/alternates)"`)
	analysisExec(t, d, sandbox, prefix+`deno eval 'Deno.writeTextFileSync("same.txt", "private label\n"); console.log(Deno.readTextFileSync("untouched.txt"))'`)
	directoryAfter, directoryErr := d.Exec(ctx, sandbox.SandboxID, prefix+`stat -c '%d:%i:%a' .; cat "$(git rev-parse --git-path objects/info/alternates)"`)
	if directoryErr != nil || string(directoryAfter) != directoryBefore {
		t.Fatalf("source edit changed parent directory identity or lost Git reference: before=%q after=%q error=%v", directoryBefore, directoryAfter, directoryErr)
	}
	record["parent_identity_and_git_reference_preserved"] = true
	measureStorage("deno_label_edit")
	analysisExec(t, d, sandbox, "/root/metadata-client reject-utime /workspace/packages/the8020/dev-core/assets/0.bin")
	measureStorage("unsupported_metadata_request")
	if storageStages["unsupported_metadata_request"] != storageStages["deno_label_edit"] {
		t.Fatal("failed metadata request copied or froze an untouched asset")
	}
	var state struct {
		GoferPID int `json:"goferPid"`
		Sandbox  struct {
			PID int `json:"pid"`
		} `json:"sandbox"`
	}
	if err := readJSON(filepath.Join(d.config.RuntimeRoot, sandbox.SandboxID+"_sandbox:"+sandbox.SandboxID+".state"), &state); err != nil {
		t.Fatal(err)
	}
	processIO := func() map[string]int64 {
		t.Helper()
		values := map[string]int64{}
		for role, pid := range map[string]int{"sentry": state.Sandbox.PID, "gofer": state.GoferPID} {
			body, err := os.ReadFile(fmt.Sprintf("/proc/%d/io", pid))
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
				fields := strings.Fields(line)
				count, err := strconv.ParseInt(fields[1], 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				values[role+"_"+strings.TrimSuffix(fields[0], ":")] = count
			}
		}
		return values
	}
	ioBefore := processIO()
	started := time.Now()
	previewText := analysisExec(t, d, sandbox, "activate --json --preview")
	record["helper_preview_ms"] = float64(time.Since(started).Microseconds()) / 1000
	ioAfter := processIO()
	for key, count := range ioBefore {
		ioAfter[key] -= count
	}
	record["preview_process_io_delta"] = ioAfter
	if ioAfter["sentry_wchar"] > 1<<20 || ioAfter["sentry_write_bytes"] > 1<<20 {
		t.Fatalf("small preview still writes asset-sized contents: %v", ioAfter)
	}
	var preview ActivationPreview
	if err := json.Unmarshal([]byte(previewText), &preview); err != nil || len(preview.Packages) != 1 || preview.Packages[0].ChangedFiles != 1 {
		t.Fatalf("real helper preview: %s, %v", previewText, err)
	}
	record["helper_preview"] = preview
	measureStorage("helper_preview")
	if storageStages["helper_preview"] > 64<<10 {
		t.Fatalf("preview copied asset object contents: %d bytes, files=%v, io=%v", storageStages["helper_preview"], largePrivateFiles, ioAfter)
	}
	t.Log("PASS real Deno helper -> authenticated endpoint -> command bus -> activation preview on the sparse mount")
	analysisExec(t, d, sandbox, prefix+"git checkout -qb private-work; git add same.txt; git -c user.name=Analysis -c user.email=analysis@example.test commit -qm 'Captured private label'")
	privateCommit := strings.TrimSpace(analysisExec(t, d, sandbox, prefix+"git rev-parse HEAD"))
	sparse.request(t, "capture", "the8020/dev-core/same.txt", "candidate")
	analysisExec(t, d, sandbox, prefix+"printf 'later primary save\\n' >same.txt")
	// Prepare from the filesystem capture, even if the editor saves again before
	// Git runs. Only these captured bytes cross into sandbox-owned Git metadata.
	captured, err := os.Open(filepath.Join(storage, "snapshots/candidate/file"))
	if err != nil {
		t.Fatal(err)
	}
	blob := &boundedBuffer{limit: 256}
	exportErr := d.ExecStream(ctx, sandbox.SandboxID, prefix+"git hash-object -w --stdin", captured, blob)
	closeErr := captured.Close()
	if exportErr != nil || closeErr != nil {
		t.Fatalf("import captured bytes: %v, %v", exportErr, closeErr)
	}
	candidate := strings.TrimSpace(analysisExec(t, d, sandbox, prefix+
		"export GIT_INDEX_FILE=/tmp/sparse-capture-index; git read-tree "+base+
		"; git update-index --add --cacheinfo 100644,"+strings.TrimSpace(blob.String())+",same.txt; "+
		"tree=$(git write-tree); printf 'Captured files\\n' | git -c user.name=Analysis -c user.email=analysis@example.test commit-tree \"$tree\" -p "+base))
	record["capture_precedes_later_edit_and_git_preparation"] = true
	measureStorage("snapshot_git_preparation")
	writeTestFile(t, filepath.Join(shared, "same.txt"), "shared label\n")
	writeTestFile(t, filepath.Join(shared, "untouched.txt"), "shared advance\n")
	if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-am", "Competing shared label"); err != nil {
		t.Fatal(err)
	}
	sharedCommit, err := gitOutput(shared, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got := analysisExec(t, d, sandbox, prefix+"cat untouched.txt"); got != "shared advance\n" {
		t.Fatalf("untouched path stale: %q", got)
	}
	conflict := "/workspace/packages/.conflicts/label"
	resolution := "set -e; cd " + conflict + "; "
	analysisExec(t, d, sandbox, prefix+"git fetch origin; git worktree add --detach --no-checkout "+conflict+" "+candidate+"; cd "+conflict+"; git sparse-checkout set --no-cone /same.txt; git read-tree --reset -u HEAD")
	output, mergeErr := d.Exec(ctx, sandbox.SandboxID, resolution+"git -c user.name=Analysis -c user.email=analysis@example.test -c merge.conflictStyle=diff3 merge --no-edit "+sharedCommit)
	if mergeErr == nil || !strings.Contains(mergeErr.Error(), "exit status 1") {
		t.Fatalf("expected native Git conflict: %q, %v", output, mergeErr)
	}
	markers := analysisExec(t, d, sandbox, resolution+"cat same.txt; git show :1:same.txt; git show :2:same.txt; git show :3:same.txt; test ! -e assets")
	if !strings.Contains(markers, "<<<<<<<") || !strings.Contains(markers, "|||||||") || !strings.HasSuffix(markers, "base\nprivate label\nshared label\n") {
		t.Fatalf("incorrect native markers/stages: %q", markers)
	}
	record["native_conflict"] = markers
	measureStorage("native_conflict")
	if err := d.Kill(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	sandbox, err = m.Start(ctx, sandbox.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if got := analysisExec(t, d, sandbox, resolution+"cat same.txt; git show :1:same.txt; git show :2:same.txt; git show :3:same.txt; test ! -e assets"); got != markers {
		t.Fatalf("unresolved native conflict did not survive recreation: %q", got)
	}
	record["unresolved_worktree_recreation"] = true
	broker, err := platformconsole.New(platformconsole.Config{Authentication: sshProofAuthentication{}, Development: m})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	terminal, err := broker.CreateTerminal(ctx, "development", sandbox.SandboxID, backend.ConsoleOptions{
		Arguments: []string{"/bin/bash", "-l"}, WorkingDir: "/workspace/packages/the8020/dev-core", Terminal: true,
		Environment: []string{"TERM=xterm-256color", "HOME=/root", "PATH=" + developmentPath}, Size: backend.ConsoleSize{Columns: 90, Rows: 27},
	})
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := terminal.Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attachment.Write([]byte("stty -echo; export PS1=''; printf '%s' $$ >/tmp/sparse-tty-before; printf 'TTY-READY\\n'\n")); err != nil {
		t.Fatal(err)
	}
	sequence := retainedUntil(t, attachment, 0, "TTY-READY")
	attachment.Close()
	analysisExec(t, d, sandbox, resolution+"printf 'resolved label\\n' >same.txt; git add same.txt; git -c user.name=Analysis -c user.email=analysis@example.test commit -qm 'Resolve both sides'")
	writeTestFile(t, filepath.Join(shared, "untouched.txt"), "shared during resolution\n")
	if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-am", "Advance during resolution"); err != nil {
		t.Fatal(err)
	}
	sharedCommit, err = gitOutput(shared, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	analysisExec(t, d, sandbox, resolution+"git fetch origin; git -c user.name=Analysis -c user.email=analysis@example.test merge --no-edit "+sharedCommit+"; test ! -e assets")
	if got := analysisExec(t, d, sandbox, resolution+"git show HEAD:untouched.txt; git status --porcelain=v1"); got != "shared during resolution\n" {
		t.Fatalf("retry lost shared content or left dirty Git state: %q", got)
	}
	resolved := strings.TrimSpace(analysisExec(t, d, sandbox, resolution+"git rev-parse HEAD"))
	bundleBytes := analysisExportCommit(t, d, sandbox, shared, resolved, sharedCommit)
	if bundleBytes > 64<<10 {
		t.Fatalf("small activation exported asset history: %d bytes", bundleBytes)
	}
	record["incremental_bundle_bytes"] = bundleBytes
	measureStorage("resolved_and_exported")
	if _, err := gitCommand(ctx, shared, nil, "merge", "--ff-only", resolved); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(shared, "same.txt")); err != nil || string(got) != "resolved label\n" {
		t.Fatalf("resolved source was not published: %q, %v", got, err)
	}
	ack := sparse.request(t, "acknowledge", "the8020/dev-core/same.txt", "candidate")
	if ack["status"] != "retained" || ack["reason"] != "later_content" {
		t.Fatalf("later edit acknowledgement: %v", ack)
	}
	record["acknowledgement"] = ack
	if got := analysisExec(t, d, sandbox, prefix+"cat same.txt untouched.txt"); got != "later primary save\nshared during resolution\n" {
		t.Fatalf("publication lost later/private or live/shared content: %q", got)
	}
	attachment, err = terminal.Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attachment.Write([]byte("printf '%s' $$ >/tmp/sparse-tty-after; printf 'TTY-SURVIVED\\n'\n")); err != nil {
		t.Fatal(err)
	}
	retainedUntil(t, attachment, sequence, "TTY-SURVIVED")
	attachment.Close()
	analysisExec(t, d, sandbox, "cmp /tmp/sparse-tty-before /tmp/sparse-tty-after")
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	t.Log("PASS native sparse Git conflict/resolution, incremental sandbox bundle, later-edit preservation, and retained PTY across publication")
	if err := d.Kill(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	sandbox, err = m.Start(ctx, sandbox.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if got := analysisExec(t, d, sandbox, resolution+"git rev-parse HEAD; cat same.txt; test ! -e assets"); got != resolved+"\nresolved label\n" {
		t.Fatalf("native resolution did not survive recreation: %q", got)
	}
	if got := analysisExec(t, d, sandbox, prefix+"cat same.txt; git rev-parse HEAD"); got != "later primary save\n"+privateCommit+"\n" {
		t.Fatalf("primary work did not survive recreation: %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(storage, "base/the8020/dev-core/same.txt")); err != nil || string(got) != "private label\n" {
		t.Fatalf("next original did not survive: %q, %v", got, err)
	}
	// Re-enter the existing publication owner through the real helper. Only its
	// scan's index refresh is altered; it still deletes conflict files.
	var mutationErr error
	m.driver = analysisPauseDriver{SandboxDriver: sparse, atPause: func() {
		mutationErr = os.WriteFile(filepath.Join(shared, "same.txt"), []byte("competing helper label\n"), 0600)
		if mutationErr == nil {
			_, mutationErr = gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-am", "Compete during helper activation")
		}
	}}
	started = time.Now()
	helperOutput := analysisExec(t, d, sandbox, "activate --json --message 'Sparse helper conflict'; status=$?; printf '\\nhelper_exit=%s\\n' \"$status\"; test \"$status\" -eq 3")
	helperDuration := time.Since(started)
	m.driver = sparse
	if mutationErr != nil {
		t.Fatal(mutationErr)
	}
	if !strings.Contains(helperOutput, `"status":"conflicted"`) || !strings.Contains(helperOutput, `"conflicts":["same.txt"]`) {
		t.Fatalf("canonical helper did not report conflict: %s", helperOutput)
	}
	if got := analysisExec(t, d, sandbox, prefix+"cat same.txt"); got != "later primary save\n" {
		t.Fatalf("helper conflict changed the primary file: %q", got)
	}
	currentConflicts, err := filepath.Glob(filepath.Join(m.config.Root, ".activation-*"))
	if err != nil || len(currentConflicts) != 0 {
		t.Fatalf("unexpected retained current-owner conflict fixture: %v, %v", currentConflicts, err)
	}
	record["current_helper_conflict"] = map[string]any{"output": helperOutput, "milliseconds": float64(helperDuration.Microseconds()) / 1000,
		"native_conflict_files_retained": false, "qualified": false, "timing": "one current-owner failure including runtime exec and helper startup; full temporary validation checkouts remain"}
	reference := analysisExec(t, d, sandbox, prefix+"cat .git")
	analysisExec(t, d, sandbox, prefix+"rm .git")
	if err := d.Kill(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	sandbox, err = m.Start(ctx, sandbox.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if got := analysisExec(t, d, sandbox, prefix+"test ! -e .git; git --git-dir=/workspace/git/private/the8020/dev-core/.git rev-parse HEAD; cat same.txt"); got != privateCommit+"\nlater primary save\n" {
		t.Fatalf("removed Git reference or private history changed after recreation: %q", got)
	}
	analysisExec(t, d, sandbox, prefix+"printf %s "+shellQuote(reference)+" >.git; git rev-parse HEAD")
	if got := analysisExec(t, d, sandbox, resolution+"git rev-parse HEAD"); got != resolved+"\n" {
		t.Fatalf("removing the Git reference lost the resolved worktree: %q", got)
	}
	record["removed_gitdir_reference_survives_recreation"] = true
	measureStorage("current_helper_conflict")
	record["private_logical_bytes_by_stage"] = storageStages
	record["large_private_files"] = largePrivateFiles
	assetsCopied := 0
	var logical, allocated int64
	inodes := map[[2]uint64]bool{}
	if err := filepath.WalkDir(storage, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path == filepath.Join(storage, "lower") {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.Contains(path, "/assets/") {
			assetsCopied++
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			if sparse.isSharedGitObject(path, info) {
				return nil
			}
			st := info.Sys().(*syscall.Stat_t)
			key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
			if !inodes[key] {
				inodes[key] = true
				logical += info.Size()
				allocated += st.Blocks * 512
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if assetsCopied != 0 {
		t.Fatalf("copied %d assets", assetsCopied)
	}
	if len(largePrivateFiles) != 0 || logical > 64<<10 {
		t.Fatalf("native workflow copied asset contents into metadata: %v, %d bytes", largePrivateFiles, logical)
	}
	record["small_edit_storage_check"] = true
	record["materialized_asset_copies"] = assetsCopied
	record["end_private_regular_storage"] = map[string]any{"inodes": len(inodes), "logical_bytes": logical, "allocated_bytes": allocated,
		"directory_allocation_included": false, "scope": "private regular inodes after native workflow; excludes unchanged shared Git inodes and transient host validation checkouts"}
	record["base_commit"] = base
	borrowedRoot := filepath.Join(storage, "borrowed/the8020/dev-core/objects")
	var borrowedFiles, borrowedBytes int64
	if err := filepath.WalkDir(borrowedRoot, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !sparse.isSharedGitObject(name, info) {
			return fmt.Errorf("retained object is not a shared inode: %s", name)
		}
		borrowedFiles++
		borrowedBytes += info.Size()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if borrowedBytes < 4<<20 {
		t.Fatal("retention fixture omitted the asset objects")
	}
	record["borrowed_objects"] = map[string]any{"files": borrowedFiles, "logical_bytes": borrowedBytes, "copied_payload_bytes": 0, "retention": "workspace lifetime"}
	objectPath := "/workspace/git/borrowed/the8020/dev-core/objects/" + base[:2] + "/" + base[2:]
	analysisExec(t, d, sandbox, "deno eval "+shellQuote(`
const source = `+strconv.Quote(objectPath)+`;
Deno.readFileSync(source);
let writable = false;
try { Deno.openSync(source, {write: true}).close(); writable = true; } catch {}
if (writable) throw new Error("borrowed object permits writing");
const target = "/workspace/git/private/borrowed-alias";
let linked = false;
try { Deno.linkSync(source, target); linked = true; } catch {}
if (linked) { Deno.removeSync(target); throw new Error("borrowed object escaped into writable storage"); }
`))
	record["borrowed_objects_read_only_and_no_writable_alias"] = true
	historyWorktree := "/workspace/packages/.conflicts/retained-history"
	history := "set -e; cd " + historyWorktree + "; "
	analysisExec(t, d, sandbox, prefix+"git worktree add --detach --no-checkout "+historyWorktree+" "+privateCommit+"; cd "+historyWorktree+"; git sparse-checkout set --no-cone /same.txt; git read-tree --reset -u HEAD")
	if output, err := d.Exec(ctx, sandbox.SandboxID, history+"git -c user.name=Analysis -c user.email=analysis@example.test -c merge.conflictStyle=diff3 merge --no-edit "+sharedCommit); err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("expected retained-history conflict: %s: %v", output, err)
	}
	historyState := analysisExec(t, d, sandbox, history+"cat same.txt; git ls-files --unmerged; git show :1:same.txt; git show :2:same.txt; git show :3:same.txt")
	assetDigest := analysisExec(t, d, sandbox, prefix+"git cat-file blob "+base+":assets/0.bin | sha256sum")
	// Borrowed history must survive the shared repository dropping its refs and
	// collecting unreachable objects. This changes only this disposable fixture.
	refs, err := gitOutput(shared, "for-each-ref", "--format=%(refname)")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range strings.Fields(refs) {
		if _, err := gitCommand(ctx, shared, nil, "update-ref", "-d", ref); err != nil {
			t.Fatal(err)
		}
	}
	emptyTree, err := gitCommand(ctx, shared, nil, "mktree")
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit-tree", strings.TrimSpace(emptyTree), "-m", "Replacement shared history")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"update-ref", "HEAD", strings.TrimSpace(orphan)}, {"read-tree", "--empty"}, {"reflog", "expire", "--expire=now", "--all"}, {"gc", "--prune=now"}} {
		if _, err := gitCommand(ctx, shared, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitCommand(ctx, shared, nil, "cat-file", "-e", base); err == nil {
		t.Fatal("negative fixture retained the old shared commit")
	}
	if output, err := d.Exec(ctx, sandbox.SandboxID, prefix+"git cat-file -e "+base+"; git fsck --full"); err != nil {
		failure, _ := json.MarshalIndent(map[string]any{"shared_gc_removed_original": true, "private_history_survived": false, "output": string(output), "error": err.Error(), "source_sha256": hashes}, "", "  ")
		if err := os.WriteFile("analysis/sparse-object-retention-failure.json", append(failure, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("shared GC broke native private history: %s: %v", output, err)
	}
	record["borrowed_history_survives_shared_gc"] = true
	if err := os.RemoveAll(shared); err != nil {
		t.Fatal(err)
	}
	if err := d.Kill(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	sandbox, err = m.Start(ctx, sandbox.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if got := analysisExec(t, d, sandbox, history+"cat same.txt; git ls-files --unmerged; git show :1:same.txt; git show :2:same.txt; git show :3:same.txt"); got != historyState {
		t.Fatalf("shared removal/recreation changed the unresolved conflict: %q", got)
	}
	if got := analysisExec(t, d, sandbox, prefix+"git cat-file blob "+base+":assets/0.bin | sha256sum"); got != assetDigest {
		t.Fatalf("shared removal/recreation lost the original asset: %q", got)
	}
	analysisExec(t, d, sandbox, history+"printf 'resolved after shared removal\\n' >same.txt; git add same.txt; git -c user.name=Analysis -c user.email=analysis@example.test commit -qm 'Resolve retained history'; git fsck --full")
	record["borrowed_history_and_native_conflict_survive_shared_removal_and_restart"] = true
	// The permanent read-only root also exposes replacement repositories to
	// ordinary Git fetch without rebinding per-package mounts or private refs.
	writeTestFile(t, filepath.Join(shared, "replacement.txt"), "new shared repository\n")
	for _, args := range [][]string{{"init", "-q"}, {"add", "replacement.txt"}, {"commit", "-qm", "Replacement package"}, {"gc"}} {
		if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), args...); err != nil {
			t.Fatal(err)
		}
	}
	if got := analysisExec(t, d, sandbox, prefix+"cat replacement.txt; git rev-parse HEAD"); got != "new shared repository\n"+privateCommit+"\n" {
		t.Fatalf("replacement froze a shared file or replaced private history: %q", got)
	}
	analysisExec(t, d, sandbox, prefix+"git fetch origin; git fsck --full")
	if got := analysisExec(t, d, sandbox, prefix+"git show FETCH_HEAD:replacement.txt"); got != "new shared repository\n" {
		t.Fatalf("running sandbox could not fetch the replacement repository: %q", got)
	}
	if err := d.Kill(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	sandbox, err = m.Start(ctx, sandbox.UserID)
	if err != nil {
		t.Fatal(err)
	}
	// Retain replacement objects when a file operation needs them, not at startup.
	analysisExec(t, d, sandbox, prefix+"mv replacement.txt renamed-replacement.txt")
	packs, err := filepath.Glob(filepath.Join(shared, ".git/objects/pack/*.pack"))
	if err != nil || len(packs) != 1 {
		t.Fatalf("replacement fixture has no packed objects: %v: %v", packs, err)
	}
	packed, err := os.Stat(filepath.Join(borrowedRoot, "pack", filepath.Base(packs[0])))
	if err != nil || !sparse.isSharedGitObject(filepath.Join(borrowedRoot, "pack", filepath.Base(packs[0])), packed) {
		t.Fatalf("replacement pack was not retained by hardlink: %v", err)
	}
	analysisExec(t, d, sandbox, prefix+"git fetch origin; git fsck --full")
	if got := analysisExec(t, d, sandbox, prefix+"git rev-parse HEAD; git show FETCH_HEAD:replacement.txt"); got != privateCommit+"\nnew shared repository\n" {
		t.Fatalf("fetch from replacement changed private history: %q", got)
	}
	record["replacement_packed_objects_and_native_fetch_preserve_private_history"] = true
	record["passed"] = true
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("analysis/sparse-runtime-results.json", append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("PASS native private Git worktrees/originals/later edits survive real driver recreation without materializing tracked assets; activation publication and schema/hooks remain unqualified")
}
