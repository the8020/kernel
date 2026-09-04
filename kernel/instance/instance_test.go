package instance

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveRootCanonicalizesAStillMissingExplicitDirectory(t *testing.T) {
	realParent := t.TempDir()
	linkParent := filepath.Join(t.TempDir(), "parent")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Fatal(err)
	}
	wanted := filepath.Join(realParent, "new", "instance")
	resolved, err := ResolveRoot(filepath.Join(linkParent, "new", "instance"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved != wanted {
		t.Fatalf("resolved root = %q, want %q", resolved, wanted)
	}
	if _, err := os.Stat(wanted); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resolution created the instance root: %v", err)
	}
}

func TestInitializeIdentityIsStable(t *testing.T) {
	root := t.TempDir()
	paths := NewPaths(root)
	first, err := Initialize(paths)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Initialize(paths)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("identity changed: %s != %s", first, second)
	}
	for _, path := range []string{paths.Packages, paths.Users, paths.Database, paths.NodeSettingsFile, paths.Run, paths.Logs, paths.Runtime, paths.RuntimeDefinitions, paths.RuntimeGroups, paths.RuntimeSandboxHistory, paths.RuntimePorts, paths.RuntimeServices, paths.RuntimeServicePools, paths.RuntimeAttachments, paths.RuntimeTemporary, paths.SSH} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing %s: %v", path, err)
		}
	}
	for _, obsolete := range []string{filepath.Join(root, "config"), filepath.Join(root, "state")} {
		if _, err := os.Stat(obsolete); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("obsolete runtime directory exists: %s (%v)", obsolete, err)
		}
	}
	attachments, err := os.Stat(paths.RuntimeAttachments)
	if err != nil {
		t.Fatal(err)
	}
	if attachments.Mode().Perm() != 0o755 {
		t.Fatalf("attachments permissions=%v", attachments.Mode().Perm())
	}
	runtimeRoot, err := os.Stat(paths.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeRoot.Mode().Perm() != 0o700 {
		t.Fatalf("runtime permissions=%v", runtimeRoot.Mode().Perm())
	}
	databaseRoot, err := os.Stat(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if databaseRoot.Mode().Perm() != 0o700 {
		t.Fatalf("database permissions=%v", databaseRoot.Mode().Perm())
	}
	sshRoot, err := os.Stat(paths.SSH)
	if err != nil {
		t.Fatal(err)
	}
	if sshRoot.Mode().Perm() != 0o700 {
		t.Fatalf("SSH state permissions=%v", sshRoot.Mode().Perm())
	}
	settingsFile, err := os.Stat(paths.NodeSettingsFile)
	if err != nil {
		t.Fatal(err)
	}
	if settingsFile.Mode().Perm() != 0o600 {
		t.Fatalf("settings file %s permissions=%v", paths.NodeSettingsFile, settingsFile.Mode().Perm())
	}
}

func TestFixedLayoutRequiresKernelConfiguration(t *testing.T) {
	root := t.TempDir()
	if _, err := LoadPaths(root); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("uninitialized load error = %v", err)
	}
	paths, err := Prepare(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != paths {
		t.Fatalf("loaded fixed paths differ: %#v != %#v", loaded, paths)
	}
}

func TestPrepareUsesOnlyFixedRoots(t *testing.T) {
	root := t.TempDir()
	paths, err := Prepare(root)
	if err != nil {
		t.Fatal(err)
	}
	if paths.Packages != filepath.Join(root, "packages") || paths.Users != filepath.Join(root, "users") || paths.Database != filepath.Join(root, "database") || paths.NodeSettingsFile != filepath.Join(root, "kernel.toml") {
		t.Fatalf("unexpected fixed paths: %#v", paths)
	}
}

func TestLockRejectsSecondOwnerAndCleansRuntimeFiles(t *testing.T) {
	paths := NewPaths(t.TempDir())
	if _, err := Initialize(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Socket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.PIDFile, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale socket was not removed: %v", err)
	}
	second, err := Acquire(paths)
	if !errors.Is(err, ErrAlreadyRunning) {
		if second != nil {
			_ = second.Release()
		}
		t.Fatalf("second lock error = %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.PIDFile, paths.Socket} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("runtime file remains %s: %v", path, err)
		}
	}
	third, err := Acquire(paths)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	_ = third.Release()
	if _, err := os.Stat(filepath.Join(paths.Run, "kernel.lock")); err != nil {
		t.Fatalf("lock file should remain reusable: %v", err)
	}
}
