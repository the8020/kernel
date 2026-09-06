package rootless

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"the8020/kernel/sandbox/backend"
)

func TestNativeOutputFixture(t *testing.T) {
	mode := os.Getenv("THE8020_NATIVE_LOGGING_FIXTURE")
	if mode == "" {
		return
	}
	switch mode {
	case "native":
		for _, stream := range []struct {
			file *os.File
			path string
		}{{os.Stdout, os.Getenv("THE8020_NATIVE_STDOUT")}, {os.Stderr, os.Getenv("THE8020_NATIVE_STDERR")}} {
			actual, err := stream.file.Stat()
			expected, pathErr := os.Stat(stream.path)
			if err != nil || pathErr != nil || !os.SameFile(actual, expected) || actual.Mode()&os.ModeNamedPipe == 0 {
				os.Exit(2)
			}
		}
		fmt.Fprint(os.Stdout, "native stdout\n")
		fmt.Fprint(os.Stderr, "native stderr\n")
	case "stderr":
		fmt.Fprint(os.Stdout, `{"status":"running"}`)
		fmt.Fprint(os.Stderr, "HEAD-")
		chunk := bytes.Repeat([]byte("x"), 1024)
		for i := 0; i < 1024; i++ {
			_, _ = os.Stderr.Write(chunk)
		}
		fmt.Fprint(os.Stderr, "-TAIL")
	case "stdout":
		chunk := bytes.Repeat([]byte("x"), 1024)
		for i := 0; i < 1024; i++ {
			_, _ = os.Stdout.Write(chunk)
		}
	case "wait":
		time.Sleep(10 * time.Second)
	}
	os.Exit(0)
}

func TestDetachedRunnerInheritsRegisteredFIFOsDirectly(t *testing.T) {
	paths := testRawPaths(t)
	t.Setenv("THE8020_NATIVE_LOGGING_FIXTURE", "native")
	t.Setenv("THE8020_NATIVE_STDOUT", paths.Stdout)
	t.Setenv("THE8020_NATIVE_STDERR", paths.Stderr)
	var reads []*os.File
	for _, path := range []string{paths.Stdout, paths.Stderr} {
		file, err := os.OpenFile(path, os.O_RDWR|unix.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		reads = append(reads, file)
	}
	_, err := (execRunner{}).Run(context.Background(), Command{
		Path: os.Args[0], Arguments: []string{"-test.run=^TestNativeOutputFixture$", "run", "--detach"},
		SandboxID: "sbx-0123456789", Output: paths,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, wanted := range []string{"native stdout\n", "native stderr\n"} {
		_ = reads[i].SetReadDeadline(time.Now().Add(time.Second))
		data := make([]byte, len(wanted))
		if _, err := io.ReadFull(reads[i], data); err != nil || string(data) != wanted {
			t.Fatalf("native output=%q error=%v", data, err)
		}
		if _, err := os.Lstat(reads[i].Name()); err != nil {
			t.Fatal("runner removed logger-owned FIFO:", err)
		}
	}
}

func TestControlRunnerBoundsDiagnosticsAndPreservesJSON(t *testing.T) {
	t.Setenv("THE8020_NATIVE_LOGGING_FIXTURE", "stderr")
	var diagnostics bytes.Buffer
	runner := execRunner{logger: slog.New(slog.NewTextHandler(&diagnostics, nil))}
	command := Command{Path: os.Args[0], Arguments: []string{"-test.run=^TestNativeOutputFixture$", "state"}, SandboxID: "sbx-0123456789"}
	output, err := runner.Run(context.Background(), command)
	if err != nil || string(output) != `{"status":"running"}` {
		t.Fatalf("stdout=%q error=%v", output, err)
	}
	text := diagnostics.String()
	if len(text) > 17<<10 || !strings.Contains(text, "HEAD-") || !strings.Contains(text, "-TAIL") || !strings.Contains(text, "bytes omitted") {
		t.Fatalf("diagnostic framing invalid (%d bytes)", len(text))
	}
	t.Setenv("THE8020_NATIVE_LOGGING_FIXTURE", "stdout")
	if _, err := runner.Run(context.Background(), command); err == nil || !strings.Contains(err.Error(), "exceeds 64 KiB") {
		t.Fatalf("oversized response error=%v", err)
	}
	t.Setenv("THE8020_NATIVE_LOGGING_FIXTURE", "wait")
	command.Timeout = 50 * time.Millisecond
	if _, err := runner.Run(context.Background(), command); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled command error=%v", err)
	}
}

func TestRawOutputRejectsAbsentReaderAndReplacedEndpoint(t *testing.T) {
	paths := testRawPaths(t)
	if file, err := backend.OpenRawOutput(paths.Stdout); err == nil {
		_ = file.Close()
		t.Fatal("FIFO without a reader was admitted")
	}
	if err := os.Remove(paths.Stdout); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Stdout, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if file, err := backend.OpenRawOutput(paths.Stdout); err == nil {
		_ = file.Close()
		t.Fatal("regular file was admitted")
	}
	if data, err := os.ReadFile(paths.Stdout); err != nil || string(data) != "unchanged" {
		t.Fatalf("unrelated file changed: %q %v", data, err)
	}
}
