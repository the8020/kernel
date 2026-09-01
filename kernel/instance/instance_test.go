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
	for _, path := range []string{paths.Packages, paths.ConfigAuth, paths.ConfigSecrets, paths.BootstrapSessions, paths.StateServices, paths.StatePackageIndex, paths.StatePackageData, paths.InstanceFile, paths.NodeSettingsFile, paths.GlobalSettingsFile, paths.Run, paths.Logs, paths.Runtime, paths.RuntimeGroups, paths.RuntimeSandboxHistory, paths.RuntimePorts, paths.RuntimeServices, paths.RuntimeServicePools, paths.RuntimeAttachments, paths.RuntimeTemporary, paths.SSH} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing %s: %v", path, err)
		}
	}
	secretsDirectory, err := os.Stat(paths.ConfigSecrets)
	if err != nil {
		t.Fatal(err)
	}
	if secretsDirectory.Mode().Perm() != 0o700 {
		t.Fatalf("secrets directory mode = %v", secretsDirectory.Mode().Perm())
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
	sshRoot, err := os.Stat(paths.SSH)
	if err != nil {
		t.Fatal(err)
	}
	if sshRoot.Mode().Perm() != 0o700 {
		t.Fatalf("SSH state permissions=%v", sshRoot.Mode().Perm())
	}
	bootstrapSessions, err := os.Stat(paths.BootstrapSessions)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrapSessions.Mode().Perm() != 0o700 {
		t.Fatalf("bootstrap authentication-session permissions=%v", bootstrapSessions.Mode().Perm())
	}
	for _, path := range []string{paths.NodeSettingsFile, paths.GlobalSettingsFile} {
		settingsFile, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if settingsFile.Mode().Perm() != 0o600 {
			t.Fatalf("settings file %s permissions=%v", path, settingsFile.Mode().Perm())
		}
	}
}

func TestLayoutUsesExplicitSharedRoots(t *testing.T) {
	root := t.TempDir()
	shared := t.TempDir()
	paths, err := WriteLayout(root, Layout{
		Packages: filepath.Join(shared, "packages"),
		Config:   filepath.Join(shared, "config"),
		State:    filepath.Join(shared, "state"),
		Users:    filepath.Join(shared, "users"),
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Node != filepath.Join(root, "node") || loaded.Kernel != filepath.Join(root, "node", "kernel") {
		t.Fatalf("node paths = %#v", loaded)
	}
	if loaded.Packages != paths.Packages || loaded.Config != paths.Config || loaded.SharedState != paths.SharedState || loaded.Users != paths.Users {
		t.Fatalf("loaded shared paths differ: %#v != %#v", loaded, paths)
	}
	if err := CheckUnixPermissions(paths.Node); err != nil {
		t.Fatal(err)
	}
}

func TestLayoutManagerUpdatesAllRootsAndRejectsOverlapWithoutReplacingLayout(t *testing.T) {
	root := t.TempDir()
	initial, err := WriteLayout(root, Layout{
		Packages: filepath.Join(root, "packages"), Config: filepath.Join(root, "config"),
		State: filepath.Join(root, "state"), Users: filepath.Join(root, "users"),
	})
	if err != nil {
		t.Fatal(err)
	}
	shared := t.TempDir()
	manager := NewLayoutManager(root)
	updated, err := manager.Set(Layout{
		Packages: filepath.Join(shared, "packages"), Config: filepath.Join(shared, "config"),
		State: filepath.Join(shared, "state"), Users: filepath.Join(shared, "users"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Packages != filepath.Join(shared, "packages") || updated.Config != filepath.Join(shared, "config") || updated.State != filepath.Join(shared, "state") || updated.Users != filepath.Join(shared, "users") {
		t.Fatalf("updated layout = %#v", updated)
	}
	if _, err := manager.Set(Layout{Packages: initial.Node, Config: updated.Config, State: updated.State, Users: updated.Users}); err == nil {
		t.Fatal("layout overlapping node directory was accepted")
	}
	if _, err := manager.Set(Layout{Packages: "relative", Config: updated.Config, State: updated.State, Users: updated.Users}); err == nil {
		t.Fatal("relative administrative layout path was accepted")
	}
	current, err := manager.Current()
	if err != nil || current != updated {
		t.Fatalf("layout changed after rejected update: %#v, %v", current, err)
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
