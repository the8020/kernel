package development

import (
	"bufio"
	"context"
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
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	workspacepackages "the8020/kernel/packages"
)

type workspace struct {
	*RunscDriver
	storage, shared string
	listener        *net.UnixListener
	control         net.Conn
	controlMu       sync.Mutex
}

// Wire metadata shared with the workspace Gofer, without file payloads.
func objectID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) != 40 && len(value) != 64 {
		return "", errors.New("invalid Git object ID length")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", errors.New("invalid Git object ID")
	}
	return value, nil
}

type fileReference struct {
	Source, Package, Blob, Version string
	Mode                           uint32
}

func (d *workspace) fileReferences(ctx context.Context, name string) (map[string]fileReference, error) {
	parts := strings.SplitN(name, "/", 3)
	if !validRelative(name) || filepath.ToSlash(filepath.Clean(name)) != name {
		return nil, errors.New("invalid rename source")
	}
	if len(parts) == 1 {
		refs := map[string]fileReference{}
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
	refs := map[string]fileReference{}
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
		refs[id+"/"+relative] = fileReference{Source: id + "/" + relative, Package: id, Blob: fields[2], Mode: s.Mode,
			Version: fmt.Sprintf("%d:%d:%d:%d:%d:%d:%d:%d:%d", unix.Major(s.Dev), unix.Minor(s.Dev), s.Ino, s.Mode, s.Size, s.Mtim.Sec, s.Mtim.Nsec, s.Ctim.Sec, s.Ctim.Nsec)}
		if len(refs) > 4096 {
			return nil, errors.New("rename exceeds 4,096 files")
		}
	}
	return refs, validate()
}

func (d *workspace) materializeReference(ctx context.Context, ref fileReference) (string, error) {
	if _, err := workspacepackages.ParsePackageID(ref.Package); err != nil {
		return "", err
	}
	if _, err := objectID(ref.Blob); err != nil {
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

func (d *workspace) start(ctx context.Context, start SandboxStart) error {
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
	if err := d.RunscDriver.start(ctx, start); err != nil {
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
func (d *workspace) serveGitInitialization(listener *net.UnixListener, ready chan<- struct{}) {
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
				Reference    fileReference
			}
			var response struct {
				Error      string
				References map[string]fileReference
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

func (d *workspace) initializeGit(ctx context.Context, id string) error {
	validate, release, err := workspacepackages.ObserveSources(ctx, d.shared, []string{id})
	if err != nil {
		return err
	}
	defer release()
	return d.initializeGitOwned(ctx, id, validate)
}

// A publishing caller already holds the source lock; readers validate their
// observed source before installing metadata into the private workspace.
func (d *workspace) initializeGitOwned(ctx context.Context, id string, validate func() error) error {
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
	return gitReference(d.storage, id)
}

// The caller locks publication or validates its source read before using these
// retained objects. Hardlink files without copying or replacing retained names.
// The workspace retains the links for the workspace lifetime. Shared/user
// roots must support hardlinks; cross-filesystem storage fails explicitly.
func (d *workspace) retainGitObjects(ctx context.Context, id string) error {
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

// Install a standard Git directory reference without overwriting private edits
// or resurrecting a reference removed through the filesystem. Confine all
// access to the private upper: package parents may contain user-made symlinks.
func gitReference(storage, id string) error {
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

func (d *workspace) exchange(ctx context.Context, action, path, id string) (map[string]any, error) {
	return d.exchangeControl(ctx, map[string]any{"action": action, "path": path, "id": id})
}

func (d *workspace) exchangeControl(ctx context.Context, request map[string]any) (map[string]any, error) {
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
	return response, nil
}

func (d *RunscDriver) Start(ctx context.Context, start SandboxStart) error {
	storage := start.WorkspaceRoot
	if !filepath.IsAbs(storage) {
		return errors.New("development workspace root must be absolute")
	}
	var legacy struct {
		Packages map[string]any `toml:"packages"`
	}
	if err := readTOML(filepath.Join(filepath.Dir(start.WorkspaceRoot), "runtime", "overlay", "state.toml"), &legacy); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(legacy.Packages) != 0 {
		return errors.New("development sandbox has legacy private edits; activate or export them using the previous kernel before upgrading")
	}
	for _, name := range []string{"lower", "upper", "base", "deleted", "snapshots"} {
		if err := os.MkdirAll(filepath.Join(storage, name), 0700); err != nil {
			return err
		}
	}
	socket := filepath.Join(storage, "control.sock")
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return err
	}
	native := &RunscDriver{config: d.config}
	wrapper := filepath.Join(storage, "runsc")
	script := "#!/bin/sh\nset -eu\nmount --bind " + shellQuote(start.Packages) + " " + shellQuote(filepath.Join(storage, "lower")) + "\nmount -o remount,bind,ro " + shellQuote(filepath.Join(storage, "lower")) + "\nexec " + shellQuote(d.config.RunscPath) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		listener.Close()
		return err
	}
	native.config.RunscPath = wrapper
	sparse := &workspace{RunscDriver: native, storage: storage, shared: start.Packages, listener: listener}
	if err := sparse.start(ctx, start); err != nil {
		listener.Close()
		return err
	}
	d.sandboxes.Store(start.SandboxID, sparse)
	return nil
}

func (m *Manager) workspaceFor(id string) (*workspace, bool) {
	d, ok := m.driver.(*RunscDriver)
	if !ok {
		return nil, false
	}
	value, ok := d.sandboxes.Load(id)
	if !ok {
		return nil, false
	}
	return value.(*workspace), true
}

func (m *Manager) resetWorkspace(sandbox *Sandbox) error {
	if _, err := os.Stat(filepath.Join(m.sandboxRoot(*sandbox), "activation/active.json")); err == nil {
		return errors.New("finish the pending activation before resetting source")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sandbox.LastActivationResult, sandbox.LastActivationStatus = nil, ""
	return os.RemoveAll(filepath.Join(m.sandboxRoot(*sandbox), "workspace"))
}
