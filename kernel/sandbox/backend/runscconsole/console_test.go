package runscconsole

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWithConsoleSocketAddsDetachedExecOptions(t *testing.T) {
	original := []string{
		"--root=/runtime", "--rootless=true", "exec", "--cwd=/workspace",
		"sandbox-1", "/bin/bash", "-l",
	}
	arguments, err := withConsoleSocket(original, "/tmp/console.sock")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--root=/runtime", "--rootless=true", "exec",
		"--console-socket=/tmp/console.sock", "--detach", "--cwd=/workspace",
		"sandbox-1", "/bin/bash", "-l",
	}
	if !reflect.DeepEqual(arguments, want) {
		t.Fatalf("arguments = %#v, want %#v", arguments, want)
	}
	if original[3] != "--cwd=/workspace" {
		t.Fatalf("input arguments were mutated: %#v", original)
	}
	if _, err := withConsoleSocket([]string{"state", "sandbox-1"}, "/tmp/console.sock"); err == nil {
		t.Fatal("arguments without exec were accepted")
	}
}

func TestReceivedConsoleCloseInterruptsIdleRead(t *testing.T) {
	// runsc donates a blocking descriptor through SCM_RIGHTS. Keeping the peer
	// open reproduces an idle terminal that has no more output to unblock Read.
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "console.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	sender, err := net.DialUnix("unix", nil, listener.Addr().(*net.UnixAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	receiver, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	pipe := make([]int, 2)
	if err := unix.Pipe(pipe); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pipe[1])
	if _, _, err := sender.WriteMsgUnix([]byte{0}, unix.UnixRights(pipe[0]), nil); err != nil {
		unix.Close(pipe[0])
		t.Fatal(err)
	}
	unix.Close(pipe[0])
	file, err := receiveConsoleFile(receiver)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	first := make(chan error, 1)
	finished := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := file.Read(buf)
		first <- err
		_, err = file.Read(buf)
		finished <- err
	}()
	if _, err := unix.Write(pipe[1], []byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	// Allow the second read to block before testing Close; no peer EOF is sent.
	time.Sleep(20 * time.Millisecond)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, os.ErrClosed) {
			t.Fatalf("idle read after close = %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("closing the donated console left its idle read blocked")
	}
}
