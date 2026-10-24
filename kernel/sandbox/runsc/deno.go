package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/lisafs"
)

// Deno's workspace glob walks package contents. Derive explicit members from
// the current merged view so shared updates and unpublished edits need no scan timer.
type denoConfigFS struct {
	*workspaceFS
	template *os.File
	mu       sync.Mutex
	data     []byte
	modified lisafs.StatxTimestamp
}

func (f *workspaceFS) denoMembers() ([]string, error) {
	members := []string{}
	namespaces, err := f.directoryNames(".")
	if err != nil {
		return nil, err
	}
	for _, namespace := range namespaces {
		if strings.HasPrefix(namespace, ".") || namespace == "node_modules" {
			continue
		}
		if ok, err := f.denoEntry(namespace, unix.S_IFDIR); err != nil {
			return nil, err
		} else if !ok {
			continue
		}
		packages, err := f.directoryNames(namespace)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, name := range packages {
			if strings.HasPrefix(name, ".") || name == "node_modules" {
				continue
			}
			dir := path.Join(namespace, name)
			if ok, err := f.denoEntry(dir, unix.S_IFDIR); err != nil {
				return nil, err
			} else if !ok {
				continue
			}
			for _, config := range []string{"deno.json", "deno.jsonc", "package.json"} {
				ok, err := f.denoEntry(path.Join(dir, config), unix.S_IFREG, unix.S_IFLNK)
				if err != nil {
					return nil, err
				}
				if ok {
					members = append(members, "./packages/"+dir)
					break
				}
			}
		}
	}
	return members, nil
}

func (f *workspaceFS) denoEntry(name string, kinds ...uint16) (bool, error) {
	file, _, err := f.lookup(name)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	s, err := stat(file)
	if err != nil {
		return false, err
	}
	for _, kind := range kinds {
		if s.Mode&unix.S_IFMT == kind {
			return true, nil
		}
	}
	return false, nil
}

func (f *denoConfigFS) snapshot() ([]byte, lisafs.Statx, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publicationMu.RLock()
	defer f.publicationMu.RUnlock()
	s, err := stat(f.template)
	if err != nil {
		return nil, s, err
	}
	if s.Mode&unix.S_IFMT != unix.S_IFREG || s.Size > 1<<20 {
		return nil, s, unix.EINVAL
	}
	body, err := io.ReadAll(io.NewSectionReader(f.template, 0, int64(s.Size)))
	if err != nil {
		return nil, s, err
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(body, &config); err != nil || config == nil {
		return nil, s, unix.EINVAL
	}
	members, err := f.denoMembers()
	if err != nil {
		return nil, s, err
	}
	config["workspace"], err = json.Marshal(members)
	if err != nil {
		return nil, s, err
	}
	body, err = json.Marshal(config)
	if err != nil {
		return nil, s, err
	}
	if !bytes.Equal(body, f.data) {
		now := time.Now()
		f.data, f.modified = body, lisafs.StatxTimestamp{Sec: now.Unix(), Nsec: uint32(now.Nanosecond())}
	}
	s.Mode, s.Size, s.Mtime, s.Ctime = unix.S_IFREG|0444, uint64(len(f.data)), f.modified, f.modified
	return f.data, s, nil
}

func (*denoConfigFS) SupportedMessages() []lisafs.MID {
	return []lisafs.MID{lisafs.Mount, lisafs.Channel, lisafs.FStat, lisafs.WalkStat,
		lisafs.OpenAt, lisafs.Close, lisafs.FSync, lisafs.PRead, lisafs.FStatFS}
}

func (f *denoConfigFS) Mount(c *lisafs.Connection, n *lisafs.Node) (*lisafs.ControlFD, lisafs.Statx, int, error) {
	_, s, err := f.snapshot()
	if err != nil {
		return nil, s, -1, err
	}
	n.IncRef()
	fd := &denoConfigControl{fs: f}
	fd.ControlFD.Init(c, n, linux.FileMode(s.Mode), fd)
	return fd.FD(), s, -1, nil
}

type denoConfigControl struct {
	lisafs.ControlFD
	unsupportedControl
	fs *denoConfigFS
}

func (fd *denoConfigControl) FD() *lisafs.ControlFD { return &fd.ControlFD }
func (fd *denoConfigControl) Close()                { fd.fs.template.Close() }
func (fd *denoConfigControl) Stat() (lisafs.Statx, error) {
	_, s, err := fd.fs.snapshot()
	return s, err
}
func (fd *denoConfigControl) WalkStat(names lisafs.StringArray, record func(lisafs.Statx)) error {
	if len(names) == 0 || len(names) == 1 && names[0] == "" {
		s, err := fd.Stat()
		if err == nil {
			record(s)
		}
		return err
	}
	return unix.ENOTDIR
}
func (fd *denoConfigControl) Open(flags uint32) (*lisafs.OpenFD, int, error) {
	if flags&unix.O_ACCMODE != unix.O_RDONLY || flags&unix.O_TRUNC != 0 {
		return nil, -1, unix.EROFS
	}
	data, s, err := fd.fs.snapshot()
	if err != nil {
		return nil, -1, err
	}
	opened := &denoConfigOpen{data: bytes.NewReader(data), stat: s}
	opened.OpenFD.Init(fd.FD(), flags, opened)
	return &opened.OpenFD, -1, nil
}
func (*denoConfigControl) StatFS() (lisafs.StatFS, error) {
	return lisafs.StatFS{Type: unix.OVERLAYFS_SUPER_MAGIC, BlockSize: 4096, NameLength: 255}, nil
}
func (*denoConfigControl) SetStat(s lisafs.SetStatReq) (uint32, error) { return s.Mask, unix.EROFS }
func (*denoConfigControl) Walk(string) (*lisafs.ControlFD, lisafs.Statx, error) {
	return nil, lisafs.Statx{}, unix.ENOTDIR
}
func (*denoConfigControl) OpenCreate(linux.FileMode, lisafs.UID, lisafs.GID, string, uint32) (*lisafs.ControlFD, lisafs.Statx, *lisafs.OpenFD, int, error) {
	return nil, lisafs.Statx{}, nil, -1, unix.EROFS
}
func (*denoConfigControl) Mkdir(linux.FileMode, lisafs.UID, lisafs.GID, string) (*lisafs.ControlFD, lisafs.Statx, error) {
	return nil, lisafs.Statx{}, unix.EROFS
}
func (*denoConfigControl) Symlink(string, string, lisafs.UID, lisafs.GID) (*lisafs.ControlFD, lisafs.Statx, error) {
	return nil, lisafs.Statx{}, unix.EROFS
}
func (*denoConfigControl) Link(lisafs.ControlFDImpl, string) (*lisafs.ControlFD, lisafs.Statx, error) {
	return nil, lisafs.Statx{}, unix.EROFS
}
func (*denoConfigControl) Unlink(string, uint32) error                         { return unix.EROFS }
func (*denoConfigControl) RenameAt(string, lisafs.ControlFDImpl, string) error { return unix.EROFS }
func (*denoConfigControl) Readlink(func(uint32) []byte) (uint16, error)        { return 0, unix.EINVAL }

type denoConfigOpen struct {
	lisafs.OpenFD
	data *bytes.Reader
	stat lisafs.Statx
}

func (fd *denoConfigOpen) FD() *lisafs.OpenFD                               { return &fd.OpenFD }
func (*denoConfigOpen) Close()                                              {}
func (fd *denoConfigOpen) Stat() (lisafs.Statx, error)                      { return fd.stat, nil }
func (*denoConfigOpen) Sync() error                                         { return nil }
func (*denoConfigOpen) Flush() error                                        { return nil }
func (*denoConfigOpen) Renamed()                                            {}
func (*denoConfigOpen) Allocate(uint64, uint64, uint64) error               { return unix.EROFS }
func (*denoConfigOpen) Write([]byte, uint64) (uint64, error)                { return 0, unix.EROFS }
func (*denoConfigOpen) Getdent64(uint32, bool, func(lisafs.Dirent64)) error { return unix.ENOTDIR }
func (fd *denoConfigOpen) Read(b []byte, off uint64) (uint64, error) {
	n, err := fd.data.ReadAt(b, int64(off))
	if err == io.EOF {
		err = nil
	}
	return uint64(n), err
}
