package update

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"the8020/kernel/cbus/core"
	"the8020/kernel/instance"
	"the8020/kernel/services"

	"golang.org/x/sys/unix"
)

func TestDetachedUpdaterSurvivesRequestCancellation(t *testing.T) {
	paths := instance.NewPaths(t.TempDir())
	for _, directory := range []string{paths.Run, filepath.Join(paths.Kernel, "scripts")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(paths.Run, "launcher.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/bash\nset -eu\nprintf '%s %s\\n' \"$1\" \"$THE8020_INSTANCE_ROOT\"\nwhile [[ ! -f \"$THE8020_INSTANCE_ROOT/release\" ]]; do sleep 0.02; done\necho finished\n"
	if err := os.WriteFile(filepath.Join(paths.Kernel, "scripts", "update.sh"), []byte(script), 0o500); err != nil {
		t.Fatal(err)
	}
	service := &services.Services{Instance: services.InstanceInfo{Paths: paths}}
	ctx, cancel := context.WithCancel(context.Background())
	result, err := New(service)(ctx, core.Request{Arguments: map[string]any{"version": "0.8.1"}})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	pid := result["pid"].(int)
	t.Cleanup(func() { _ = unix.Kill(pid, unix.SIGKILL) })
	if sid, err := unix.Getsid(pid); err != nil || sid != pid {
		t.Fatalf("updater session = %d, %v", sid, err)
	}
	logPath := result["log_file"].(string)
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "0.8.1 "+paths.Root) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("updater did not start: %s", data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if info, err := os.Stat(logPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("log permissions: %v, %v", info, err)
	}
	if err := os.WriteFile(filepath.Join(paths.Root, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "finished") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("canceled request stopped updater")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUpdateRejectsInvalidOrUnmanagedRequests(t *testing.T) {
	paths := instance.NewPaths(t.TempDir())
	service := &services.Services{Instance: services.InstanceInfo{Paths: paths}}
	for _, version := range []string{"", "0.8", "00.8.1", "0.8.1;touch /tmp/bad"} {
		if _, err := New(service)(context.Background(), core.Request{Arguments: map[string]any{"version": version}}); err == nil {
			t.Fatalf("accepted %q", version)
		}
	}
	if _, err := New(service)(context.Background(), core.Request{Arguments: map[string]any{"version": "0.8.1"}}); err == nil {
		t.Fatal("accepted missing updater")
	}
	if err := os.MkdirAll(filepath.Join(paths.Kernel, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Kernel, "scripts", "update.sh"), nil, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := New(service)(context.Background(), core.Request{Arguments: map[string]any{"version": "0.8.1"}}); err == nil {
		t.Fatal("accepted missing launcher")
	}
}
