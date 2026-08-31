package runscconsole

import (
	"reflect"
	"testing"
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
