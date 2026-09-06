package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

var appTestLogd string

// Managed Unix endpoints live below the instance. Keep disposable fixture roots
// short enough for the kernel's fixed socket layout and Unix pathname bound.
func testInstanceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "app-instance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "app-logd-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	appTestLogd = filepath.Join(dir, "logd")
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", appTestLogd, "../logd")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
