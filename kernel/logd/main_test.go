package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"the8020/kernel/logging/records"
)

func TestLogdChild(t *testing.T) {
	if os.Getenv("THE8020_LOGD_TEST_CHILD") != "1" {
		return
	}
	os.Args = []string{"logd"}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestInheritedControlAndRawDescriptors(t *testing.T) {
	dir, err := os.MkdirTemp("", "logd-native-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "parent-control")
	childFile := os.NewFile(uintptr(fds[1]), "child-control")
	defer childFile.Close()
	parent, err := net.FileConn(parentFile)
	_ = parentFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer outRead.Close()
	defer outWrite.Close()
	errRead, errWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer errRead.Close()
	defer errWrite.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLogdChild$")
	cmd.Env = append(os.Environ(), "THE8020_LOGD_TEST_CHILD=1")
	cmd.ExtraFiles = []*os.File{childFile, outRead, errRead}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	_ = childFile.Close()
	_ = outRead.Close()
	_ = errRead.Close()
	initial := records.Initialization{Version: records.ProtocolVersion, Directory: filepath.Join(dir, "logs"), Socket: filepath.Join(dir, "api", "logs.sock"), NodeID: "nod-0123456789", MaxProducers: 8, Policy: records.Policy{Enabled: true, Level: "info", SplitBy: "none", SplitPeriod: "day", MaxFileSize: 128 * 1024, MaxTotalSize: 1024 * 1024, MaxAge: time.Hour}}
	data, _ := json.Marshal(initial)
	_ = parent.SetDeadline(time.Now().Add(3 * time.Second))
	if err = records.WriteFrame(parent, data); err != nil {
		t.Fatal(err)
	}
	if _, err = records.ReadControlFrame(parent); err != nil {
		t.Fatal("inherited control descriptor failed", err)
	}
	if _, err = outWrite.Write([]byte("kernel native stdout\n")); err != nil {
		t.Fatal(err)
	}
	_ = parent.Close()
	if _, err = errWrite.Write([]byte("kernel final stack 💡")); err != nil {
		t.Fatal(err)
	}
	_ = outWrite.Close()
	_ = errWrite.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err, diagnostics.String())
		}
	case <-time.After(4 * time.Second):
		t.Fatal("native logd did not drain inherited descriptors")
	}
	paths, err := filepath.Glob(filepath.Join(initial.Directory, "segment-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.SplitAfter(data, []byte{'\n'}) {
			if len(line) == 0 {
				continue
			}
			r, err := records.Decode(line)
			if err != nil {
				t.Fatal(err)
			}
			if r.Source == "kernel" {
				found[r.Message] = true
				if r.WorkerID != "" || r.ContextID != "" {
					t.Fatal("invented raw identity")
				}
				if strings.Contains(r.Message, "final stack") && (r.Stream != "stderr" || r.Attributes["unterminated"] != "true") {
					t.Fatal("final native stack lost metadata")
				}
			}
		}
	}
	if !found["kernel native stdout"] || !found["kernel final stack 💡"] {
		t.Fatal("native descriptor drain lost output", found)
	}
}
