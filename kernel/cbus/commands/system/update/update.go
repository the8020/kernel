// Package update launches the installed kernel updater independently of the kernel.
package update

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, request core.Request) (core.Result, error) {
		version := commandutil.String(request, "version")
		if !versionPattern.MatchString(version) {
			return nil, core.NewError(core.CodeInvalidArguments, "version must be exactly major.minor.patch")
		}
		paths := serviceSet.Instance.Paths
		script := filepath.Join(paths.Kernel, "scripts", "update.sh")
		if info, err := os.Stat(script); err != nil || !info.Mode().IsRegular() {
			return nil, core.NewError(core.CodeNotFound, "the installed kernel updater is unavailable")
		}
		launcher, err := os.ReadFile(filepath.Join(paths.Run, "launcher.pid"))
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(launcher)))
		if err != nil || parseErr != nil || pid <= 0 || syscall.Kill(pid, 0) != nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "kernel updates require a bootstrapped release instance started by launch.sh")
		}
		logPath := filepath.Join(paths.Kernel, "update.log")
		log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, core.NewError(core.CodePersistence, fmt.Sprintf("open updater log: %v", err))
		}
		defer log.Close()
		command := exec.Command("bash", script, version)
		command.Env = append(os.Environ(), "THE8020_INSTANCE_ROOT="+paths.Root)
		command.Stdout, command.Stderr = log, log
		command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := command.Start(); err != nil {
			return nil, core.NewError(core.CodeRuntimeOperation, fmt.Sprintf("start updater: %v", err))
		}
		go func() { _ = command.Wait() }()
		return core.Result{"requested": true, "pid": command.Process.Pid, "log_file": logPath}, nil
	}
}
