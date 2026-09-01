package development

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"the8020/kernel/auth"
	"the8020/kernel/sandbox/backend"
	"the8020/kernel/sandbox/backend/runscconsole"
)

type RunscConfig struct {
	RunscPath   string
	RuntimeRoot string
	SandboxRoot string
	LogRoot     string
	Rootless    bool
	// ignoreCgroups is limited to reduced-environment driver verification.
	// Production full mode leaves this false and requires real cgroup authority.
	ignoreCgroups bool
}

type RootlessConfig = RunscConfig

type RunscDriver struct{ config RunscConfig }
type RootlessDriver = RunscDriver

const developmentPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// A conventional OCI user namespace maps 65536 IDs. Mapping that complete
// identity range lets native package managers preserve service-user ownership
// while runsc and its gofer remain confined to a child user namespace.
const rootlessIDMapSize = 1 << 16

var developmentRootCapabilities = []string{
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_FOWNER",
	"CAP_FSETID",
	"CAP_KILL",
	"CAP_MKNOD",
	"CAP_SETFCAP",
	"CAP_SETGID",
	"CAP_SETPCAP",
	"CAP_SETUID",
	"CAP_SYS_CHROOT",
}

func NewRootlessDriver(config RootlessConfig) *RunscDriver {
	config.Rootless = true
	return &RunscDriver{config: config}
}

func NewRunscDriver(config RunscConfig) *RunscDriver {
	return &RunscDriver{config: config}
}

func (d *RunscDriver) validate() error {
	for name, path := range map[string]string{"runsc": d.config.RunscPath, "runtime root": d.config.RuntimeRoot, "sandbox root": d.config.SandboxRoot, "log root": d.config.LogRoot} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("development %s must be absolute", name)
		}
	}
	if info, err := os.Stat(d.config.RunscPath); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return errors.New("development runsc driver requires executable runsc")
	}
	for _, directory := range []string{d.config.RuntimeRoot, d.config.SandboxRoot, d.config.LogRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (d *RunscDriver) List(ctx context.Context) ([]string, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	args := []string{"--root=" + d.config.RuntimeRoot, "--rootless=" + strconv.FormatBool(d.config.Rootless), "--platform=systrap", "list", "--format=json"}
	output, err := d.commandOutput(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("list development sandboxes: %w: %s", err, strings.TrimSpace(string(output)))
	}
	var states []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(output), &states); err != nil {
		return nil, fmt.Errorf("decode development sandbox list: %w", err)
	}
	ids := make([]string, 0, len(states))
	for _, state := range states {
		if validDevelopmentSandboxID(state.ID) {
			ids = append(ids, state.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (d *RunscDriver) Start(ctx context.Context, start SandboxStart) error {
	if err := d.validate(); err != nil {
		return err
	}
	for name, path := range map[string]string{"packages": start.Packages, "rootfs": start.RootFS} {
		canonical, err := canonicalDirectory(path)
		if err != nil {
			return fmt.Errorf("development %s: %w", name, err)
		}
		switch name {
		case "packages":
			start.Packages = canonical
		case "rootfs":
			start.RootFS = canonical
		}
	}
	if len(start.Mounts) == 0 {
		return errors.New("development sandbox requires a validated mount profile")
	}
	for index := range start.Mounts {
		mount := &start.Mounts[index]
		if !safeMountID(mount.ID) || !safeSandboxMountTarget(mount.Target) {
			return fmt.Errorf("development sandbox mount %q is invalid", mount.ID)
		}
		if mount.Behavior == MountEphemeral {
			if mount.HostSource != "" {
				return fmt.Errorf("ephemeral development mount %s has a host source", mount.ID)
			}
			continue
		}
		canonical, err := canonicalDirectory(mount.HostSource)
		if err != nil {
			return fmt.Errorf("development mount %s source: %w", mount.ID, err)
		}
		mount.HostSource = canonical
	}
	if !validDevelopmentSandboxID(start.SandboxID) {
		return errors.New("development sandbox ID must use the dev-<user> resource format")
	}
	path := filepath.Join(d.config.SandboxRoot, start.SandboxID)
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("development sandbox %s already exists", start.SandboxID)
	}
	bundle, logs := filepath.Join(path, "bundle"), filepath.Join(d.config.LogRoot, start.SandboxID)
	for _, directory := range []string{bundle, logs, d.config.RuntimeRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			_ = os.RemoveAll(path)
			return err
		}
	}
	for _, source := range []string{"/etc/resolv.conf", "/etc/hosts"} {
		if err := copySandboxNetworkFile(source, filepath.Join(bundle, filepath.Base(source))); err != nil {
			_ = os.RemoveAll(path)
			return err
		}
	}
	spec := developmentSpec(start, bundle)
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		_ = os.RemoveAll(path)
		return err
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), append(data, '\n'), 0o600); err != nil {
		_ = os.RemoveAll(path)
		return err
	}
	stdout, err := os.OpenFile(filepath.Join(logs, "process.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		_ = os.RemoveAll(path)
		return err
	}
	defer stdout.Close()
	args := append(d.flags(start.SandboxID, "run"), "--detach", "--bundle="+bundle, "--user-log="+filepath.Join(logs, "user.log"), start.SandboxID)
	command := d.commandContext(ctx, args...)
	command.Stdout, command.Stderr = stdout, stdout
	if err := command.Run(); err != nil {
		_ = stdout.Sync()
		detail, _ := os.ReadFile(filepath.Join(logs, "process.log"))
		if len(detail) > commandOutputLimit {
			detail = append(detail[:commandOutputLimit], []byte("\n[output truncated]")...)
		}
		_ = d.Delete(context.Background(), start.SandboxID)
		if message := strings.TrimSpace(string(detail)); message != "" {
			return fmt.Errorf("start development gVisor sandbox: %w: %s", err, message)
		}
		return fmt.Errorf("start development gVisor sandbox: %w", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		running, stateErr := d.Running(ctx, start.SandboxID)
		if stateErr == nil && running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	_ = d.Delete(context.Background(), start.SandboxID)
	return errors.New("development sandbox did not reach running state")
}

func developmentSpec(start SandboxStart, bundle string) specs.Spec {
	mounts := []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/run", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=755", "size=65536k"}},
		{Destination: "/etc/resolv.conf", Type: "bind", Source: filepath.Join(bundle, "resolv.conf"), Options: []string{"bind", "ro", "nosuid", "nodev", "noexec"}},
		{Destination: "/etc/hosts", Type: "bind", Source: filepath.Join(bundle, "hosts"), Options: []string{"bind", "ro", "nosuid", "nodev", "noexec"}},
	}
	for _, mount := range start.Mounts {
		if mount.Behavior == MountEphemeral {
			mounts = append(mounts, specs.Mount{Destination: mount.Target, Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=1777", "size=536870912"}})
			continue
		}
		mode := "ro"
		if mount.Writable {
			mode = "rw"
		}
		options := []string{"rbind", mode, "nosuid", "nodev", "rprivate"}
		if !mount.Writable {
			options = append(options, "noexec")
		}
		mounts = append(mounts, specs.Mount{Destination: mount.Target, Type: "bind", Source: mount.HostSource, Options: options})
	}
	capabilities := append([]string(nil), developmentRootCapabilities...)
	spec := specs.Spec{Version: specs.Version, Process: &specs.Process{
		Terminal: false, User: specs.User{UID: 0, GID: 0}, Args: []string{"/bin/bash", "/opt/development/sandbox.sh"},
		Env: []string{"PATH=" + developmentPath, "HOME=/root", "USER=root", "LOGNAME=root", "DENO_DIR=/root/.cache/deno", "DENO_NO_UPDATE_CHECK=1", "DENO_NO_PROMPT=1", "DEVELOPMENT_WORKSPACE_ID=" + start.WorkspaceID, "DEVELOPMENT_ACTIVATION_ENDPOINT=" + start.Endpoint, "DEVELOPMENT_ACTIVATION_TOKEN=" + start.Token},
		Cwd: "/workspace", Capabilities: &specs.LinuxCapabilities{Bounding: capabilities, Effective: capabilities, Permitted: capabilities}, NoNewPrivileges: true,
	}, Root: &specs.Root{Path: start.RootFS, Readonly: false}, Hostname: start.SandboxID, Mounts: mounts,
		Linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{{Type: specs.PIDNamespace}, {Type: specs.IPCNamespace}, {Type: specs.UTSNamespace}, {Type: specs.MountNamespace}}, MaskedPaths: []string{"/proc/acpi", "/proc/kcore", "/proc/keys", "/proc/timer_list", "/sys/firmware"}, ReadonlyPaths: []string{"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"}},
	}
	return spec
}

func (d *RunscDriver) Exec(ctx context.Context, sandboxID, commandText string) ([]byte, error) {
	if strings.TrimSpace(commandText) == "" {
		commandText = "exec /bin/bash"
	}
	args := append(d.flags(sandboxID, "exec"), "--cwd=/workspace", "--env=HOME=/root", "--env=USER=root", "--env=LOGNAME=root", "--env=PATH="+developmentPath, sandboxID, "/bin/bash", "-lc", commandText)
	command := d.commandContext(ctx, args...)
	output := &boundedBuffer{limit: commandOutputLimit}
	command.Stdout, command.Stderr = output, output
	if err := command.Run(); err != nil {
		return []byte(output.String()), fmt.Errorf("development sandbox exec: %w: %s", err, output.String())
	}
	return []byte(output.RawString()), nil
}

func (d *RunscDriver) OpenConsole(ctx context.Context, sandboxID string, options backend.ConsoleOptions) (backend.Console, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	if !validDevelopmentSandboxID(sandboxID) {
		return nil, errors.New("safe development sandbox ID is required")
	}
	if err := backend.ValidateConsoleOptions(options); err != nil {
		return nil, err
	}
	running, err := d.Running(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	if !running {
		return nil, errors.New("development sandbox is not running")
	}
	arguments := []string{"--cwd=" + options.WorkingDir}
	for _, environment := range options.Environment {
		arguments = append(arguments, "--env="+environment)
	}
	arguments = append(arguments, sandboxID)
	arguments = append(arguments, options.Arguments...)
	arguments = append(d.flags(sandboxID, "exec"), arguments...)
	if !options.Terminal {
		return runscconsole.OpenStreamConfigured(ctx, d.config.RunscPath, arguments, d.configureCommand)
	}
	return runscconsole.OpenConfigured(ctx, d.config.RunscPath, arguments, options.Size, d.configureCommand)
}

func (d *RunscDriver) Pause(ctx context.Context, id string) error {
	return d.simple(ctx, id, "pause")
}
func (d *RunscDriver) Resume(ctx context.Context, id string) error {
	return d.simple(ctx, id, "resume")
}
func (d *RunscDriver) Stop(ctx context.Context, id string) error {
	if err := d.signal(ctx, id, "TERM"); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		running, _ := d.Running(ctx, id)
		if !running {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return d.signal(ctx, id, "KILL")
}
func (d *RunscDriver) Kill(ctx context.Context, id string) error { return d.signal(ctx, id, "KILL") }
func (d *RunscDriver) Delete(ctx context.Context, id string) error {
	if !validDevelopmentSandboxID(id) {
		return errors.New("safe development sandbox ID is required")
	}
	args := append(d.flags(id, "delete"), "--force", id)
	output, err := d.commandOutput(ctx, args...)
	if err != nil && !strings.Contains(output, "does not exist") && !strings.Contains(output, "not found") {
		return fmt.Errorf("delete development sandbox: %w: %s", err, output)
	}
	return errors.Join(
		os.RemoveAll(filepath.Join(d.config.SandboxRoot, id)),
		os.RemoveAll(filepath.Join(d.config.LogRoot, id)),
	)
}
func (d *RunscDriver) Running(ctx context.Context, id string) (bool, error) {
	if !validDevelopmentSandboxID(id) {
		return false, errors.New("safe development sandbox ID is required")
	}
	args := append(d.flags(id, "state"), id)
	output, err := d.commandOutput(ctx, args...)
	if err != nil {
		if strings.Contains(output, "does not exist") || strings.Contains(output, "not found") {
			return false, nil
		}
		return false, err
	}
	var state struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(output), &state); err != nil {
		return false, err
	}
	return state.Status == "running", nil
}
func (d *RunscDriver) signal(ctx context.Context, id, signal string) error {
	args := append(d.flags(id, "kill"), "--all", id, signal)
	output, err := d.commandOutput(ctx, args...)
	if err != nil && !strings.Contains(output, "does not exist") && !strings.Contains(output, "not found") && !strings.Contains(output, "connection refused") {
		return fmt.Errorf("signal development sandbox: %w: %s", err, output)
	}
	return nil
}
func (d *RunscDriver) simple(ctx context.Context, id, operation string) error {
	args := append(d.flags(id, operation), id)
	output, err := d.commandOutput(ctx, args...)
	if err != nil {
		return fmt.Errorf("%s development sandbox: %w: %s", operation, err, output)
	}
	return nil
}
func (d *RunscDriver) flags(id, operation string) []string {
	flags := []string{"--root=" + d.config.RuntimeRoot, "--rootless=" + strconv.FormatBool(d.config.Rootless), "--platform=systrap", "--directfs=false", "--file-access=exclusive", "--file-access-mounts=shared", "--network=host", "--overlay2=none", "--log=" + filepath.Join(d.config.LogRoot, id, "runsc-"+operation+".log")}
	if d.config.ignoreCgroups {
		flags = append(flags, "--ignore-cgroups=true")
	}
	return append(flags, operation)
}

func (d *RunscDriver) commandContext(ctx context.Context, arguments ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, d.config.RunscPath, arguments...)
	d.configureCommand(command)
	return command
}

func (d *RunscDriver) commandOutput(ctx context.Context, arguments ...string) (string, error) {
	command := d.commandContext(ctx, arguments...)
	output := &boundedBuffer{limit: commandOutputLimit}
	command.Stdout, command.Stderr = output, output
	err := command.Run()
	return output.String(), err
}

func (d *RunscDriver) configureCommand(command *exec.Cmd) {
	if !d.config.Rootless {
		return
	}
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: 0, Size: rootlessIDMapSize}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: 0, Size: rootlessIDMapSize}},
		Credential:  &syscall.Credential{Uid: 0, Gid: 0},
		Pdeathsig:   syscall.SIGKILL,
		Setsid:      true,
	}
}

func copySandboxNetworkFile(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, data, 0o644)
}

func canonicalDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	value, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(value)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	return filepath.Clean(value), nil
}
func safeRuntimeID(value string) bool {
	if value == "" || len(value) > 120 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func validDevelopmentSandboxID(value string) bool {
	username, found := strings.CutPrefix(value, "dev-")
	return found && auth.ValidateUsername(username) == nil
}
