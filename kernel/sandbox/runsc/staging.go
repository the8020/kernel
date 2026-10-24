package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// Staging retirement uses the existing publication gate: no live descriptor
// may outlive the final version check and removal from the writable namespace.
func stagingPath(name string) bool {
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != ".conflicts" || len(parts[1]) != 24 || !validWorkspacePath(name) {
		return false
	}
	_, err := hex.DecodeString(parts[1])
	return err == nil && !strings.HasPrefix(parts[2], ".") && !strings.HasPrefix(parts[3], ".")
}

func (f *workspaceFS) stagingVersion(name string, retiring bool) (string, error) {
	if !stagingPath(name) {
		return "", unix.EINVAL
	}
	for parent := name; parent != "."; parent = path.Dir(parent) {
		info, err := f.upper.Lstat(parent)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", errors.New("staging parents must be ordinary directories")
		}
	}
	hash := sha256.New()
	count := 0
	err := fs.WalkDir(f.upper.FS(), name, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		if count > 8192 {
			return errors.New("staging exceeds 8,192 entries; preserve and export it")
		}
		file, err := f.upper.OpenFile(filename, unix.O_PATH|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		s, err := stat(file)
		file.Close()
		if err != nil {
			return err
		}
		if retiring {
			f.mu.Lock()
			inode := f.inodes[key(s)]
			busy := inode != nil && inode.refs != 0
			f.mu.Unlock()
			if busy || s.Mode&unix.S_IFMT != unix.S_IFDIR && s.Nlink != 1 {
				return errors.New("staging has open references or hardlinks; close users of this worktree and retry")
			}
		}
		fmt.Fprintf(hash, "%s\x00%d:%d:%d:%d:%d:%d:%d:%d:%d:%d\n", filename, s.DevMajor, s.DevMinor, s.Ino, s.Mode, s.Nlink, s.Size, s.Mtime.Sec, s.Mtime.Nsec, s.Ctime.Sec, s.Ctime.Nsec)
		return nil
	})
	return fmt.Sprintf("%x", hash.Sum(nil)), err
}

func (f *workspaceFS) staging(action, name, id, version string) (map[string]any, error) {
	if !stagingPath(name) {
		return nil, unix.EINVAL
	}
	if action == "staging-version" {
		f.publicationMu.RLock()
		defer f.publicationMu.RUnlock()
		version, err := f.stagingVersion(name, false)
		return map[string]any{"version": version}, err
	}
	if len(version) != 64 {
		return nil, unix.EINVAL
	}
	if _, err := hex.DecodeString(version); err != nil {
		return nil, unix.EINVAL
	}
	owner := name + "\n" + version + "\n"
	if saved, err := f.snapshots.ReadFile(id + "/staging-owner"); err == nil {
		if string(saved) != owner {
			return nil, unix.ESTALE
		}
	} else if errors.Is(err, os.ErrNotExist) && action == "retire-staging" {
		if err := f.snapshots.MkdirAll(id, 0700); err != nil {
			return nil, err
		}
		if err := f.snapshots.WriteFile(id+"/staging-owner", []byte(owner), 0600); err != nil {
			return nil, err
		}
		for _, p := range []string{id + "/staging-owner", id, "."} {
			if err := syncRootPath(f.snapshots, p); err != nil {
				return nil, err
			}
		}
	} else {
		return nil, err
	}
	if action == "release-staging" {
		// Keep the small identity receipt for restart-safe retries, like captures.
		if err := f.snapshots.RemoveAll(id + "/staging"); err != nil {
			return nil, err
		}
		return map[string]any{"status": "released"}, syncRootPath(f.snapshots, id)
	}
	f.publicationMu.Lock()
	defer f.publicationMu.Unlock()
	if _, err := f.upper.Lstat(name); errors.Is(err, os.ErrNotExist) {
		// The journal plus durable receipt owns this exact absent path. A
		// recreated directory must pass the original version check below.
		return map[string]any{"status": "retired"}, nil
	} else if err != nil {
		return nil, err
	}
	got, err := f.stagingVersion(name, true)
	if err != nil {
		return nil, err
	}
	if got != version {
		return nil, errors.New("staging changed after candidate preparation; export later work or abort cleanup preserving it")
	}
	from, err := f.upper.Open(path.Dir(name))
	if err != nil {
		return nil, err
	}
	defer from.Close()
	to, err := f.snapshots.Open(id)
	if err != nil {
		return nil, err
	}
	defer to.Close()
	if err := unix.Renameat2(int(from.Fd()), path.Base(name), int(to.Fd()), "staging", unix.RENAME_NOREPLACE); err != nil {
		return nil, err
	}
	if err := errors.Join(from.Sync(), to.Sync()); err != nil {
		return nil, err
	}
	for parent := path.Dir(name); parent != ".conflicts"; parent = path.Dir(parent) {
		if err := f.upper.Remove(parent); err != nil {
			break // Only empty task-owned parents are retired.
		}
	}
	return map[string]any{"status": "retired"}, nil
}
