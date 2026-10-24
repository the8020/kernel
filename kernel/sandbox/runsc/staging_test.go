package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStagingRetirementPreservesChangesAndRetries(t *testing.T) {
	root := t.TempDir()
	openRoot := func(name string) *os.Root {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
		r, err := os.OpenRoot(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close() })
		return r
	}
	f := &workspaceFS{upper: openRoot("upper"), snapshots: openRoot("snapshots"), inodes: map[inodeKey]*workspaceInode{}}
	name := ".conflicts/" + strings.Repeat("a", 24) + "/team/pkg"
	if err := f.upper.MkdirAll(name, 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.upper.WriteFile(name+"/file", []byte("published"), 0600); err != nil {
		t.Fatal(err)
	}
	version, err := f.stagingVersion(name, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.upper.WriteFile(name+"/file", []byte("later work"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.staging("retire-staging", name, "changed", version); err == nil {
		t.Fatal("retired later edits")
	}
	if data, _ := f.upper.ReadFile(name + "/file"); string(data) != "later work" {
		t.Fatal("lost later edit")
	}
	version, err = f.stagingVersion(name, false)
	if err != nil {
		t.Fatal(err)
	}
	file, err := f.upper.OpenFile(name+"/file", unix.O_PATH, 0)
	if err != nil {
		t.Fatal(err)
	}
	st, err := stat(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	f.inodes[key(st)] = &workspaceInode{refs: 1}
	if _, err := f.staging("retire-staging", name, "busy", version); err == nil {
		t.Fatal("retired an open descriptor")
	}
	delete(f.inodes, key(st))
	if _, err := f.staging("retire-staging", name, "busy", version); err != nil {
		t.Fatal(err)
	}
	if _, err := f.upper.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree remains: %v", err)
	}
	if data, _ := f.snapshots.ReadFile("busy/staging/file"); string(data) != "later work" {
		t.Fatal("retirement lost protected bytes")
	}
	for i := 0; i < 2; i++ {
		if _, err := f.staging("retire-staging", name, "busy", version); err != nil {
			t.Fatal(err)
		}
		if _, err := f.staging("release-staging", name, "busy", version); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.snapshots.Lstat("busy/staging"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired payload remains: %v", err)
	}
	if err := f.upper.MkdirAll(name, 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.upper.WriteFile(name+"/new", []byte("recreated"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.staging("retire-staging", name, "busy", version); err == nil {
		t.Fatal("retired a recreated path")
	}
	if _, err := f.staging("retire-staging", "team/pkg", "unknown", version); err == nil {
		t.Fatal("accepted a non-staging path")
	}
}

func TestDiscardCapturePreservesPrivateState(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	f := &workspaceFS{snapshots: root}
	name, id := "team/pkg/file", "capture"
	for _, directory := range []string{id, ".pending/team/pkg", ".heads/team/pkg"} {
		if err := root.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for file, body := range map[string]string{id + "/path": name, id + "/parent": "", id + "/file": "captured", id + "/base": "original", ".pending/" + name: id} {
		if err := root.WriteFile(file, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.discardCapture("team/pkg/foreign", id); err == nil {
		t.Fatal("discarded a foreign capture")
	}
	for i := 0; i < 2; i++ {
		if err := f.discardCapture(name, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{id + "/file", id + "/base", ".pending/" + name, ".heads/" + name} {
		if _, err := root.Stat(file); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("discard changed publication or retained payload %s: %v", file, err)
		}
	}
	if err := root.WriteFile(".heads/"+name, []byte(id), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.discardCapture(name, id); !errors.Is(err, unix.ESTALE) {
		t.Fatalf("discard accepted published capture: %v", err)
	}
}
