package rootless

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"the8020/kernel/logging/records"
	"the8020/kernel/sandbox/backend"
)

// Command distinguishes native process output from bounded control responses.
// Output is mandatory for detached run: inherited descriptors outlive the kernel.
type Command struct {
	Path, SandboxID string
	Arguments       []string
	Output          records.RawPaths
	Timeout         time.Duration
}

type execRunner struct{ logger *slog.Logger }

func (r execRunner) Run(ctx context.Context, request Command) ([]byte, error) {
	timeout := request.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(ctx, request.Path, request.Arguments...)
	command.WaitDelay = 250 * time.Millisecond
	if rootlessCommand(request.Arguments) == "run" && containsArgument(request.Arguments, "--detach") {
		stdout, err := backend.OpenRawOutput(request.Output.Stdout)
		if err != nil {
			return nil, err
		}
		defer stdout.Close()
		stderr, err := backend.OpenRawOutput(request.Output.Stderr)
		if err != nil {
			return nil, err
		}
		defer stderr.Close()
		command.Stdout, command.Stderr = stdout, stderr
		if err := command.Run(); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(request.Path), errors.Join(ctx.Err(), err))
		}
		return nil, nil
	}

	// Short control processes may need their JSON stdout and stderr error text.
	// Drain both through fixed budgets, even after overflow; never CombinedOutput.
	stdout, stderr := backend.NewOutputBuffer(64<<10), backend.NewOutputBuffer(16<<10)
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	if stderr.Total() > 0 && r.logger != nil {
		r.logger.Warn("runtime command diagnostics: "+string(stderr.Bytes()), "sandbox_id", request.SandboxID,
			"command", rootlessCommand(request.Arguments), "stream", "stderr")
	}
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("%s: %w: %s", filepath.Base(request.Path), errors.Join(ctx.Err(), err), strings.TrimSpace(string(stderr.Bytes())))
	}
	if stdout.Truncated() {
		return nil, errors.New("runtime control response exceeds 64 KiB")
	}
	return stdout.Bytes(), nil
}
