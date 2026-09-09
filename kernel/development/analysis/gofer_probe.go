//go:build ignore

// A bounded copy-on-write transport experiment, not a production filesystem.
// Build in a disposable module against the generated upstream gVisor SDK.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/lisafs"
	"gvisor.dev/gvisor/pkg/marshal/primitive"
	"gvisor.dev/gvisor/pkg/seccomp"
	"gvisor.dev/gvisor/runsc/cli/maincli"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/fsgofer/extension"
)

const probeMount = "/workspace/packages"

type probeExtension struct {
	fds [8]int
	fs  *probeFS
}

func (*probeExtension) Name() string { return "workflow-sparse-probe" }
func (*probeExtension) SeccompRules() seccomp.SyscallRules {
	// os.Root uses confined descriptor-relative operations. Keep the stock
	// filter and add only the syscall forms used by this experiment.
	return seccomp.MakeSyscallRules(map[uintptr]seccomp.SyscallRule{
		unix.SYS_NEWFSTATAT: seccomp.MatchAll{},
		unix.SYS_FSTAT:      seccomp.MatchAll{},
		unix.SYS_RENAMEAT:   seccomp.MatchAll{},
		unix.SYS_FCNTL: seccomp.Or{
			seccomp.PerArg{seccomp.AnyValue{}, seccomp.EqualTo(unix.F_DUPFD_CLOEXEC)},
			seccomp.PerArg{seccomp.AnyValue{}, seccomp.EqualTo(unix.F_SETFD)},
		},
	})
}
func (e *probeExtension) SetFlags(f *flag.FlagSet) {
	for i := range e.fds {
		f.IntVar(&e.fds[i], fmt.Sprintf("workflow-probe-fd-%d", i), -1, "Disposable sparse probe root descriptor")
	}
}
func (e *probeExtension) PrepareGofer(ctx extension.GoferPrepareContext) (extension.GoferPrepareResult, error) {
	var result extension.GoferPrepareResult
	if ctx.Spec.Annotations["workflow.probe.lower"] == "" {
		return result, nil
	}
	result.FlagOverrides = map[string]string{}
	roots := [5]*os.Root{}
	names := []string{"lower", "upper", "base", "deleted", "snapshots", "proc"}
	if ctx.Spec.Annotations["workflow.probe.control"] != "" {
		names = append(names, "control")
		if ctx.Spec.Annotations["workflow.probe.git"] != "" {
			names = append(names, "git")
		}
	}
	for i, name := range names {
		if e.fds[i] < 0 {
			// SetupRootFS has already pivoted into the prepared Gofer root.
			// The mount contains private storage roots; only the merged view is served.
			p := path.Join("/root", probeMount, name)
			if name == "proc" {
				p = "/proc/self/fd"
			}
			var fd int
			var err error
			if name == "control" || name == "git" {
				fd, err = unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
				if err == nil {
					err = unix.Connect(fd, &unix.SockaddrUnix{Name: p + ".sock"})
				}
			} else {
				fd, err = unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
			}
			if err != nil {
				return result, fmt.Errorf("open %s root: %w", name, err)
			}
			e.fds[i] = fd // No CLOEXEC: the documented extension hook may re-exec.
		}
		result.FlagOverrides[fmt.Sprintf("workflow-probe-fd-%d", i)] = strconv.Itoa(e.fds[i])
		if i < len(roots) {
			root, err := os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", e.fds[i]))
			if err != nil {
				return result, err
			}
			roots[i] = root
		}
	}
	e.fs = &probeFS{lower: roots[0], upper: roots[1], base: roots[2], deleted: roots[3], snapshots: roots[4], proc: e.fds[5], inodes: map[inodeKey]*probeInode{}}
	if data, err := e.fs.snapshots.ReadFile(".references"); err == nil {
		if err := json.Unmarshal(data, &e.fs.refs); err != nil {
			return result, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	for _, mount := range ctx.Spec.Mounts {
		if strings.HasPrefix(mount.Destination, probeMount+"/") {
			e.fs.submounts = append(e.fs.submounts, strings.TrimPrefix(mount.Destination, probeMount+"/"))
		}
	}
	return result, nil
}
func (e *probeExtension) TryHandleMount(spec *specs.Spec, mount *specs.Mount, mountPath string, readonly bool) (lisafs.ConnectionImpl, lisafs.ConnectionOpts, error) {
	if mount == nil || mount.Destination != probeMount || spec.Annotations["workflow.probe.lower"] == "" {
		return nil, lisafs.ConnectionOpts{}, nil
	}
	if e.fs == nil || readonly {
		return nil, lisafs.ConnectionOpts{}, unix.EINVAL
	}
	if e.fds[6] >= 0 {
		go e.fs.serveControl(os.NewFile(uintptr(e.fds[6]), "publication-control"))
		e.fds[6] = -1
	}
	if e.fds[7] >= 0 {
		e.fs.git = os.NewFile(uintptr(e.fds[7]), "git-initialization")
		e.fds[7] = -1
	}
	return e.fs, lisafs.ConnectionOpts{WalkStatSupported: true}, nil
}

type inodeKey struct{ major, minor, ino uint64 }

func key(st lisafs.Statx) inodeKey { return inodeKey{uint64(st.DevMajor), uint64(st.DevMinor), st.Ino} }

type probeFS struct {
	lower, upper, base, deleted, snapshots *os.Root
	// Mutators and control-FD creation take shared access. Publication takes
	// exclusive access only for its final checks and namespace changes; hashing,
	// Git, validation, and shared-source publication happen outside this lock.
	publicationMu sync.RWMutex
	proc          int
	mu            sync.Mutex
	inodes        map[inodeKey]*probeInode
	submounts     []string
	git           *os.File
	gitMu         sync.Mutex
	refsMu        sync.Mutex
	refs          probeReferences
}

// References contain no payloads. Git's retained immutable objects supply the
// original after shared publication replaces or removes its former pathname.
type probeReference struct {
	Source, Package, Blob, Version string
	Mode                           uint32
}
type probeReferences struct {
	Files, Bases map[string]probeReference
}

func (f *probeFS) reference(name string, base bool) (probeReference, bool) {
	f.refsMu.Lock()
	defer f.refsMu.Unlock()
	refs := f.refs.Files
	if base {
		refs = f.refs.Bases
	}
	r, ok := refs[name]
	return r, ok
}

func (f *probeFS) updateReferences(change func(*probeReferences)) error {
	f.refsMu.Lock()
	defer f.refsMu.Unlock()
	// ponytail: bounded prototype change sets; replace this metadata checkpoint
	// with per-operation records if large pending rename sets become common.
	next := probeReferences{Files: map[string]probeReference{}, Bases: map[string]probeReference{}}
	for name, ref := range f.refs.Files {
		next.Files[name] = ref
	}
	for name, ref := range f.refs.Bases {
		next.Bases[name] = ref
	}
	change(&next)
	if maps.Equal(f.refs.Files, next.Files) && maps.Equal(f.refs.Bases, next.Bases) {
		return nil
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if len(data) > 8<<20 {
		return unix.E2BIG
	}
	if err := f.snapshots.WriteFile(".references-new", data, 0600); err != nil {
		return err
	}
	if err := syncRootPath(f.snapshots, ".references-new"); err != nil {
		return err
	}
	if err := f.snapshots.Rename(".references-new", ".references"); err != nil {
		return err
	}
	f.refs = next
	return syncRootPath(f.snapshots, ".")
}

func probeVersion(s lisafs.Statx) string {
	return fmt.Sprintf("%d:%d:%d:%d:%d:%d:%d:%d:%d", s.DevMajor, s.DevMinor, s.Ino, s.Mode, s.Size, s.Mtime.Sec, s.Mtime.Nsec, s.Ctime.Sec, s.Ctime.Nsec)
}

func (f *probeFS) gitRequest(request any, answer any) error {
	f.gitMu.Lock()
	defer f.gitMu.Unlock()
	if f.git == nil {
		return unix.EOPNOTSUPP
	}
	if err := json.NewEncoder(f.git).Encode(request); err != nil {
		return err
	}
	var response struct {
		Error      string
		References map[string]probeReference
		Path       string
	}
	if err := json.NewDecoder(io.LimitReader(f.git, 8<<20)).Decode(&response); err != nil {
		return err
	}
	if response.Error != "" {
		return fmt.Errorf("rename content: %s: %w", response.Error, unix.EIO)
	}
	switch target := answer.(type) {
	case *map[string]probeReference:
		*target = response.References
	case *string:
		*target = response.Path
	}
	return nil
}

func (f *probeFS) openReference(ref probeReference) (*os.File, error) {
	file, err := f.lower.OpenFile(ref.Source, unix.O_PATH|unix.O_NOFOLLOW, 0)
	if err == nil {
		s, statErr := stat(file)
		if statErr == nil && probeVersion(s) == ref.Version {
			return file, nil
		}
		file.Close()
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOTDIR) {
		return nil, err
	}
	var cached string
	if err := f.gitRequest(struct {
		Action    string
		Reference probeReference
	}{"materialize", ref}, &cached); err != nil {
		return nil, err
	}
	if !safeProbePath(cached) || !strings.HasPrefix(cached, ".reference-data/") {
		return nil, unix.EIO
	}
	return f.snapshots.OpenFile(cached, unix.O_PATH|unix.O_NOFOLLOW, 0)
}

type probeInode struct {
	mu      sync.Mutex // Only this file's copy-up serializes its operations.
	file    *os.File
	private bool
	keys    []inodeKey
	refs    int
	logical inodeKey
}

func (*probeFS) MaxMessageSize() uint32 { return lisafs.MaxMessageSize() }
func (*probeFS) SupportedMessages() []lisafs.MID {
	return []lisafs.MID{lisafs.Mount, lisafs.Channel, lisafs.FStat, lisafs.Walk,
		lisafs.WalkStat, lisafs.OpenAt, lisafs.Close, lisafs.FSync, lisafs.PRead,
		lisafs.PWrite, lisafs.Getdents64, lisafs.FStatFS, lisafs.ReadLinkAt,
		lisafs.OpenCreateAt, lisafs.MkdirAt, lisafs.UnlinkAt, lisafs.RenameAt,
		lisafs.SetStat, lisafs.LinkAt, lisafs.SymlinkAt}
}

// Directory tombstones are separate from per-file deletion markers: the latter
// must retain every child's original for activation. Flat keys allow nested
// removals without replacing a parent marker or reserving source filenames.
func directoryMarker(name string) string {
	return fmt.Sprintf(".directories/%x", sha256.Sum256([]byte(name)))
}
func directoryHead(name string) string {
	return ".directory-heads/" + path.Base(directoryMarker(name))
}
func (f *probeFS) writeDirectoryMarker(name string) error {
	if err := f.snapshots.MkdirAll(".directories", 0700); err != nil {
		return err
	}
	temporary := fmt.Sprintf(".directory-%d", time.Now().UnixNano())
	file, err := f.snapshots.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.snapshots.Remove(temporary)
	_, writeErr := file.WriteString(name)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := f.snapshots.Rename(temporary, directoryMarker(name)); err != nil {
		return err
	}
	return syncRootPath(f.snapshots, ".directories")
}

// Replacing the record's inode distinguishes later directory operations from a
// captured removal, including create/remove cycles. Captures retain a hardlink,
// so that identity cannot be recycled while an activation still references it.
func (f *probeFS) touchDirectoryMarkers(name string) error {
	for current := name; current != "."; current = path.Dir(current) {
		if _, err := f.snapshots.Lstat(directoryMarker(current)); err == nil {
			if err := f.writeDirectoryMarker(current); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
func (f *probeFS) hiddenDirectory(name string) (bool, error) {
	for current := name; current != "."; current = path.Dir(current) {
		if _, err := f.snapshots.Lstat(directoryMarker(current)); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func (f *probeFS) lookup(name string) (*os.File, bool, error) {
	hidden, err := f.hiddenDirectory(name)
	if err != nil {
		return nil, false, err
	}
	file, err := f.upper.OpenFile(name, unix.O_PATH|unix.O_NOFOLLOW, 0)
	if err == nil {
		s, err := stat(file)
		if err != nil {
			file.Close()
			return nil, false, err
		}
		if s.Mode&unix.S_IFMT == unix.S_IFDIR && !hidden {
			// Upper parents only hold private children; they are not directory
			// edits. Keep the lower directory's identity and metadata so creating
			// a private child cannot replace its parent or detach nested mounts.
			lower, err := f.lower.OpenFile(name, unix.O_PATH|unix.O_NOFOLLOW, 0)
			if err == nil {
				s, err := stat(lower)
				if err == nil && s.Mode&unix.S_IFMT == unix.S_IFDIR {
					file.Close()
					return lower, false, nil
				}
				lower.Close()
				if err != nil {
					file.Close()
					return nil, false, err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				file.Close()
				return nil, false, err
			}
		}
		return file, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	if ref, ok := f.reference(name, false); ok {
		file, err := f.openReference(ref)
		return file, false, err
	}
	if hidden {
		return nil, false, unix.ENOENT
	}
	if marker, err := f.deleted.Lstat(name); err == nil {
		// Directories here only contain child deletion markers. Removing one
		// nested file must not hide its parent or the parent's other children.
		if !marker.IsDir() {
			return nil, false, unix.ENOENT
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	file, err = f.lower.OpenFile(name, unix.O_PATH|unix.O_NOFOLLOW, 0)
	parts := strings.Split(name, "/")
	if err == nil && f.git != nil && len(parts) == 3 && parts[2] == ".git" {
		file.Close()
		// ponytail: serialize first Git access per sandbox; split by package
		// if simultaneous discovery becomes costly. Ordinary file I/O proceeds.
		f.gitMu.Lock()
		defer f.gitMu.Unlock()
		if _, err := f.upper.Lstat(name); errors.Is(err, os.ErrNotExist) {
			if err := json.NewEncoder(f.git).Encode(path.Dir(name)); err != nil {
				return nil, false, err
			}
			var message string
			if err := json.NewDecoder(io.LimitReader(f.git, 4096)).Decode(&message); err != nil {
				return nil, false, err
			}
			if message != "" {
				return nil, false, fmt.Errorf("initialize private Git: %s", message)
			}
		} else if err != nil {
			return nil, false, err
		}
		file, err = f.upper.OpenFile(name, unix.O_PATH|unix.O_NOFOLLOW, 0)
		return file, true, err
	}
	return file, false, err
}
func (f *probeFS) control(c *lisafs.Connection, node *lisafs.Node, name string) (*probeControl, lisafs.Statx, error) {
	file, private, err := f.lookup(name)
	if err != nil {
		return nil, lisafs.Statx{}, err
	}
	st, err := stat(file)
	if err != nil {
		file.Close()
		return nil, st, err
	}
	k := key(st)
	f.mu.Lock()
	i := f.inodes[k]
	if i == nil {
		i = &probeInode{file: file, private: private, logical: k, keys: []inodeKey{k}}
		f.inodes[k] = i
	} else {
		file.Close()
	}
	i.refs++
	f.mu.Unlock()
	fd := &probeControl{fs: f, inode: i}
	fd.ControlFD.Init(c, node, linux.FileMode(st.Mode), fd)
	st, err = fd.Stat()
	return fd, st, err
}
func (f *probeFS) Mount(c *lisafs.Connection, n *lisafs.Node) (*lisafs.ControlFD, lisafs.Statx, int, error) {
	f.publicationMu.RLock()
	defer f.publicationMu.RUnlock()
	n.IncRef()
	fd, st, err := f.control(c, n, ".")
	if err != nil {
		n.DecRef(nil)
		return nil, st, -1, err
	}
	return fd.FD(), st, -1, nil // Never donate a directory that bypasses copy-up.
}
func (f *probeFS) reopen(file *os.File, flags int) (*os.File, error) {
	fd, err := unix.Openat(f.proc, strconv.Itoa(int(file.Fd())), flags|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), file.Name()), nil
}
func stat(file *os.File) (lisafs.Statx, error) {
	var s unix.Statx_t
	if err := unix.Statx(int(file.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_BASIC_STATS, &s); err != nil {
		return lisafs.Statx{}, err
	}
	return lisafs.Statx{Mask: s.Mask, Mode: s.Mode, Ino: s.Ino, DevMajor: s.Dev_major, DevMinor: s.Dev_minor,
		Size: s.Size, Blocks: s.Blocks, Blksize: s.Blksize, UID: s.Uid, GID: s.Gid, Nlink: s.Nlink,
		Atime: lisafs.StatxTimestamp{Sec: s.Atime.Sec, Nsec: s.Atime.Nsec},
		Mtime: lisafs.StatxTimestamp{Sec: s.Mtime.Sec, Nsec: s.Mtime.Nsec},
		Ctime: lisafs.StatxTimestamp{Sec: s.Ctime.Sec, Nsec: s.Ctime.Nsec}}, nil
}

type probeControl struct {
	lisafs.ControlFD
	unsupportedControl
	fs    *probeFS
	inode *probeInode
}

func (fd *probeControl) FD() *lisafs.ControlFD { return &fd.ControlFD }
func (fd *probeControl) name() string {
	p := strings.TrimPrefix(fd.Node().FilePath(), probeMount)
	if p == "" {
		return "."
	}
	return strings.TrimPrefix(p, "/")
}
func (fd *probeControl) Close() {
	f, i := fd.fs, fd.inode
	f.mu.Lock()
	defer f.mu.Unlock()
	i.refs--
	if i.refs == 0 {
		for _, k := range i.keys {
			delete(f.inodes, k)
		}
		i.file.Close()
	}
}
func (fd *probeControl) Stat() (lisafs.Statx, error) {
	i := fd.inode
	i.mu.Lock()
	defer i.mu.Unlock()
	s, err := stat(i.file)
	s.Ino, s.DevMajor, s.DevMinor = i.logical.ino, uint32(i.logical.major), uint32(i.logical.minor)
	return s, err
}
func (fd *probeControl) Walk(name string) (*lisafs.ControlFD, lisafs.Statx, error) {
	fd.fs.publicationMu.RLock()
	defer fd.fs.publicationMu.RUnlock()
	return fd.walkFD(name)
}
func (fd *probeControl) walkFD(name string) (*lisafs.ControlFD, lisafs.Statx, error) {
	child, s, err := fd.walk(name)
	if err != nil {
		return nil, s, err
	}
	return child.FD(), s, nil
}
func (fd *probeControl) walk(name string) (*probeControl, lisafs.Statx, error) {
	var n *lisafs.Node
	fd.Node().WithChildrenMu(func() {
		n = fd.Node().LookupChildLocked(name)
		if n == nil {
			n = new(lisafs.Node)
			n.InitLocked(name, fd.Node())
		} else {
			n.IncRef()
		}
	})
	child, s, err := fd.fs.control(fd.Conn(), n, path.Join(fd.name(), name))
	if err != nil {
		n.DecRef(nil)
		return nil, s, err
	}
	return child, s, nil
}
func (fd *probeControl) WalkStat(names lisafs.StringArray, record func(lisafs.Statx)) error {
	fd.fs.publicationMu.RLock()
	defer fd.fs.publicationMu.RUnlock()
	p := fd.name()
	for _, name := range names {
		p = path.Join(p, name)
		file, _, err := fd.fs.lookup(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		s, err := stat(file)
		file.Close()
		if err != nil {
			return err
		}
		fd.fs.mu.Lock()
		if i := fd.fs.inodes[key(s)]; i != nil {
			s.Ino, s.DevMajor, s.DevMinor = i.logical.ino, uint32(i.logical.major), uint32(i.logical.minor)
		}
		fd.fs.mu.Unlock()
		record(s)
		if s.Mode&unix.S_IFMT == unix.S_IFLNK {
			break
		}
	}
	return nil
}
func (fd *probeControl) copyUp() error {
	i, f, name := fd.inode, fd.fs, fd.name()
	if i.private {
		return nil
	}
	s, err := stat(i.file)
	if err != nil {
		return err
	}
	if s.Mode&unix.S_IFMT != unix.S_IFREG && s.Mode&unix.S_IFMT != unix.S_IFLNK {
		return unix.EOPNOTSUPP
	}
	aliases := []string{}
	if s.Nlink > 1 {
		aliases, err = f.lowerAliases(name, key(s))
		if err != nil {
			return err
		}
	}
	// Refuse a stale path instead of copying a different inode. Detached lower
	// handles still need a separate qualification case.
	current, _, err := f.lookup(name)
	if err != nil {
		return err
	}
	now, err := stat(current)
	current.Close()
	if err != nil {
		return err
	}
	if key(now) != key(s) {
		return unix.ESTALE
	}
	if _, renamed := f.reference(name, false); !renamed {
		if err := f.saveOriginal(name, i.file, s); err != nil {
			return err
		}
	}
	if err := f.copyFile(f.upper, name, i.file, s); err != nil {
		return err
	}
	for _, alias := range aliases {
		if err := f.upper.MkdirAll(path.Dir(alias), 0700); err != nil {
			return err
		}
		if err := f.base.MkdirAll(path.Dir(alias), 0700); err != nil {
			return err
		}
		if err := f.captureOriginal(alias); err != nil {
			return err
		}
		if err := f.upper.Link(name, alias); err != nil {
			return err
		}
	}
	file, err := f.upper.OpenFile(name, unix.O_PATH|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	st, err := stat(file)
	if err != nil {
		file.Close()
		return err
	}
	i.file.Close()
	i.file, i.private = file, true
	f.mu.Lock()
	i.keys = append(i.keys, key(st))
	f.inodes[key(st)] = i
	f.mu.Unlock()
	return nil
}

func (f *probeFS) lowerAliases(name string, wanted inodeKey) ([]string, error) {
	var aliases []string
	visited := 0
	err := fs.WalkDir(f.lower.FS(), ".", func(candidate string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		for _, mount := range f.submounts {
			if candidate == mount {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		if entry.IsDir() {
			visible, _, err := f.lookup(candidate)
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) {
				return fs.SkipDir
			}
			if err != nil {
				return err
			}
			s, err := stat(visible)
			visible.Close()
			if err != nil {
				return err
			}
			if s.Mode&unix.S_IFMT != unix.S_IFDIR {
				return fs.SkipDir
			}
		}
		visited++
		// ponytail: only multiply-linked lower files need this bounded metadata
		// walk. Measure this path before replacing it with an alias index.
		if visited > 100000 {
			return unix.E2BIG
		}
		if candidate == name || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		s := info.Sys().(*syscall.Stat_t)
		k := inodeKey{uint64(unix.Major(uint64(s.Dev))), uint64(unix.Minor(uint64(s.Dev))), s.Ino}
		if k != wanted {
			return nil
		}
		file, private, err := f.lookup(candidate)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		file.Close()
		if !private {
			aliases = append(aliases, candidate)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	f.refsMu.Lock()
	var referenced []string
	for candidate := range f.refs.Files {
		if candidate != name {
			referenced = append(referenced, candidate)
		}
	}
	f.refsMu.Unlock()
	for _, candidate := range referenced {
		file, private, err := f.lookup(candidate)
		if err != nil {
			return nil, err
		}
		s, err := stat(file)
		file.Close()
		if err != nil {
			return nil, err
		}
		if !private && key(s) == wanted && !slices.Contains(aliases, candidate) {
			aliases = append(aliases, candidate)
		}
	}
	return aliases, err
}

func (fd *probeControl) Open(flags uint32) (*lisafs.OpenFD, int, error) {
	fd.fs.publicationMu.RLock()
	defer fd.fs.publicationMu.RUnlock()
	return fd.open(flags)
}
func (fd *probeControl) open(flags uint32) (*lisafs.OpenFD, int, error) {
	i := fd.inode
	i.mu.Lock()
	defer i.mu.Unlock()
	if flags&unix.O_ACCMODE != unix.O_RDONLY || flags&unix.O_TRUNC != 0 {
		if err := fd.copyUp(); err != nil {
			return nil, -1, err
		}
	}
	file, err := fd.fs.reopen(i.file, int(flags)&^(unix.O_NOFOLLOW|unix.O_CREAT|unix.O_EXCL))
	if err != nil {
		return nil, -1, err
	}
	opened := &probeOpen{file: file, control: fd}
	opened.OpenFD.Init(fd.FD(), flags, opened)
	donate := -1
	if fd.IsRegular() {
		donate, err = unix.Dup(int(file.Fd()))
	}
	return opened.FD(), donate, err
}

// A copy is linked into its final name only after its complete contents are
// synced. Originals and upper installation still need joint crash recovery.
func (f *probeFS) copyFile(root *os.Root, name string, source *os.File, s lisafs.Statx) error {
	dir := path.Dir(name)
	if err := root.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if s.Mode&unix.S_IFMT == unix.S_IFLNK {
		target, err := readSymlink(source)
		if err != nil {
			return err
		}
		if err := root.Symlink(target, name); err != nil {
			return err
		}
		return syncRootPath(root, dir)
	}
	if s.Mode&unix.S_IFMT != unix.S_IFREG {
		return unix.EOPNOTSUPP
	}
	input, err := f.reopen(source, unix.O_RDONLY)
	if err != nil {
		return err
	}
	defer input.Close()
	// The temporary lives under the separately mounted originals root, never
	// in the application namespace. Both private roots share one filesystem.
	tmp := fmt.Sprintf(".copy-%d", time.Now().UnixNano())
	out, err := f.base.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(s.Mode&0777))
	if err != nil {
		return err
	}
	defer f.base.Remove(tmp)
	_, copyErr := io.Copy(struct{ io.Writer }{out}, struct{ io.Reader }{input})
	if err := errors.Join(copyErr, out.Sync(), out.Close()); err != nil {
		return err
	}
	from, err := f.base.Open(".")
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer to.Close()
	if err := unix.Linkat(int(from.Fd()), tmp, int(to.Fd()), path.Base(name), 0); err != nil {
		return err
	}
	return to.Sync()
}
func (f *probeFS) saveOriginal(name string, source *os.File, s lisafs.Statx) error {
	if _, ok := f.reference(name, true); ok {
		return nil
	}
	if _, err := f.base.Lstat(name); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return f.copyFile(f.base, name, source, s)
}
func (f *probeFS) captureOriginal(name string) error {
	if _, ok := f.reference(name, false); ok {
		return nil
	}
	file, private, err := f.lookup(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil // An absent original is represented by no base file.
	}
	if err != nil {
		return err
	}
	defer file.Close()
	if private {
		return nil
	}
	s, err := stat(file)
	if err != nil {
		return err
	}
	return f.saveOriginal(name, file, s)
}
func (fd *probeControl) createParent(name string) (string, error) {
	p := path.Join(fd.name(), name)
	if file, _, err := fd.fs.lookup(p); err == nil {
		file.Close()
		return "", unix.EEXIST
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return p, fd.fs.upper.MkdirAll(fd.name(), 0700)
}
func (fd *probeControl) OpenCreate(mode linux.FileMode, uid lisafs.UID, gid lisafs.GID, name string, flags uint32) (*lisafs.ControlFD, lisafs.Statx, *lisafs.OpenFD, int, error) {
	fd.fs.publicationMu.RLock()
	defer fd.fs.publicationMu.RUnlock()
	p, err := fd.createParent(name)
	if err != nil {
		return nil, lisafs.Statx{}, nil, -1, err
	}
	file, err := fd.fs.upper.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, os.FileMode(mode&0777))
	if err != nil {
		return nil, lisafs.Statx{}, nil, -1, err
	}
	err = errors.Join(unix.Fchownat(int(file.Fd()), "", int(uid), int(gid), unix.AT_EMPTY_PATH), file.Close())
	if err != nil {
		fd.fs.upper.Remove(p)
		return nil, lisafs.Statx{}, nil, -1, err
	}
	child, s, err := fd.walk(name)
	if err != nil {
		fd.fs.upper.Remove(p)
		return nil, s, nil, -1, err
	}
	opened, donated, err := child.open(flags)
	if err != nil {
		child.DecRef(nil)
		fd.fs.upper.Remove(p)
		return nil, s, nil, -1, err
	}
	return child.FD(), s, opened, donated, nil
}
func (fd *probeControl) Mkdir(mode linux.FileMode, uid lisafs.UID, gid lisafs.GID, name string) (*lisafs.ControlFD, lisafs.Statx, error) {
	fd.fs.publicationMu.RLock()
	defer fd.fs.publicationMu.RUnlock()
	p, err := fd.createParent(name)
	if err != nil {
		return nil, lisafs.Statx{}, err
	}
	if err := fd.fs.upper.Mkdir(p, os.FileMode(mode&0777)); err != nil {
		return nil, lisafs.Statx{}, err
	}
	if err := fd.fs.upper.Chown(p, int(uid), int(gid)); err != nil {
		fd.fs.upper.Remove(p)
		return nil, lisafs.Statx{}, err
	}
	if err := fd.fs.touchDirectoryMarkers(p); err != nil {
		return nil, lisafs.Statx{}, err
	}
	return fd.walkFD(name)
}
func (fd *probeControl) SetStat(s lisafs.SetStatReq) (uint32, error) {
	fd.fs.publicationMu.RLock()
	defer fd.fs.publicationMu.RUnlock()
	if fd.IsSymlink() {
		return s.Mask, unix.EOPNOTSUPP
	}
	// A wholly unsupported request must not freeze a shared file or copy its
	// contents. Git's existing-object metadata refresh reaches this path.
	if s.Mask&uint32(unix.STATX_MODE|unix.STATX_SIZE) == 0 {
		return s.Mask, unix.EOPNOTSUPP
	}
	i := fd.inode
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := fd.copyUp(); err != nil {
		return s.Mask, err
	}
	flags := unix.O_RDONLY
	if s.Mask&unix.STATX_SIZE != 0 {
		flags = unix.O_WRONLY
	}
	file, err := fd.fs.reopen(i.file, flags)
	if err != nil {
		return s.Mask, err
	}
	defer file.Close()
	failed := s.Mask & ^uint32(unix.STATX_MODE|unix.STATX_SIZE)
	var failure error
	if failed != 0 {
		failure = unix.EOPNOTSUPP
	}
	if s.Mask&unix.STATX_MODE != 0 {
		if err := unix.Fchmod(int(file.Fd()), uint32(s.Mode&07777)); err != nil {
			failed |= unix.STATX_MODE
			failure = err
		}
	}
	if s.Mask&unix.STATX_SIZE != 0 {
		if err := file.Truncate(int64(s.Size)); err != nil {
			failed |= unix.STATX_SIZE
			failure = err
		}
	}
	return failed, failure
}
func (fd *probeControl) Link(dir lisafs.ControlFDImpl, name string) (*lisafs.ControlFD, lisafs.Statx, error) {
	fd.fs.publicationMu.RLock()
	defer fd.fs.publicationMu.RUnlock()
	to := dir.(*probeControl)
	p, err := to.createParent(name)
	if err != nil {
		return nil, lisafs.Statx{}, err
	}
	i := fd.inode
	i.mu.Lock()
	err = fd.copyUp()
	i.mu.Unlock()
	if err != nil {
		return nil, lisafs.Statx{}, err
	}
	if err := fd.fs.upper.Link(fd.name(), p); err != nil {
		return nil, lisafs.Statx{}, err
	}
	return to.walkFD(name)
}
func (fd *probeControl) Symlink(name, target string, uid lisafs.UID, gid lisafs.GID) (*lisafs.ControlFD, lisafs.Statx, error) {
	fd.fs.publicationMu.RLock()
	defer fd.fs.publicationMu.RUnlock()
	p, err := fd.createParent(name)
	if err != nil {
		return nil, lisafs.Statx{}, err
	}
	if err := fd.fs.upper.Symlink(target, p); err != nil {
		return nil, lisafs.Statx{}, err
	}
	if err := fd.fs.upper.Lchown(p, int(uid), int(gid)); err != nil {
		fd.fs.upper.Remove(p)
		return nil, lisafs.Statx{}, err
	}
	return fd.walkFD(name)
}
func (f *probeFS) hideLower(name string) error {
	// A captured new file can be published after its private name is removed.
	// Retain that absence, but do not accumulate markers for ordinary temp files
	// that were created and removed without an original or activation capture.
	_, needed := f.reference(name, true)
	for _, location := range []struct {
		root *os.Root
		path string
	}{{f.lower, name}, {f.base, name}, {f.snapshots, ".pending/" + name}} {
		if _, err := location.root.Lstat(location.path); err == nil {
			needed = true
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if !needed {
		return nil
	}
	if err := f.deleted.MkdirAll(path.Dir(name), 0700); err != nil {
		return err
	}
	marker, err := f.deleted.OpenFile(name, os.O_CREATE|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	return errors.Join(marker.Sync(), marker.Close())
}
func (fd *probeControl) Unlink(name string, flags uint32) error {
	fd.fs.publicationMu.RLock()
	defer fd.fs.publicationMu.RUnlock()
	p, f := path.Join(fd.name(), name), fd.fs
	file, private, err := f.lookup(p)
	if err != nil {
		return err
	}
	s, err := stat(file)
	file.Close()
	if err != nil {
		return err
	}
	if flags == unix.AT_REMOVEDIR {
		if s.Mode&unix.S_IFMT != unix.S_IFDIR {
			return unix.ENOTDIR
		}
		names, err := f.directoryNames(p)
		if err != nil {
			return err
		}
		for _, name := range names {
			child, _, err := f.lookup(path.Join(p, name))
			if err == nil {
				child.Close()
				return unix.ENOTEMPTY
			}
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if private {
			if err := f.upper.Remove(p); err != nil {
				return err
			}
			return f.touchDirectoryMarkers(p)
		}
		marker := directoryMarker(p)
		if err := f.writeDirectoryMarker(p); err != nil {
			return err
		}
		// The SDK owns the directory and parent during this operation. Its upper
		// may only be a structural parent left after removing private children.
		// ponytail: marker/upper changes still need joint host-crash recovery.
		if err := f.upper.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.Join(err, f.snapshots.Remove(marker), syncRootPath(f.snapshots, ".directories"))
		}
		return nil
	}
	if flags != 0 {
		return unix.EINVAL
	}
	if s.Mode&unix.S_IFMT == unix.S_IFDIR {
		return unix.EISDIR
	}
	if err := f.captureOriginal(p); err != nil {
		return err
	}
	if err := f.hideLower(p); err != nil {
		return err
	}
	if _, ok := f.reference(p, false); ok {
		if err := f.updateReferences(func(refs *probeReferences) { delete(refs.Files, p) }); err != nil {
			return err
		}
	}
	if private {
		return f.upper.Remove(p)
	}
	return nil
}

// Both files and directories become ordinary old-path deletions/new-path
// entries. The SDK excludes operations in the renamed subtree; publication's
// final section also excludes capture/acknowledgement until the batch is visible.
func (fd *probeControl) RenameAt(oldName string, dir lisafs.ControlFDImpl, newName string) (returnErr error) {
	to, f := dir.(*probeControl), fd.fs
	oldPath, newPath := path.Join(fd.name(), oldName), path.Join(to.name(), newName)
	f.publicationMu.RLock()
	locked := false
	defer func() {
		if locked {
			f.publicationMu.Unlock()
		} else {
			f.publicationMu.RUnlock()
		}
	}()
	file, sourcePrivate, err := f.lookup(oldPath)
	if err != nil {
		return err
	}
	sourceStat, err := stat(file)
	file.Close()
	if err != nil {
		return err
	}
	if oldPath == newPath {
		return nil
	}
	isDir := sourceStat.Mode&unix.S_IFMT == unix.S_IFDIR
	if isDir && strings.HasPrefix(newPath, oldPath+"/") {
		return unix.EINVAL
	}
	for _, mount := range f.submounts {
		if mount == oldPath || strings.HasPrefix(mount, oldPath+"/") || mount == newPath || strings.HasPrefix(mount, newPath+"/") {
			return unix.EBUSY
		}
	}
	var targetOriginal *probeReference
	if target, private, err := f.lookup(newPath); err == nil {
		targetStat, err := stat(target)
		target.Close()
		if err != nil {
			return err
		}
		if key(sourceStat) == key(targetStat) {
			return nil
		}
		targetDir := targetStat.Mode&unix.S_IFMT == unix.S_IFDIR
		if !isDir && targetDir {
			return unix.EISDIR
		}
		if isDir && !targetDir {
			return unix.ENOTDIR
		}
		if targetDir {
			names, err := f.directoryNames(newPath)
			if err != nil {
				return err
			}
			for _, name := range names {
				child, _, err := f.lookup(path.Join(newPath, name))
				if err == nil {
					child.Close()
					return unix.ENOTEMPTY
				}
				if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
		} else if _, referenced := f.reference(newPath, false); !private && !referenced {
			if sourcePrivate {
				// An editor's private temporary replacing shared text is already
				// a content edit. Reuse ordinary original capture without taking
				// source-publication ownership during an activation hook.
				if err := f.captureOriginal(newPath); err != nil {
					return err
				}
			} else {
				var originals map[string]probeReference
				if err := f.gitRequest(struct{ Action, Path string }{"references", newPath}, &originals); err != nil {
					return err
				}
				ref, ok := originals[newPath]
				if !ok || ref.Version != probeVersion(targetStat) {
					return unix.EBUSY
				}
				targetOriginal = &ref
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	type movedEntry struct {
		old, new string
		private  bool
		stat     lisafs.Statx
		ref      *probeReference
	}
	var entries []movedEntry
	needReferences := false
	var visit func(string) error
	visit = func(name string) error {
		file, private, err := f.lookup(name)
		if err != nil {
			return err
		}
		s, err := stat(file)
		file.Close()
		if err != nil {
			return err
		}
		entry := movedEntry{old: name, new: newPath + strings.TrimPrefix(name, oldPath), private: private, stat: s}
		switch s.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
		case unix.S_IFREG, unix.S_IFLNK:
			if !private {
				if ref, ok := f.reference(name, false); ok {
					entry.ref = &ref
				} else {
					needReferences = true
				}
			}
		default:
			return unix.EOPNOTSUPP
		}
		entries = append(entries, entry)
		if len(entries) > 4096 {
			return unix.E2BIG
		}
		if s.Mode&unix.S_IFMT == unix.S_IFDIR {
			names, err := f.directoryNames(name)
			if err != nil {
				return err
			}
			for _, child := range names {
				if err := visit(path.Join(name, child)); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(oldPath); err != nil {
		return err
	}
	if needReferences {
		var refs map[string]probeReference
		if err := f.gitRequest(struct{ Action, Path string }{"references", oldPath}, &refs); err != nil {
			return err
		}
		for i := range entries {
			entry := &entries[i]
			if entry.private || entry.ref != nil || entry.stat.Mode&unix.S_IFMT == unix.S_IFDIR {
				continue
			}
			ref, ok := refs[entry.old]
			if !ok || ref.Version != probeVersion(entry.stat) {
				return unix.EBUSY
			}
			entry.ref = &ref
		}
	}
	// No Git, hashing or payload copies occur in the exclusive section.
	f.publicationMu.RUnlock()
	f.publicationMu.Lock()
	locked = true
	for _, entry := range entries {
		if ref, exists := f.reference(entry.old, false); exists && !entry.private {
			if entry.ref == nil || ref != *entry.ref {
				return unix.EBUSY
			}
			continue
		}
		current, private, err := f.lookup(entry.old)
		if err != nil {
			return err
		}
		s, err := stat(current)
		current.Close()
		if err != nil {
			return err
		}
		if private != entry.private || key(s) != key(entry.stat) || !private && !sameVersion(s, entry.stat) {
			return unix.EBUSY
		}
	}
	var undo []func() error
	complete := false
	defer func() {
		if !complete {
			for i := len(undo) - 1; i >= 0; i-- {
				returnErr = errors.Join(returnErr, undo[i]())
			}
		}
	}()
	for _, entry := range entries {
		if entry.stat.Mode&unix.S_IFMT == unix.S_IFDIR {
			if err := f.upper.MkdirAll(entry.old, os.FileMode(entry.stat.Mode&0777)); err != nil {
				return err
			}
			continue
		}
		_, previous := f.deleted.Lstat(entry.old)
		if err := f.hideLower(entry.old); err != nil {
			return err
		}
		if errors.Is(previous, os.ErrNotExist) {
			name := entry.old
			undo = append(undo, func() error {
				err := f.deleted.Remove(name)
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			})
		}
	}
	if isDir {
		marker := directoryMarker(oldPath)
		backup := fmt.Sprintf(".rename-marker-%d", time.Now().UnixNano())
		if err := f.snapshots.Link(marker, backup); err == nil {
			defer func() {
				if complete {
					f.snapshots.Remove(backup)
				}
			}()
			undo = append(undo, func() error { return f.snapshots.Rename(backup, marker) })
		} else if errors.Is(err, os.ErrNotExist) {
			undo = append(undo, func() error { return f.snapshots.Remove(marker) })
		} else {
			return err
		}
		if err := f.writeDirectoryMarker(oldPath); err != nil {
			return err
		}
	}
	if err := f.upper.MkdirAll(to.name(), 0700); err != nil {
		return err
	}
	// Move an existing private target aside so an ordinary I/O error can undo
	// the operation without losing its contents or its retained descriptors.
	backup := fmt.Sprintf(".rename-target-%d", time.Now().UnixNano())
	moveTarget := func(from *os.Root, a string, to *os.Root, b string) error {
		parent, err := from.Open(path.Dir(a))
		if err != nil {
			return err
		}
		defer parent.Close()
		dest, err := to.Open(path.Dir(b))
		if err != nil {
			return err
		}
		defer dest.Close()
		return unix.Renameat(int(parent.Fd()), path.Base(a), int(dest.Fd()), path.Base(b))
	}
	if _, err := f.upper.Lstat(newPath); err == nil {
		if err := moveTarget(f.upper, newPath, f.snapshots, backup); err != nil {
			return err
		}
		defer func() {
			if complete {
				f.snapshots.RemoveAll(backup)
			}
		}()
		undo = append(undo, func() error { return moveTarget(f.snapshots, backup, f.upper, newPath) })
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if isDir || entries[0].private {
		if err := f.upper.Rename(oldPath, newPath); err != nil {
			return err
		}
		undo = append(undo, func() error { return f.upper.Rename(newPath, oldPath) })
	}
	f.refsMu.Lock()
	previousReferences := f.refs
	f.refsMu.Unlock()
	err = f.updateReferences(func(refs *probeReferences) {
		if targetOriginal != nil {
			if _, exists := refs.Bases[newPath]; !exists {
				if _, err := f.base.Lstat(newPath); errors.Is(err, os.ErrNotExist) {
					refs.Bases[newPath] = *targetOriginal
				}
			}
		}
		for _, entry := range entries {
			if entry.stat.Mode&unix.S_IFMT == unix.S_IFDIR {
				continue
			}
			_, wasReference := refs.Files[entry.old]
			delete(refs.Files, entry.old)
			delete(refs.Files, entry.new)
			if entry.ref != nil {
				if !wasReference {
					if _, exists := refs.Bases[entry.old]; !exists {
						if _, err := f.base.Lstat(entry.old); errors.Is(err, os.ErrNotExist) {
							refs.Bases[entry.old] = *entry.ref
						}
					}
				}
				refs.Files[entry.new] = *entry.ref
			}
		}
	})
	if err != nil {
		return errors.Join(err, f.updateReferences(func(refs *probeReferences) { *refs = previousReferences }))
	}
	complete = true
	return nil
}

// This private fixture socket models the filesystem-owner boundary. It is not
// mounted into the developer namespace and is not an activation implementation.
func (f *probeFS) serveControl(file *os.File) {
	defer file.Close()
	scanner := bufio.NewScanner(file)
	encoder := json.NewEncoder(file)
	for scanner.Scan() {
		var request struct{ Action, Path, ID string }
		var answer map[string]any
		err := json.Unmarshal(scanner.Bytes(), &request)
		if err == nil && (!safeProbePath(request.Path) || !safeProbePath(request.ID) || strings.Contains(request.ID, "/") || strings.HasPrefix(request.ID, ".") || len(request.ID) > 64) {
			err = unix.EINVAL
		}
		if err == nil {
			switch request.Action {
			case "changes":
				answer, err = f.changedPaths(request.Path)
			case "capture":
				answer, err = f.capture(request.Path, request.ID)
			case "capture-directory":
				answer, err = f.captureDirectory(request.Path, request.ID)
			case "acknowledge":
				answer, err = f.acknowledge(request.Path, request.ID)
			case "acknowledge-directory":
				answer, err = f.acknowledgeDirectory(request.Path, request.ID)
			case "release":
				err = f.releaseCapture(request.Path, request.ID)
				answer = map[string]any{"status": "released"}
			case "verify":
				var captured *os.File
				captured, err = f.snapshots.OpenFile(request.ID+"/file", unix.O_PATH|unix.O_NOFOLLOW, 0)
				if err == nil {
					var sum string
					sum, err = f.digest(captured)
					captured.Close()
					answer = map[string]any{"sha256": sum}
				}
			default:
				err = unix.EINVAL
			}
		}
		if err != nil {
			answer = map[string]any{"error": err.Error()}
			var errno unix.Errno
			if errors.As(err, &errno) {
				answer["errno"] = int(errno)
			}
		}
		if encoder.Encode(answer) != nil {
			return
		}
	}
}

func (f *probeFS) changedPaths(prefix string) (map[string]any, error) {
	f.publicationMu.RLock()
	defer f.publicationMu.RUnlock()
	names := map[string]bool{}
	for _, root := range []*os.Root{f.upper, f.deleted} {
		err := fs.WalkDir(root.FS(), prefix, func(name string, entry fs.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				names[name] = true
			}
			// ponytail: this probe refuses larger private change sets; paginate
			// the private namespace before qualifying bulk package rewrites.
			if len(names) > 4096 {
				return unix.E2BIG
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	f.refsMu.Lock()
	for name := range f.refs.Files {
		if name == prefix || strings.HasPrefix(name, prefix+"/") {
			names[name] = true
		}
	}
	f.refsMu.Unlock()
	if len(names) > 4096 {
		return nil, unix.E2BIG
	}
	paths := make([]string, 0, len(names))
	for name := range names {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	directories := []string{}
	if dir, err := f.snapshots.Open(".directories"); err == nil {
		entries, readErr := dir.ReadDir(4097)
		dir.Close()
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		if len(entries) > 4096 {
			return nil, unix.E2BIG
		}
		for _, entry := range entries {
			marker := ".directories/" + entry.Name()
			file, err := f.snapshots.OpenFile(marker, os.O_RDONLY|unix.O_NOFOLLOW, 0)
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(io.LimitReader(file, 4097))
			file.Close()
			name := string(data)
			if err != nil || len(data) > 4096 || !safeProbePath(name) || marker != directoryMarker(name) {
				return nil, unix.EIO
			}
			if name == prefix || strings.HasPrefix(name, prefix+"/") {
				directories = append(directories, name)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	sort.Strings(directories)
	answer := map[string]any{"paths": paths, "directories": directories}
	encoded, err := json.Marshal(answer)
	if err != nil || len(encoded) > 1<<20 {
		return nil, unix.E2BIG
	}
	// Review needs only retained blob identity, not rename source/version data.
	type previewReference struct {
		Package, Blob string
		Mode          uint32
	}
	references := struct{ Files, Bases map[string]previewReference }{
		Files: map[string]previewReference{}, Bases: map[string]previewReference{},
	}
	f.refsMu.Lock()
	for _, name := range paths {
		if ref, ok := f.refs.Files[name]; ok {
			references.Files[name] = previewReference{ref.Package, ref.Blob, ref.Mode}
		}
		if ref, ok := f.refs.Bases[name]; ok {
			references.Bases[name] = previewReference{ref.Package, ref.Blob, ref.Mode}
		}
	}
	f.refsMu.Unlock()
	answer["references"] = references
	encoded, err = json.Marshal(answer)
	if err != nil || len(encoded) >= 4<<20 {
		return nil, unix.E2BIG
	}
	return answer, nil
}

func safeProbePath(name string) bool {
	return name != "" && name != "." && name != ".." && path.Clean(name) == name &&
		!strings.HasPrefix(name, "/") && !strings.HasPrefix(name, "../") && !strings.ContainsRune(name, 0)
}
func sameVersion(a, b lisafs.Statx) bool {
	return key(a) == key(b) && a.Mode == b.Mode && a.Size == b.Size && a.Mtime == b.Mtime && a.Ctime == b.Ctime
}
func readSymlink(file *os.File) (string, error) {
	buffer := make([]byte, 4096)
	n, err := unix.Readlinkat(int(file.Fd()), "", buffer)
	if err != nil {
		return "", err
	}
	if n == len(buffer) {
		return "", unix.ENAMETOOLONG
	}
	return string(buffer[:n]), nil
}

func (f *probeFS) digest(file *os.File) (string, error) {
	s, err := stat(file)
	if err != nil {
		return "", err
	}
	if s.Mode&unix.S_IFMT == unix.S_IFLNK {
		target, err := readSymlink(file)
		return fmt.Sprintf("%x", sha256.Sum256([]byte(target))), err
	}
	input, err := f.reopen(file, unix.O_RDONLY)
	if err != nil {
		return "", err
	}
	defer input.Close()
	hash := sha256.New()
	_, err = io.Copy(hash, input)
	return fmt.Sprintf("%x", hash.Sum(nil)), err
}
func (f *probeFS) readPublicationHead(headPath string) (string, error) {
	data, err := f.snapshots.ReadFile(headPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(data), err
}
func (f *probeFS) publicationHead(name string) (string, error) {
	return f.readPublicationHead(".heads/" + name)
}

func (f *probeFS) captureDirectory(name, id string) (_ map[string]any, err error) {
	f.publicationMu.RLock()
	defer f.publicationMu.RUnlock()
	if err := f.snapshots.Mkdir(id, 0700); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			f.snapshots.RemoveAll(id)
		}
	}()
	if err := f.snapshots.Link(directoryMarker(name), id+"/directory"); err != nil {
		return nil, err
	}
	if err := f.snapshots.WriteFile(id+"/path", []byte(name), 0600); err != nil {
		return nil, err
	}
	head, err := f.readPublicationHead(directoryHead(name))
	if err != nil {
		return nil, err
	}
	if err := f.snapshots.WriteFile(id+"/parent", []byte(head), 0600); err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "directory": true}, nil
}

func (f *probeFS) acknowledgeDirectory(name, id string) (map[string]any, error) {
	savedPath, err := f.snapshots.ReadFile(id + "/path")
	if err != nil || string(savedPath) != name {
		return nil, unix.EINVAL
	}
	parent, err := f.snapshots.ReadFile(id + "/parent")
	if err != nil {
		return nil, err
	}
	captured, err := f.snapshots.Lstat(id + "/directory")
	if err != nil {
		return nil, err
	}
	f.publicationMu.Lock()
	defer f.publicationMu.Unlock()
	headPath := directoryHead(name)
	head, err := f.readPublicationHead(headPath)
	if err != nil {
		return nil, err
	}
	if head == id {
		return map[string]any{"status": "already_acknowledged"}, nil
	}
	if head != string(parent) {
		return nil, unix.ESTALE
	}
	answer := map[string]any{"status": "retired"}
	marker := directoryMarker(name)
	if current, err := f.snapshots.Lstat(marker); err == nil {
		if !os.SameFile(captured, current) {
			answer["status"], answer["reason"] = "retained", "later_directory_change"
		} else if upper, err := f.upper.OpenFile(name, unix.O_PATH|unix.O_NOFOLLOW, 0); err == nil {
			s, err := stat(upper)
			upper.Close()
			if err != nil {
				return nil, err
			}
			f.mu.Lock()
			references := 0
			if inode := f.inodes[key(s)]; inode != nil {
				references = inode.refs
			}
			f.mu.Unlock()
			if s.Mode&unix.S_IFMT != unix.S_IFDIR {
				answer["status"], answer["reason"] = "retained", "later_creation"
			} else if references != 0 {
				answer["status"], answer["reason"] = "retained", "open_references"
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if answer["status"] == "retired" {
			if err := f.snapshots.Remove(marker); err != nil {
				return nil, err
			}
			if err := syncRootPath(f.snapshots, ".directories"); err != nil {
				return nil, err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return answer, f.recordPublicationHead(headPath, id)
}

func (f *probeFS) capture(name, id string) (answer map[string]any, err error) {
	f.publicationMu.RLock()
	defer f.publicationMu.RUnlock()
	// Register before opening the source so a later unlink can preserve the
	// absence of a captured new file. The control owner serializes captures/acks.
	pending := ".pending/" + name
	previous, previousErr := f.snapshots.ReadFile(pending)
	if previousErr != nil && !errors.Is(previousErr, os.ErrNotExist) {
		return nil, previousErr
	}
	if err := f.snapshots.MkdirAll(path.Dir(pending), 0700); err != nil {
		return nil, err
	}
	if err := f.snapshots.WriteFile(pending, []byte(id), 0600); err != nil {
		return nil, err
	}
	complete := false
	defer func() {
		if !complete {
			var cleanup error
			if previousErr == nil {
				cleanup = f.snapshots.WriteFile(pending, previous, 0600)
			} else {
				cleanup = f.snapshots.Remove(pending)
			}
			err = errors.Join(err, cleanup)
		}
	}()
	source, err := f.upper.OpenFile(name, unix.O_PATH|unix.O_NOFOLLOW, 0)
	deleted := false
	ref, referenced := f.reference(name, false)
	referenced = referenced && errors.Is(err, os.ErrNotExist)
	if errors.Is(err, os.ErrNotExist) {
		if !referenced {
			marker, err := f.deleted.Lstat(name)
			if err != nil {
				return nil, err
			}
			if !marker.Mode().IsRegular() {
				return nil, unix.EOPNOTSUPP
			}
			deleted = true
		}
	} else if err != nil {
		return nil, err
	}
	var before lisafs.Statx
	if !deleted && !referenced {
		defer source.Close()
		before, err = stat(source)
		if err != nil {
			return nil, err
		}
		if before.Mode&unix.S_IFMT != unix.S_IFREG && before.Mode&unix.S_IFMT != unix.S_IFLNK {
			return nil, unix.EOPNOTSUPP
		}
	}
	if err := f.snapshots.Mkdir(id, 0700); err != nil {
		return nil, err
	}
	defer func() {
		if !complete {
			f.snapshots.RemoveAll(id)
		}
	}()
	if deleted {
		if err := f.snapshots.WriteFile(id+"/deleted", nil, 0600); err != nil {
			return nil, err
		}
	} else if referenced {
		data, err := json.Marshal(ref)
		if err != nil {
			return nil, err
		}
		if err := f.snapshots.WriteFile(id+"/file-reference", data, 0600); err != nil {
			return nil, err
		}
	} else {
		if err := f.copyFile(f.snapshots, id+"/file", source, before); err != nil {
			return nil, err
		}
		after, err := stat(source)
		if err != nil {
			return nil, err
		}
		if !sameVersion(before, after) {
			return nil, unix.EBUSY
		}
	}
	original, err := f.base.OpenFile(name, unix.O_PATH|unix.O_NOFOLLOW, 0)
	hasOriginal := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if hasOriginal {
		s, err := stat(original)
		if err == nil {
			err = f.copyFile(f.snapshots, id+"/base", original, s)
		}
		original.Close()
		if err != nil {
			return nil, err
		}
	} else if ref, ok := f.reference(name, true); ok {
		data, err := json.Marshal(ref)
		if err != nil {
			return nil, err
		}
		if err := f.snapshots.WriteFile(id+"/base-reference", data, 0600); err != nil {
			return nil, err
		}
		hasOriginal = true
	}
	if err := f.snapshots.WriteFile(id+"/path", []byte(name), 0600); err != nil {
		return nil, err
	}
	head, err := f.publicationHead(name)
	if err != nil {
		return nil, err
	}
	if err := f.snapshots.WriteFile(id+"/parent", []byte(head), 0600); err != nil {
		return nil, err
	}
	complete = true
	return map[string]any{"id": id, "original": hasOriginal, "deleted": deleted}, nil
}

func (f *probeFS) recordPublicationHead(headPath, id string) error {
	if err := f.snapshots.MkdirAll(path.Dir(headPath), 0700); err != nil {
		return err
	}
	tmp := ".head-" + id
	if err := f.snapshots.WriteFile(tmp, []byte(id), 0600); err != nil {
		return err
	}
	if err := f.snapshots.Rename(tmp, headPath); err != nil {
		return err
	}
	return nil
}
func (f *probeFS) recordPublication(name, id string) error {
	if err := f.recordPublicationHead(".heads/"+name, id); err != nil {
		return err
	}
	pending := ".pending/" + name
	if captured, err := f.snapshots.ReadFile(pending); err == nil && string(captured) == id {
		return f.snapshots.Remove(pending)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func syncRootPath(root *os.Root, name string) error {
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

// Only the serialized control owner releases captures, after publication and
// acknowledgement. Keep identity receipts so IDs cannot be reused and retries
// remain safe. The next original is already stored independently in base/.
func (f *probeFS) releaseCapture(name, id string) error {
	receipt := ".heads/" + name
	if _, err := f.snapshots.Lstat(id + "/directory"); err == nil {
		receipt = directoryHead(name)
		if err := syncRootPath(f.snapshots, id+"/directory"); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	head, err := f.readPublicationHead(receipt)
	if err != nil {
		return err
	}
	if head != id {
		return unix.ESTALE
	}
	for _, receipt := range []string{id + "/path", id + "/parent", id} {
		if err := syncRootPath(f.snapshots, receipt); err != nil {
			return err
		}
	}
	if err := syncRootPath(f.snapshots, receipt); err != nil {
		return err
	}
	for directory := path.Dir(receipt); ; directory = path.Dir(directory) {
		if err := syncRootPath(f.snapshots, directory); err != nil {
			return err
		}
		if directory == "." {
			break
		}
	}
	for _, payload := range []string{id + "/file", id + "/base", id + "/file-reference", id + "/base-reference"} {
		if err := f.snapshots.Remove(payload); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return syncRootPath(f.snapshots, id)
}

func (f *probeFS) acknowledge(name, id string) (answer map[string]any, err error) {
	savedPath, err := f.snapshots.ReadFile(id + "/path")
	if err != nil || string(savedPath) != name {
		return nil, unix.EINVAL
	}
	parent, err := f.snapshots.ReadFile(id + "/parent")
	if err != nil {
		return nil, err
	}
	head, err := f.publicationHead(name)
	if err != nil {
		return nil, err
	}
	if head == id {
		return map[string]any{"status": "already_acknowledged"}, nil
	}
	if head != string(parent) {
		return nil, unix.ESTALE
	}
	if data, err := f.snapshots.ReadFile(id + "/file-reference"); err == nil {
		var captured probeReference
		if err := json.Unmarshal(data, &captured); err != nil {
			return nil, err
		}
		f.publicationMu.Lock()
		defer f.publicationMu.Unlock()
		if head, err := f.publicationHead(name); err != nil || head != string(parent) {
			return nil, unix.ESTALE
		}
		current, exists := f.reference(name, false)
		_, upperErr := f.upper.Lstat(name)
		if upperErr != nil && !errors.Is(upperErr, os.ErrNotExist) {
			return nil, upperErr
		}
		retire := exists && current == captured && errors.Is(upperErr, os.ErrNotExist)
		parts := strings.Split(captured.Version, ":")
		if len(parts) != 9 {
			return nil, unix.EIO
		}
		var originalKey inodeKey
		originalKey.major, _ = strconv.ParseUint(parts[0], 10, 64)
		originalKey.minor, _ = strconv.ParseUint(parts[1], 10, 64)
		originalKey.ino, _ = strconv.ParseUint(parts[2], 10, 64)
		f.mu.Lock()
		if inode := f.inodes[originalKey]; inode != nil && inode.refs != 0 {
			retire = false
		}
		f.mu.Unlock()
		if err := f.updateReferences(func(refs *probeReferences) {
			if retire {
				delete(refs.Files, name)
				delete(refs.Bases, name)
			} else {
				refs.Bases[name] = captured
			}
		}); err != nil {
			return nil, err
		}
		if err := f.base.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		answer := map[string]any{"status": "retained", "reason": "later_change_or_open_reference"}
		if retire {
			if err := f.deleted.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			answer = map[string]any{"status": "retired"}
		}
		return answer, f.recordPublication(name, id)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if _, deletedErr := f.snapshots.Lstat(id + "/deleted"); deletedErr == nil {
		f.publicationMu.Lock()
		defer f.publicationMu.Unlock()
		head, err := f.publicationHead(name)
		if err != nil {
			return nil, err
		}
		if head != string(parent) {
			return nil, unix.ESTALE
		}
		answer = map[string]any{"status": "retired"}
		if _, err := f.upper.Lstat(name); err == nil {
			answer["status"], answer["reason"] = "retained", "later_creation"
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if _, exists := f.reference(name, false); exists {
			answer["status"], answer["reason"] = "retained", "later_creation"
		}
		// The captured private version was absent, even if a later creation or
		// the merged shared result now contains a file at this name.
		for _, root := range []*os.Root{f.base, f.deleted} {
			if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
		if err := f.updateReferences(func(refs *probeReferences) { delete(refs.Bases, name) }); err != nil {
			return nil, err
		}
		return answer, f.recordPublication(name, id)
	} else if !errors.Is(deletedErr, os.ErrNotExist) {
		return nil, deletedErr
	}
	captured, err := f.snapshots.OpenFile(id+"/file", unix.O_PATH|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer captured.Close()
	wantedVersion, err := stat(captured)
	if err != nil {
		return nil, err
	}
	want, err := f.digest(captured)
	if err != nil {
		return nil, err
	}
	// Hash outside the exclusive section. Under the lock we recheck version,
	// inode identity, and all control references before removing a private file.
	current, openErr := f.upper.OpenFile(name, unix.O_PATH|unix.O_NOFOLLOW, 0)
	var before lisafs.Statx
	var got string
	if openErr == nil {
		defer current.Close()
		before, err = stat(current)
		if err == nil {
			got, err = f.digest(current)
		}
		if err != nil {
			return nil, err
		}
	} else if !errors.Is(openErr, os.ErrNotExist) {
		return nil, openErr
	}
	// Keep checkpoint bytes independent of the immutable captured file. A
	// cross-root hardlink alias became unopenable from the confined Gofer after
	// replacing that alias, although the host could still read the snapshot.
	// Use the existing complete-file copy and rename at this owning boundary.
	checkpoint := ".publish-" + id
	if err := f.copyFile(f.base, checkpoint, captured, wantedVersion); err != nil {
		return nil, err
	}
	defer f.base.Remove(checkpoint)
	f.publicationMu.Lock()
	started := time.Now()
	defer f.publicationMu.Unlock()
	answer = map[string]any{"status": "retained"}
	defer func() { answer["exclusive_ms"] = float64(time.Since(started).Nanoseconds()) / 1e6 }()
	head, err = f.publicationHead(name)
	if err != nil {
		return answer, err
	}
	if head != string(parent) {
		return answer, unix.ESTALE
	}
	defer func() {
		if err != nil {
			return
		}
		err = f.recordPublication(name, id)
	}()
	if err := f.base.MkdirAll(path.Dir(name), 0700); err != nil {
		return answer, err
	}
	to, err := f.base.Open(path.Dir(name))
	if err != nil {
		return answer, err
	}
	defer to.Close()
	// The next delta is relative to what was captured, not to a merged shared
	// result that the developer's retained file may never have contained.
	if err := f.base.Rename(checkpoint, name); err != nil {
		return answer, err
	}
	if err := to.Sync(); err != nil {
		return answer, err
	}
	if openErr != nil {
		answer["reason"] = "path_changed"
		return answer, nil
	}
	if got != want {
		answer["reason"] = "later_content"
		return answer, nil
	}
	if before.Mode != wantedVersion.Mode {
		answer["reason"] = "later_mode"
		return answer, nil
	}
	file, err := f.upper.OpenFile(name, unix.O_PATH|unix.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		answer["reason"] = "path_changed"
		return answer, nil
	}
	if err != nil {
		return answer, err
	}
	defer file.Close()
	now, err := stat(file)
	if err != nil {
		return answer, err
	}
	if !sameVersion(before, now) {
		answer["reason"] = "version_changed"
		return answer, nil
	}
	f.mu.Lock()
	references := 0
	if inode := f.inodes[key(now)]; inode != nil {
		references = inode.refs
	}
	f.mu.Unlock()
	if references != 0 {
		answer["reason"], answer["references"] = "open_references", references
		return answer, nil
	}
	// ponytail: live-operation safety only. These namespace updates still need
	// a durable intent and recovery before power-loss/crash qualification.
	if err := f.upper.Remove(name); err != nil {
		return answer, err
	}
	if err := f.base.Remove(name); err != nil {
		return answer, err
	}
	if err := f.updateReferences(func(refs *probeReferences) { delete(refs.Bases, name); delete(refs.Files, name) }); err != nil {
		return answer, err
	}
	if err := f.deleted.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return answer, err
	}
	answer["status"] = "retired"
	return answer, nil
}
func (fd *probeControl) StatFS() (lisafs.StatFS, error) {
	return lisafs.StatFS{Type: unix.OVERLAYFS_SUPER_MAGIC, BlockSize: 4096, NameLength: 255}, nil
}
func (fd *probeControl) Readlink(buffer func(uint32) []byte) (uint16, error) {
	fd.inode.mu.Lock()
	defer fd.inode.mu.Unlock()
	name, err := readSymlink(fd.inode.file)
	if err != nil {
		return 0, err
	}
	if len(name) > 65535 {
		return 0, unix.ENAMETOOLONG
	}
	copy(buffer(uint32(len(name))), name)
	return uint16(len(name)), nil
}

type probeOpen struct {
	lisafs.OpenFD
	file    *os.File
	control *probeControl
	entries []string
	offset  int
}

func (fd *probeOpen) FD() *lisafs.OpenFD                    { return &fd.OpenFD }
func (fd *probeOpen) Close()                                { fd.file.Close() }
func (fd *probeOpen) Stat() (lisafs.Statx, error)           { return stat(fd.file) }
func (fd *probeOpen) Sync() error                           { return fd.file.Sync() }
func (fd *probeOpen) Flush() error                          { return nil }
func (fd *probeOpen) Renamed()                              {}
func (fd *probeOpen) Allocate(uint64, uint64, uint64) error { return unix.EOPNOTSUPP }
func (fd *probeOpen) Read(b []byte, off uint64) (uint64, error) {
	n, err := fd.file.ReadAt(b, int64(off))
	if err == io.EOF {
		err = nil
	}
	return uint64(n), err
}
func (fd *probeOpen) Write(b []byte, off uint64) (uint64, error) {
	n, err := fd.file.WriteAt(b, int64(off))
	return uint64(n), err
}
func (f *probeFS) directoryNames(name string) ([]string, error) {
	hidden, err := f.hiddenDirectory(name)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, root := range []*os.Root{f.lower, f.upper} {
		if hidden && root == f.lower {
			continue
		}
		dir, err := root.Open(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// ponytail: one directory is materialized per enumeration in this
		// probe. Bound/stream it before qualifying large single directories.
		entries, err := dir.ReadDir(-1)
		dir.Close()
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			names[entry.Name()] = true
		}
	}
	f.refsMu.Lock()
	for filename := range f.refs.Files {
		if path.Dir(filename) == name {
			names[path.Base(filename)] = true
		}
	}
	f.refsMu.Unlock()
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func (fd *probeOpen) Getdent64(count uint32, seek0 bool, record func(lisafs.Dirent64)) error {
	if seek0 {
		fd.entries, fd.offset = nil, 0
	}
	if fd.entries == nil {
		var err error
		fd.entries, err = fd.control.fs.directoryNames(fd.control.name())
		if err != nil {
			return err
		}
	}
	var used uint32
	for fd.offset < len(fd.entries) {
		name := fd.entries[fd.offset]
		size := uint32((24 + len(name) + 7) &^ 7)
		if used+size > count {
			break
		}
		file, _, err := fd.control.fs.lookup(path.Join(fd.control.name(), name))
		if errors.Is(err, os.ErrNotExist) {
			fd.offset++
			continue
		}
		if err != nil {
			return err
		}
		s, err := stat(file)
		file.Close()
		if err != nil {
			return err
		}
		fd.offset++
		record(lisafs.Dirent64{Ino: primitive.Uint64(s.Ino), Off: primitive.Uint64(fd.offset),
			DevMajor: primitive.Uint32(s.DevMajor), DevMinor: primitive.Uint32(s.DevMinor),
			Type: primitive.Uint8((s.Mode & unix.S_IFMT) >> 12), Name: lisafs.SizedString(name)})
		used += size
	}
	return nil
}

// The remaining unsupported mutations fail explicitly; this probe is not adoptable.
type unsupportedControl struct{}

func (unsupportedControl) Renamed() {}
func (unsupportedControl) Mknod(linux.FileMode, lisafs.UID, lisafs.GID, string, uint32, uint32) (*lisafs.ControlFD, lisafs.Statx, error) {
	return nil, lisafs.Statx{}, unix.EOPNOTSUPP
}
func (unsupportedControl) Connect(uint32) (int, error) { return -1, unix.EOPNOTSUPP }
func (unsupportedControl) ConnectWithCreds(uint32, lisafs.UID, lisafs.GID) (int, error) {
	return -1, unix.EOPNOTSUPP
}
func (unsupportedControl) BindAt(string, uint32, linux.FileMode, lisafs.UID, lisafs.GID) (*lisafs.ControlFD, lisafs.Statx, *lisafs.BoundSocketFD, int, error) {
	return nil, lisafs.Statx{}, nil, -1, unix.EOPNOTSUPP
}
func (unsupportedControl) RenameAt2(string, lisafs.ControlFDImpl, string, uint32) error {
	return unix.EOPNOTSUPP
}
func (unsupportedControl) GetXattr(string, uint32, func(uint32) []byte) (uint16, error) {
	return 0, unix.ENODATA
}
func (unsupportedControl) SetXattr(string, string, uint32) error        { return unix.EOPNOTSUPP }
func (unsupportedControl) ListXattr(uint64) (lisafs.StringArray, error) { return nil, nil }
func (unsupportedControl) RemoveXattr(string) error                     { return unix.EOPNOTSUPP }

func main() {
	if len(os.Args) == 2 && os.Args[1] == "workflow-read-client" {
		if err := readClient(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "workflow-held-mmap-client" {
		if err := heldMmapClient(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if (len(os.Args) == 3 || len(os.Args) == 4 && os.Args[3] == "atomic") && os.Args[1] == "workflow-write-client" {
		if err := writeClient(os.Args[2], len(os.Args) == 4); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "workflow-file-client" {
		if err := fileClient(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	extension.Register(&probeExtension{})
	maincli.Main()
}

func readClient() error {
	var samples []int64
	for sample := -1; sample < 31; sample++ {
		started := time.Now()
		for n := 0; n < 10; n++ {
			body, err := os.ReadFile(fmt.Sprintf("reads/%d", n))
			if err != nil {
				return err
			}
			if string(body) != "read working set\n" {
				return fmt.Errorf("unexpected working-set file %d", n)
			}
		}
		if sample >= 0 {
			samples = append(samples, time.Since(started).Nanoseconds())
		}
	}
	return json.NewEncoder(os.Stdout).Encode(samples)
}

func heldMmapClient() error {
	file, err := os.OpenFile("publish-mmap.txt", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	mapped, err := unix.Mmap(int(file.Fd()), 0, 4096, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		file.Close()
		return err
	}
	copy(mapped, "private")
	if err := errors.Join(unix.Msync(mapped, unix.MS_SYNC), file.Close()); err != nil {
		return err
	}
	if err := os.WriteFile("/tmp/map-ready", nil, 0600); err != nil {
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat("/tmp/map-release"); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for mmap publication barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}
	copy(mapped[16:], "later mmap")
	if err := errors.Join(unix.Msync(mapped, unix.MS_SYNC), unix.Munmap(mapped)); err != nil {
		return err
	}
	return os.WriteFile("/tmp/map-done", nil, 0600)
}

func writeClient(profile string, atomic bool) error {
	results := map[string]map[string][]int64{}
	for _, size := range []int{64, 65536, 1048576} {
		body := make([]byte, size)
		for i := range body {
			body[i] = byte(i % 251)
		}
		samples := map[string][]int64{}
		for _, phase := range []string{"first", "repeat"} {
			body[0]++
			for n := 0; n < 31; n++ {
				name := fmt.Sprintf("writes/%s/%d/%d", profile, size, n)
				started := time.Now()
				target, flags := name, os.O_WRONLY|os.O_TRUNC
				if atomic {
					target, flags = name+".tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL
				}
				file, err := os.OpenFile(target, flags, 0644)
				if err != nil {
					return err
				}
				written, writeErr := file.Write(body)
				err = errors.Join(writeErr, file.Sync(), file.Close())
				if err != nil {
					return err
				}
				if written != size {
					return io.ErrShortWrite
				}
				if atomic {
					if err := os.Rename(target, name); err != nil {
						return err
					}
					dir, err := os.Open(path.Dir(name))
					if err != nil {
						return err
					}
					if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
						return err
					}
				}
				samples[phase] = append(samples[phase], time.Since(started).Nanoseconds())
			}
		}
		results[strconv.Itoa(size)] = samples
	}
	return json.NewEncoder(os.Stdout).Encode(results)
}

func fileClient() error {
	read, err := os.Open("handles.txt")
	if err != nil {
		return err
	}
	defer read.Close()
	mapped, err := unix.Mmap(int(read.Fd()), 0, 4096, unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return err
	}
	defer unix.Munmap(mapped)
	if string(mapped[:7]) != "before\n" {
		return fmt.Errorf("initial mmap content=%q", mapped[:7])
	}
	write, err := os.OpenFile("handles.txt", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer write.Close()
	if _, err := write.WriteAt([]byte("private"), 0); err != nil {
		return err
	}
	if err := write.Sync(); err != nil {
		return err
	}
	b := make([]byte, 7)
	if _, err := read.ReadAt(b, 0); err != nil {
		return err
	}
	if string(b) != "private" || string(mapped[:7]) != "private" {
		return fmt.Errorf("first write: retained read=%q retained mmap=%q", b, mapped[:7])
	}
	writable, err := unix.Mmap(int(write.Fd()), 0, 4096, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return err
	}
	copy(writable[16:], "mmap write")
	syncErr := unix.Msync(writable, unix.MS_SYNC)
	unmapErr := unix.Munmap(writable)
	if err := errors.Join(syncErr, unmapErr, write.Sync()); err != nil {
		return err
	}
	fmt.Println("PASS ELF retained-read retained-mmap writable-mmap")
	return nil
}
