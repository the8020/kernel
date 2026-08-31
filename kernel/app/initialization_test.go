package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"the8020/kernel/instance"
)

func TestInteractiveInitializationSelectsEveryRootAndCommitsAfterValidation(t *testing.T) {
	current := t.TempDir()
	selected := filepath.Join(t.TempDir(), "selected")
	shared := t.TempDir()
	input := strings.Join([]string{
		"yes", selected,
		filepath.Join(shared, "packages"),
		filepath.Join(shared, "config"),
		filepath.Join(shared, "users"),
		filepath.Join(shared, "state"),
	}, "\n") + "\n"
	var output bytes.Buffer
	root, err := initializeInteractive(current, strings.NewReader(input), &output)
	if err != nil {
		t.Fatal(err)
	}
	if root != selected {
		t.Fatalf("selected root = %q", root)
	}
	paths, err := instance.LoadPaths(selected)
	if err != nil {
		t.Fatal(err)
	}
	if paths.Packages != filepath.Join(shared, "packages") || paths.Config != filepath.Join(shared, "config") || paths.Users != filepath.Join(shared, "users") || paths.SharedState != filepath.Join(shared, "state") {
		t.Fatalf("initialized paths = %#v", paths)
	}
	entries, err := os.ReadDir(paths.Users)
	if err != nil || len(entries) != 0 {
		t.Fatalf("new users root = %v, %v", entries, err)
	}
	for _, prompt := range []string{"Initialization directory", "Packages directory", "System configuration directory", "User data directory", "System state directory"} {
		if !strings.Contains(output.String(), prompt) {
			t.Fatalf("missing prompt %q in %q", prompt, output.String())
		}
	}
}

func TestCancelledInitializationLeavesNoCommittedLayout(t *testing.T) {
	root := t.TempDir()
	_, err := initializeInteractive(root, strings.NewReader("no\n"), &bytes.Buffer{})
	if err == nil {
		t.Fatal("cancelled initialization succeeded")
	}
	if _, err := instance.LoadPaths(root); !errors.Is(err, instance.ErrNotInitialized) {
		t.Fatalf("layout exists after cancellation: %v", err)
	}
}
