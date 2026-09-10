//go:build ignore

// A disposable syscall client built by the native integration tests.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func removedDirectory(file *os.File) {
	var buffer [4096]byte
	if _, err := unix.Getdents(int(file.Fd()), buffer[:]); err != unix.ENOENT {
		panic(fmt.Sprintf("getdents64 on removed directory: %v, want ENOENT", err))
	}
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "symlink-lower" {
		readLink := func(file *os.File, want string) {
			buffer := make([]byte, 4096)
			n, err := unix.Readlinkat(int(file.Fd()), "", buffer)
			must(err)
			if string(buffer[:n]) != want {
				panic(fmt.Sprintf("retained symlink = %q, want %q", buffer[:n], want))
			}
		}
		if err := syscall.Rename("links/from", "links/directory"); !errors.Is(err, syscall.EISDIR) {
			panic(fmt.Sprintf("symlink over directory: %v", err))
		}
		must(os.Rename("links/from", "links/from"))
		from, err := os.OpenFile("links/from", unix.O_PATH|unix.O_NOFOLLOW, 0)
		must(err)
		defer from.Close()
		target, err := os.OpenFile("links/target", unix.O_PATH|unix.O_NOFOLLOW, 0)
		must(err)
		defer target.Close()
		must(os.Rename("links/from", "links/target"))
		readLink(from, "../../host-only-source")
		readLink(target, "../../host-only-target")
		must(os.Remove("links/target"))
		readLink(from, "../../host-only-source")
		readLink(target, "../../host-only-target")
		must(os.Remove("links/remove"))
		must(os.Rename("links/keep", "links/moved"))
		must(os.Symlink("/tmp/private-symlink-target", "links/new"))
		fmt.Println("PASS shared symlink rename/delete, retained link descriptors, no-op, and type rejection")
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "gitdir-file" {
		must(syscall.Rename(".git", ".git-reference-check"))
		must(syscall.Rename(".git-reference-check", ".git"))
		info, err := os.Lstat(".git")
		must(err)
		if !info.Mode().IsRegular() {
			panic("package Git metadata is not an ordinary reference file")
		}
		fmt.Println("PASS ordinary Git reference rename")
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "remove-directories" {
		for _, name := range []string{"nested/remove.txt", "nested/deep/remove.txt"} {
			must(os.Remove(name))
		}
		entries, err := os.ReadDir(".")
		must(err)
		parentVisible := false
		for _, entry := range entries {
			parentVisible = parentVisible || entry.Name() == "nested"
		}
		if !parentVisible {
			panic("deleting a nested file hid its parent")
		}
		for _, name := range []string{"nested/keep.txt", "nested/deep/keep.txt"} {
			body, err := os.ReadFile(name)
			must(err)
			if string(body) != "untouched sibling\n" {
				panic("nested deletion changed an untouched sibling")
			}
		}
		must(os.MkdirAll("private-remove/deep", 0700))
		must(os.WriteFile("private-remove/deep/file.txt", []byte("private\n"), 0600))
		if err := syscall.Rmdir("private-remove"); !errors.Is(err, syscall.ENOTEMPTY) {
			panic(fmt.Sprintf("nonempty private rmdir: %v", err))
		}
		held, err := os.Open("private-remove/deep")
		must(err)
		defer held.Close()
		must(os.RemoveAll("private-remove"))
		must(os.MkdirAll("private-remove/deep", 0700))
		must(os.WriteFile("private-remove/deep/file.txt", []byte("new directory\n"), 0600))
		removedDirectory(held)
		must(os.RemoveAll("private-remove"))
		fmt.Println("PASS nested deletion keeps siblings; private directory removal and retained descriptor")
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "remove-shared-directories" {
		if err := syscall.Rmdir("remove-shared"); !errors.Is(err, syscall.ENOTEMPTY) {
			panic(fmt.Sprintf("nonempty shared rmdir: %v", err))
		}
		must(syscall.Rmdir("shared-empty"))
		must(os.RemoveAll("remove-shared"))
		held, err := os.Open("recreate-shared")
		must(err)
		defer held.Close()
		before, err := held.Stat()
		must(err)
		must(os.RemoveAll("recreate-shared"))
		must(os.Mkdir("recreate-shared", 0700))
		must(os.WriteFile("recreate-shared/private.txt", []byte("new private directory\n"), 0600))
		after, err := os.Stat("recreate-shared")
		must(err)
		if os.SameFile(before, after) {
			panic("recreated directory reused the removed directory's identity")
		}
		removedDirectory(held)
		for _, name := range []string{"shared-empty", "remove-shared", "recreate-shared/old.txt"} {
			if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
				panic(fmt.Sprintf("removed shared path reappeared: %s: %v", name, err))
			}
		}
		fmt.Println("PASS shared directory removal, nonempty rejection, recreation, and retained handle")
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "rename-lower" {
		// The integration test supplies these files in the shared tree.
		read := func(file *os.File, want string) {
			_, err := file.Seek(0, 0)
			must(err)
			body, err := io.ReadAll(file)
			must(err)
			if string(body) != want {
				panic(fmt.Sprintf("retained %s: got %q, want %q", file.Name(), body, want))
			}
		}
		must(os.Rename("rename/same.txt", "rename/same.txt"))
		must(os.Rename("rename/same.txt", "rename/alias.txt"))
		a, err := os.Stat("rename/same.txt")
		must(err)
		b, err := os.Stat("rename/alias.txt")
		must(err)
		if !os.SameFile(a, b) {
			panic("same-inode rename changed its names")
		}
		if err := syscall.Rename("rename/invalid.txt", "renamed/directory"); !errors.Is(err, syscall.EISDIR) {
			panic(fmt.Sprintf("rename file over directory: %v", err))
		}
		for _, item := range []struct{ from, to, oldTarget string }{
			{"rename/new.txt", "renamed/new.txt", ""},
			{"rename/replace.txt", "renamed/target.txt", "old target\n"},
		} {
			held, err := os.Open(item.from)
			must(err)
			var replaced *os.File
			if item.oldTarget != "" {
				replaced, err = os.Open(item.to)
				must(err)
			}
			must(os.Rename(item.from, item.to))
			if _, err := os.Stat(item.from); !errors.Is(err, os.ErrNotExist) {
				panic(fmt.Sprintf("renamed source remains: %v", err))
			}
			read(held, "shared rename\n")
			must(os.WriteFile(item.to, []byte("private rename\n"), 0600))
			read(held, "private rename\n")
			must(held.Close())
			if replaced != nil {
				read(replaced, item.oldTarget)
				must(replaced.Close())
			}
		}
		fmt.Println("PASS shared-file rename, replacement, retained handles, no-op and type rejection")
		return
	}
	if len(os.Args) == 4 && (os.Args[1] == "lower-links" || os.Args[1] == "link-lower") {
		first, second := os.Args[2], os.Args[3]
		readPath, writePath := second, first
		if os.Args[1] == "link-lower" {
			readPath, writePath = first, second
		}
		held, err := os.Open(readPath)
		must(err)
		defer held.Close()
		body, err := io.ReadAll(held)
		must(err)
		if string(body) != "linked original\n" {
			panic("incorrect linked lower fixture")
		}
		if os.Args[1] == "link-lower" {
			must(os.Link(first, second))
		}
		must(os.WriteFile(writePath, []byte("private linked\n"), 0600))
		_, err = held.Seek(0, 0)
		must(err)
		body, err = io.ReadAll(held)
		must(err)
		if string(body) != "private linked\n" {
			panic("pre-copy alias descriptor missed the private write")
		}
		a, err := os.Stat(first)
		must(err)
		b, err := os.Stat(second)
		must(err)
		if !os.SameFile(a, b) {
			panic("copy-up split visible hardlink aliases")
		}
		fmt.Println("PASS lower hardlinks and retained alias descriptor")
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "reject-utime" {
		err := syscall.UtimesNano(os.Args[2], []syscall.Timespec{{Sec: 1}, {Sec: 1}})
		if err != syscall.EOPNOTSUPP {
			panic(fmt.Sprintf("utimensat must expose EOPNOTSUPP, got %v", err))
		}
		fmt.Println("PASS utimensat EOPNOTSUPP")
		return
	}
	if len(os.Args) != 1 {
		panic("unknown native syscall check")
	}
	must(os.Mkdir("ignored/native", 0700))
	must(os.Chdir("ignored/native"))
	f, err := os.OpenFile("file", os.O_CREATE|os.O_RDWR, 0600)
	must(err)
	must(f.Truncate(4096))
	data, err := syscall.Mmap(int(f.Fd()), 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	must(err)
	copy(data, "mmap write")
	must(syscall.Munmap(data))
	must(f.Sync())
	body, err := os.ReadFile("file")
	must(err)
	if string(body[:10]) != "mmap write" {
		panic("shared mmap write lost")
	}
	must(os.Link("file", "hard"))
	must(os.Symlink("hard", "link"))
	must(os.WriteFile("replacement", []byte("replacement"), 0600))
	must(os.Rename("replacement", "file"))
	_, err = f.WriteAt([]byte("old handle"), 0)
	must(err)
	body, err = os.ReadFile("link")
	must(err)
	if string(body[:10]) != "old handle" {
		panic("hardlink/symlink/open handle detached incorrectly")
	}
	body, err = os.ReadFile("file")
	must(err)
	if string(body) != "replacement" {
		panic("atomic replacement modified the wrong inode")
	}
	must(syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	other, err := os.OpenFile("hard", os.O_RDWR, 0)
	must(err)
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != syscall.EWOULDBLOCK {
		panic(fmt.Sprintf("independent file lock was not excluded: %v", err))
	}
	must(other.Close())
	must(f.Close())
	watch, err := syscall.InotifyInit1(syscall.IN_NONBLOCK)
	must(err)
	defer syscall.Close(watch)
	_, err = syscall.InotifyAddWatch(watch, ".", syscall.IN_CREATE|syscall.IN_MOVED_TO|syscall.IN_DELETE)
	must(err)
	must(os.WriteFile("watch.tmp", []byte("watch"), 0600))
	must(os.Rename("watch.tmp", "watched"))
	must(os.Remove("watched"))
	events := make([]byte, 4096)
	n, err := syscall.Read(watch, events)
	must(err)
	var mask uint32
	for at := 0; at+16 <= n; {
		mask |= binary.LittleEndian.Uint32(events[at+4:])
		at += 16 + int(binary.LittleEndian.Uint32(events[at+12:]))
	}
	want := uint32(syscall.IN_CREATE | syscall.IN_MOVED_TO | syscall.IN_DELETE)
	if mask&want != want {
		panic(fmt.Sprintf("local watcher missing events: %#x", mask))
	}
	fmt.Println("PASS executable mmap hardlink symlink rename open-handle flock local-inotify")
}
