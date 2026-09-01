// Package runscconsole opens and owns one runsc exec console socket.
package runscconsole

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"

	"the8020/kernel/sandbox/backend"
)

type terminal struct {
	master    *os.File
	done      chan struct{}
	doneOnce  sync.Once
	closeOnce sync.Once
}

type stream struct {
	command   *exec.Cmd
	input     io.WriteCloser
	output    io.ReadCloser
	stderr    io.ReadCloser
	done      chan struct{}
	closeOnce sync.Once
	status    atomic.Uint32
}

// CommandConfigurator applies caller-owned process isolation before runsc is
// started. Runtime arguments and console lifecycle remain owned here.
type CommandConfigurator func(*exec.Cmd)

// OpenStream starts an attached runsc exec without a terminal. It preserves
// byte-transparent stdin/stdout and real half-close semantics for SSH exec.
func OpenStream(ctx context.Context, runscPath string, arguments []string) (backend.Console, error) {
	return OpenStreamConfigured(ctx, runscPath, arguments, nil)
}

// OpenStreamConfigured is OpenStream with caller-owned runsc process setup.
func OpenStreamConfigured(ctx context.Context, runscPath string, arguments []string, configure CommandConfigurator) (backend.Console, error) {
	if !filepath.IsAbs(runscPath) || len(arguments) == 0 {
		return nil, errors.New("absolute runsc path and exec arguments are required")
	}
	command := exec.CommandContext(ctx, runscPath, arguments...)
	if configure != nil {
		configure(command)
	}
	input, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	outputReader, outputWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	command.Stdout, command.Stderr = outputWriter, stderrWriter
	if err := command.Start(); err != nil {
		_ = input.Close()
		_ = outputReader.Close()
		_ = outputWriter.Close()
		_ = stderrWriter.Close()
		return nil, fmt.Errorf("start runsc stream: %w", err)
	}
	value := &stream{command: command, input: input, output: outputReader, stderr: stderrReader, done: make(chan struct{})}
	go func() {
		waitErr := command.Wait()
		if waitErr != nil {
			status := uint32(255)
			var exitError *exec.ExitError
			if errors.As(waitErr, &exitError) && exitError.ExitCode() >= 0 {
				status = uint32(exitError.ExitCode())
			}
			value.status.Store(status)
		}
		_ = outputWriter.Close()
		_ = stderrWriter.Close()
		close(value.done)
	}()
	return value, nil
}

func (s *stream) Read(data []byte) (int, error)  { return s.output.Read(data) }
func (s *stream) Write(data []byte) (int, error) { return s.input.Write(data) }
func (s *stream) CloseWrite() error              { return s.input.Close() }
func (s *stream) Stderr() io.Reader              { return s.stderr }
func (s *stream) Resize(context.Context, backend.ConsoleSize) error {
	return errors.New("non-terminal process cannot be resized")
}
func (s *stream) Done() <-chan struct{} { return s.done }
func (s *stream) ExitStatus() uint32    { return s.status.Load() }
func (s *stream) Close() error {
	var result error
	s.closeOnce.Do(func() {
		result = s.input.Close()
		if s.command.Process != nil {
			if err := s.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				result = errors.Join(result, err)
			}
		}
		result = errors.Join(result, s.output.Close())
		result = errors.Join(result, s.stderr.Close())
	})
	return result
}

// Open inserts a private console socket into an already complete runsc exec
// argument vector and returns its PTY master.
func Open(ctx context.Context, runscPath string, arguments []string, size backend.ConsoleSize) (backend.Console, error) {
	return OpenConfigured(ctx, runscPath, arguments, size, nil)
}

// OpenConfigured is Open with caller-owned runsc process setup.
func OpenConfigured(ctx context.Context, runscPath string, arguments []string, size backend.ConsoleSize, configure CommandConfigurator) (backend.Console, error) {
	if !filepath.IsAbs(runscPath) || len(arguments) == 0 {
		return nil, errors.New("absolute runsc path and exec arguments are required")
	}
	temporary, err := os.MkdirTemp("", "the8020-console-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temporary)
	socketPath := filepath.Join(temporary, "console.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen for runsc console: %w", err)
	}
	defer listener.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stopClose()
	arguments, err = withConsoleSocket(arguments, socketPath)
	if err != nil {
		return nil, err
	}
	command := exec.Command(runscPath, arguments...)
	if configure != nil {
		configure(command)
	}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start runsc console: %w", err)
	}
	processDone := make(chan error, 1)
	go func() { processDone <- command.Wait() }()
	type acceptResult struct {
		connection *net.UnixConn
		err        error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		accepted <- acceptResult{connection: connection, err: acceptErr}
	}()
	var connection *net.UnixConn
	processExited := false
	select {
	case result := <-accepted:
		connection, err = result.connection, result.err
	case processErr := <-processDone:
		processExited = true
		if processErr != nil {
			_ = listener.Close()
			return nil, fmt.Errorf("runsc console exited before connecting: %v: %s", processErr, strings.TrimSpace(output.String()))
		}
		select {
		case result := <-accepted:
			connection, err = result.connection, result.err
		case <-ctx.Done():
			_ = listener.Close()
			return nil, ctx.Err()
		}
	case <-ctx.Done():
		_ = listener.Close()
		_ = command.Process.Kill()
		<-processDone
		return nil, ctx.Err()
	}
	if err != nil {
		if !processExited {
			_ = command.Process.Kill()
			<-processDone
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("accept runsc console: %w: %s", err, strings.TrimSpace(output.String()))
	}
	master, receiveErr := receiveConsoleFile(connection)
	_ = connection.Close()
	if receiveErr != nil {
		if !processExited {
			_ = command.Process.Kill()
			<-processDone
		}
		return nil, receiveErr
	}
	value := &terminal{master: master, done: make(chan struct{})}
	if !processExited {
		go func() { <-processDone }()
	}
	if err := value.Resize(ctx, size); err != nil {
		_ = value.Close()
		return nil, fmt.Errorf("size runsc console: %w", err)
	}
	return value, nil
}

func withConsoleSocket(arguments []string, socketPath string) ([]string, error) {
	for index, argument := range arguments {
		if argument != "exec" {
			continue
		}
		result := append([]string(nil), arguments[:index+1]...)
		result = append(result, "--console-socket="+socketPath, "--detach")
		result = append(result, arguments[index+1:]...)
		return result, nil
	}
	return nil, errors.New("runsc console arguments do not contain exec")
}

func receiveConsoleFile(connection *net.UnixConn) (*os.File, error) {
	data := make([]byte, 1)
	rights := make([]byte, unix.CmsgSpace(4))
	_, controlBytes, _, _, err := connection.ReadMsgUnix(data, rights)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("receive runsc console: %w", err)
	}
	messages, err := unix.ParseSocketControlMessage(rights[:controlBytes])
	if err != nil {
		return nil, fmt.Errorf("parse runsc console: %w", err)
	}
	for _, message := range messages {
		files, parseErr := unix.ParseUnixRights(&message)
		if parseErr != nil {
			return nil, fmt.Errorf("parse runsc console descriptor: %w", parseErr)
		}
		if len(files) == 0 {
			continue
		}
		for _, extra := range files[1:] {
			_ = unix.Close(extra)
		}
		return os.NewFile(uintptr(files[0]), "sandbox-console"), nil
	}
	return nil, errors.New("runsc did not provide a console descriptor")
}

func (t *terminal) Read(buffer []byte) (int, error) {
	count, err := t.master.Read(buffer)
	if errors.Is(err, unix.EIO) {
		err = io.EOF
	}
	if err != nil {
		t.doneOnce.Do(func() { close(t.done) })
	}
	return count, err
}

func (t *terminal) Write(data []byte) (int, error) { return t.master.Write(data) }
func (t *terminal) Stderr() io.Reader              { return nil }

func (t *terminal) CloseWrite() error {
	_, err := t.master.Write([]byte{0x04})
	return err
}

func (t *terminal) Resize(_ context.Context, size backend.ConsoleSize) error {
	return unix.IoctlSetWinsize(int(t.master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Col: uint16(size.Columns), Row: uint16(size.Rows),
	})
}

func (t *terminal) Done() <-chan struct{} { return t.done }

func (t *terminal) Close() error {
	var closeErr error
	t.closeOnce.Do(func() {
		closeErr = t.master.Close()
		t.doneOnce.Do(func() { close(t.done) })
	})
	return closeErr
}
