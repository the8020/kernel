// Package rootless implements direct rootless gVisor sandbox lifecycle.
package rootless

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"the8020/kernel/sandbox/backend"
	"the8020/kernel/sandbox/backend/runscconsole"
	"the8020/kernel/sandbox/model"
)

const (
	labelManaged      = "the8020.runtime.managed"
	labelInstance     = "the8020.runtime.instance_uuid"
	labelRuntimeGroup = "the8020.runtime.group_id"
	labelWorkloadType = "the8020.runtime.workload_type"
	labelProfileHash  = "the8020.runtime.profile_hash"
	labelImageDigest  = "the8020.runtime.image_digest"
	labelOwner        = "the8020.owner"
	labelOwners       = "the8020.owners"
	labelServices     = "the8020.services"
	labelGroupKey     = "the8020.group_key"
	labelAssignedAt   = "the8020.assigned_at"
)

type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type Config struct {
	RunscPath                   string
	RootFS                      string
	StateRoot                   string
	RuntimeRoot                 string
	LogRoot                     string
	InstanceUUID                string
	CallbackAddress             string
	SupervisorHeartbeatInterval time.Duration
	WorkerStopGrace             time.Duration
	StartTimeout                time.Duration
	Runner                      CommandRunner
	Logger                      *slog.Logger
}

type Backend struct {
	mu                          sync.Mutex
	runscPath                   string
	rootFS                      string
	stateRoot                   string
	runtimeRoot                 string
	logRoot                     string
	instanceUUID                string
	callbackAddress             string
	supervisorHeartbeatInterval time.Duration
	workerStopGrace             time.Duration
	startTimeout                time.Duration
	runner                      CommandRunner
	logger                      *slog.Logger
	subreaper                   bool
	procRoot                    string
	consoleEnabled              bool
}

type metadata struct {
	SandboxID      string            `json:"sandbox_id"`
	RuntimeGroupID string            `json:"runtime_group_id"`
	InstanceUUID   string            `json:"instance_uuid"`
	Labels         map[string]string `json:"labels"`
	CreatedAt      time.Time         `json:"created_at"`
}

type runtimeState struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	PID    int64  `json:"pid"`
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	command.Stdin = nil
	if rootlessCommand(arguments) == "run" && containsArgument(arguments, "--detach") {
		null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return nil, err
		}
		defer null.Close()
		command.Stdout, command.Stderr = null, null
		if err := command.Run(); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(name), err)
		}
		return nil, nil
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func rootlessCommand(arguments []string) string {
	for _, argument := range arguments {
		switch argument {
		case "run", "state", "kill", "delete", "events":
			return argument
		}
	}
	return ""
}

func containsArgument(arguments []string, wanted string) bool {
	for _, argument := range arguments {
		if argument == wanted {
			return true
		}
	}
	return false
}

func New(config Config) (*Backend, error) {
	for name, value := range map[string]string{"runsc path": config.RunscPath, "rootfs": config.RootFS, "state root": config.StateRoot, "runtime root": config.RuntimeRoot, "log root": config.LogRoot} {
		if !filepath.IsAbs(value) {
			return nil, fmt.Errorf("absolute %s is required", name)
		}
	}
	if strings.TrimSpace(config.InstanceUUID) == "" {
		return nil, errors.New("instance UUID is required")
	}
	if info, err := os.Stat(config.RunscPath); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return nil, errors.New("executable node-local runsc is required")
	}
	if info, err := os.Stat(config.RootFS); err != nil || !info.IsDir() {
		return nil, errors.New("portable runtime rootfs is required")
	}
	if config.SupervisorHeartbeatInterval == 0 {
		config.SupervisorHeartbeatInterval = 5 * time.Second
	}
	if config.WorkerStopGrace == 0 {
		config.WorkerStopGrace = time.Second
	}
	if config.StartTimeout == 0 {
		config.StartTimeout = 15 * time.Second
	}
	if config.StartTimeout < time.Second || config.StartTimeout > 2*time.Minute {
		return nil, errors.New("rootless sandbox start timeout must be between 1 second and 2 minutes")
	}
	productionRunner := config.Runner == nil
	if productionRunner {
		config.Runner = execRunner{}
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
			return nil, fmt.Errorf("enable rootless sandbox child reaping: %w", err)
		}
	}
	for _, directory := range []string{config.StateRoot, config.RuntimeRoot, config.LogRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("initialize rootless runtime directory: %w", err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, fmt.Errorf("restrict rootless runtime directory: %w", err)
		}
	}
	return &Backend{
		runscPath: config.RunscPath, rootFS: config.RootFS, stateRoot: config.StateRoot, runtimeRoot: config.RuntimeRoot,
		logRoot: config.LogRoot, instanceUUID: config.InstanceUUID, callbackAddress: config.CallbackAddress,
		supervisorHeartbeatInterval: config.SupervisorHeartbeatInterval, workerStopGrace: config.WorkerStopGrace,
		startTimeout: config.StartTimeout, runner: config.Runner, logger: config.Logger,
		subreaper: productionRunner, procRoot: "/proc",
		consoleEnabled: productionRunner,
	}, nil
}

func (b *Backend) Close() error { return nil }

func (b *Backend) OpenConsole(ctx context.Context, sandboxID string, options backend.ConsoleOptions) (backend.Console, error) {
	if err := backend.ValidateConsoleOptions(options); err != nil {
		return nil, err
	}
	b.mu.Lock()
	if !b.consoleEnabled {
		b.mu.Unlock()
		return nil, errors.New("rootless console requires the production runsc executor")
	}
	meta, err := b.loadMetadata(sandboxID)
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	state, err := b.state(ctx, meta)
	if err != nil || state.Status != "running" {
		b.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("sandbox is not running")
	}
	arguments := []string{"--cwd=" + options.WorkingDir}
	for _, environment := range options.Environment {
		arguments = append(arguments, "--env="+environment)
	}
	arguments = append(arguments, sandboxID)
	arguments = append(arguments, options.Arguments...)
	arguments = b.runscArguments(meta, "exec", arguments...)
	b.mu.Unlock()
	if !options.Terminal {
		return runscconsole.OpenStream(ctx, b.runscPath, arguments)
	}
	return runscconsole.Open(ctx, b.runscPath, arguments, options.Size)
}

func (b *Backend) Create(ctx context.Context, sandbox model.SandboxSpec) (backend.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := sandbox.Validate(); err != nil {
		return backend.Observation{}, err
	}
	if sandbox.InternalToken == "" {
		return backend.Observation{}, errors.New("sandbox internal token is required")
	}
	if sandbox.Network.SandboxIP != "127.0.0.1" || sandbox.Network.SupervisorPort < 1 || sandbox.Network.InspectorPort < 1 {
		return backend.Observation{}, errors.New("rootless sandbox requires assigned loopback control endpoints")
	}
	if !safeID(sandbox.SandboxID) || !safeID(sandbox.RuntimeGroupID) {
		return backend.Observation{}, errors.New("safe sandbox and runtime-group IDs are required")
	}
	path := b.sandboxPath(sandbox.SandboxID)
	if _, err := os.Lstat(path); err == nil {
		return backend.Observation{}, fmt.Errorf("rootless sandbox %s already exists", sandbox.SandboxID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return backend.Observation{}, err
	}
	bundle := filepath.Join(path, "bundle")
	overlay := filepath.Join(path, "overlay")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		return backend.Observation{}, err
	}
	if err := os.Mkdir(overlay, 0o700); err != nil {
		_ = os.RemoveAll(path)
		return backend.Observation{}, err
	}
	if err := os.MkdirAll(b.sandboxLogRoot(sandbox.SandboxID), 0o700); err != nil {
		_ = os.RemoveAll(path)
		return backend.Observation{}, err
	}
	labels, err := b.labels(sandbox)
	if err != nil {
		_ = os.RemoveAll(path)
		return backend.Observation{}, err
	}
	meta := metadata{SandboxID: sandbox.SandboxID, RuntimeGroupID: sandbox.RuntimeGroupID, InstanceUUID: b.instanceUUID, Labels: labels, CreatedAt: time.Now().UTC()}
	if err := copyHostFile("/etc/resolv.conf", filepath.Join(bundle, "resolv.conf")); err != nil {
		_ = os.RemoveAll(path)
		return backend.Observation{}, err
	}
	if err := copyHostFile("/etc/hosts", filepath.Join(bundle, "hosts")); err != nil {
		_ = os.RemoveAll(path)
		return backend.Observation{}, err
	}
	configuration, err := b.ociSpec(sandbox, bundle)
	if err != nil {
		_ = os.RemoveAll(path)
		return backend.Observation{}, err
	}
	if err := writeJSON(filepath.Join(bundle, "config.json"), configuration); err != nil {
		_ = os.RemoveAll(path)
		return backend.Observation{}, err
	}
	if err := writeJSON(filepath.Join(path, "metadata.json"), meta); err != nil {
		_ = os.RemoveAll(path)
		return backend.Observation{}, err
	}
	arguments := b.runscArguments(meta, "run", "--detach", "--bundle="+bundle, "--user-log="+filepath.Join(b.sandboxLogRoot(sandbox.SandboxID), "user.log"), sandbox.SandboxID)
	if output, err := b.runner.Run(ctx, b.runscPath, arguments...); err != nil {
		_ = b.forceDelete(context.Background(), meta)
		_ = os.RemoveAll(path)
		return backend.Observation{}, fmt.Errorf("start rootless gVisor sandbox: %w: %s", err, strings.TrimSpace(string(output)))
	}
	state, err := b.waitForState(ctx, meta, map[string]bool{"running": true}, b.startTimeout)
	if err != nil {
		_ = b.forceDelete(context.Background(), meta)
		_ = os.RemoveAll(path)
		return backend.Observation{}, fmt.Errorf("wait for rootless gVisor sandbox: %w", err)
	}
	if b.logger != nil {
		b.logger.Info("rootless sandbox started", "sandbox_id", sandbox.SandboxID, "runtime_group_id", sandbox.RuntimeGroupID, "runtime", backend.RootlessRuntimeName, "pid", state.PID)
	}
	return observation(meta, state), nil
}

func (b *Backend) UpdateLabels(ctx context.Context, sandboxID string, updates map[string]string) error {
	_ = ctx
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := validateLabelUpdates(updates); err != nil {
		return err
	}
	meta, err := b.loadMetadata(sandboxID)
	if err != nil {
		return err
	}
	for key, value := range updates {
		meta.Labels[key] = value
	}
	return writeJSON(filepath.Join(b.sandboxPath(sandboxID), "metadata.json"), meta)
}

func (b *Backend) Observe(ctx context.Context, sandboxID string) (backend.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.observeLocked(ctx, sandboxID)
}

func (b *Backend) observeLocked(ctx context.Context, sandboxID string) (backend.Observation, error) {
	meta, err := b.loadMetadata(sandboxID)
	if err != nil {
		return backend.Observation{}, err
	}
	state, err := b.state(ctx, meta)
	if err != nil {
		if runtimeAbsent(err) {
			return observation(meta, runtimeState{ID: sandboxID, Status: "absent"}), nil
		}
		return backend.Observation{}, err
	}
	return observation(meta, state), nil
}

func (b *Backend) List(ctx context.Context) ([]backend.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	owned, err := b.listOwnedLocked()
	if err != nil {
		return nil, err
	}
	result := make([]backend.Observation, 0, len(owned))
	for _, ownedSandbox := range owned {
		item, observeErr := b.observeLocked(ctx, ownedSandbox.ContainerID)
		if errors.Is(observeErr, os.ErrNotExist) {
			continue
		}
		if observeErr != nil {
			return nil, observeErr
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ContainerID < result[j].ContainerID })
	return result, nil
}

// ListOwned returns instance-owned metadata without querying each runsc
// sandbox. Startup destruction uses this path so stale supervisors cannot delay
// control-plane readiness or cleanup.
func (b *Backend) ListOwned(_ context.Context) ([]backend.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.listOwnedLocked()
}

func (b *Backend) listOwnedLocked() ([]backend.Observation, error) {
	entries, err := os.ReadDir(b.stateRoot)
	if err != nil {
		return nil, err
	}
	result := make([]backend.Observation, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !safeID(entry.Name()) {
			continue
		}
		meta, loadErr := b.loadMetadata(entry.Name())
		if errors.Is(loadErr, os.ErrNotExist) {
			continue
		}
		if loadErr != nil {
			return nil, loadErr
		}
		result = append(result, observation(meta, runtimeState{}))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ContainerID < result[j].ContainerID })
	return result, nil
}

func (b *Backend) Stop(ctx context.Context, sandboxID string, grace time.Duration) error {
	if grace <= 0 {
		grace = 10 * time.Second
	}
	return b.stop(ctx, sandboxID, "TERM", grace)
}

func (b *Backend) Kill(ctx context.Context, sandboxID string) error {
	return b.stop(ctx, sandboxID, "KILL", 2*time.Second)
}

func (b *Backend) stop(ctx context.Context, sandboxID, signal string, grace time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	pids := b.sandboxPIDs(sandboxID)
	defer b.reapPIDs(pids)
	meta, err := b.loadMetadata(sandboxID)
	if err != nil {
		return err
	}
	state, stateErr := b.state(ctx, meta)
	if runtimeAbsent(stateErr) || state.Status == "stopped" {
		return nil
	}
	if stateErr != nil {
		return stateErr
	}
	if output, err := b.runner.Run(ctx, b.runscPath, b.runscArguments(meta, "kill", "--all", sandboxID, signal)...); err != nil && !runtimeAbsent(err) {
		if staleControl(err) {
			return b.forceDelete(ctx, meta)
		}
		return fmt.Errorf("signal rootless sandbox: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if _, err := b.waitForState(ctx, meta, map[string]bool{"stopped": true}, grace); err == nil {
		return nil
	} else if staleControl(err) {
		return b.forceDelete(ctx, meta)
	}
	if signal != "KILL" {
		if output, err := b.runner.Run(ctx, b.runscPath, b.runscArguments(meta, "kill", "--all", sandboxID, "KILL")...); err != nil && !runtimeAbsent(err) {
			if staleControl(err) {
				return b.forceDelete(ctx, meta)
			}
			return fmt.Errorf("force rootless sandbox stop: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	_, err = b.waitForState(ctx, meta, map[string]bool{"stopped": true}, 2*time.Second)
	return err
}

func (b *Backend) Delete(ctx context.Context, sandboxID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	pids := b.sandboxPIDs(sandboxID)
	defer b.reapPIDs(pids)
	meta, err := b.loadMetadata(sandboxID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := b.forceDelete(ctx, meta); err != nil {
		return err
	}
	if err := os.RemoveAll(b.sandboxPath(sandboxID)); err != nil {
		return fmt.Errorf("remove rootless sandbox state: %w", err)
	}
	return nil
}

func (b *Backend) Metrics(ctx context.Context, sandboxID string) (model.ResourceMetrics, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.loadMetadata(sandboxID); err != nil {
		return model.ResourceMetrics{}, err
	}
	output, err := b.runner.Run(ctx, b.runscPath, "--root="+b.runtimeRoot, "events", "--stats", sandboxID)
	if err != nil {
		return model.ResourceMetrics{}, fmt.Errorf("query rootless sandbox metrics: %w", err)
	}
	var event struct {
		Type string `json:"type"`
		Data struct {
			CPU struct {
				Usage struct {
					Total uint64 `json:"total"`
				} `json:"usage"`
			} `json:"cpu"`
			Memory struct {
				Usage struct {
					Usage uint64 `json:"usage"`
					Max   uint64 `json:"max"`
				} `json:"usage"`
			} `json:"memory"`
			PIDs struct {
				Current uint64 `json:"current"`
			} `json:"pids"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	if err := decoder.Decode(&event); err != nil || event.Type != "stats" {
		return model.ResourceMetrics{}, errors.New("runsc returned an invalid stats event")
	}
	return model.ResourceMetrics{
		CPUUsageMicros: int64(event.Data.CPU.Usage.Total / 1000), MemoryCurrent: int64(event.Data.Memory.Usage.Usage),
		MemoryPeak: int64(event.Data.Memory.Usage.Max), PIDCurrent: int64(event.Data.PIDs.Current),
	}, nil
}

func (b *Backend) ociSpec(sandbox model.SandboxSpec, bundle string) (specs.Spec, error) {
	processConfig := backend.ProcessConfig{
		NodeID:          b.instanceUUID,
		CallbackAddress: b.callbackAddress, SupervisorHost: "127.0.0.1", SupervisorPort: sandbox.Network.SupervisorEndpointPort(),
		InspectorHost: "127.0.0.1", InspectorPort: sandbox.Network.InspectorEndpointPort(),
		SupervisorHeartbeatInterval: b.supervisorHeartbeatInterval, WorkerStopGrace: b.workerStopGrace,
	}
	if err := backend.ValidateProcessConfig(processConfig); err != nil {
		return specs.Spec{}, err
	}
	baseArguments := []string{
		"/usr/bin/deno", "run", "--unstable-worker-options", "--config=/opt/runtime/deno.json", "--cached-only", "--no-prompt",
		"/opt/runtime/supervisor/main.ts",
	}
	mounts := []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/run", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=755"}},
		{Destination: "/etc/resolv.conf", Type: "bind", Source: filepath.Join(bundle, "resolv.conf"), Options: []string{"bind", "ro", "nosuid", "nodev", "noexec"}},
		{Destination: "/etc/hosts", Type: "bind", Source: filepath.Join(bundle, "hosts"), Options: []string{"bind", "ro", "nosuid", "nodev", "noexec"}},
	}
	for _, mount := range sandbox.Mounts {
		mounts = append(mounts, backend.OCIMount(mount))
	}
	hostname := sandbox.SandboxID
	if len(hostname) > 63 {
		hostname = hostname[:63]
	}
	return specs.Spec{
		Version: specs.Version,
		Process: &specs.Process{
			Terminal: false, User: specs.User{UID: 0, GID: 0}, Args: backend.DenoProcessArguments(baseArguments, sandbox, processConfig),
			Env: backend.RuntimeEnvironment([]string{"PATH=/usr/bin", "HOME=/tmp", "DENO_DIR=/runtime-cache", "DENO_NO_UPDATE_CHECK=1", "DENO_NO_PROMPT=1"}, sandbox, processConfig),
			Cwd: "/opt/runtime", Capabilities: &specs.LinuxCapabilities{}, NoNewPrivileges: true,
			Rlimits: []specs.POSIXRlimit{{Type: "RLIMIT_NOFILE", Hard: 1048576, Soft: 1048576}},
		},
		Root: &specs.Root{Path: b.rootFS, Readonly: false}, Hostname: hostname, Mounts: mounts,
		Linux: &specs.Linux{
			Namespaces:    []specs.LinuxNamespace{{Type: specs.PIDNamespace}, {Type: specs.IPCNamespace}, {Type: specs.UTSNamespace}, {Type: specs.MountNamespace}},
			MaskedPaths:   []string{"/proc/acpi", "/proc/kcore", "/proc/keys", "/proc/latency_stats", "/proc/timer_list", "/proc/timer_stats", "/proc/sched_debug", "/sys/firmware"},
			ReadonlyPaths: []string{"/proc/asound", "/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"},
		},
	}, nil
}

func (b *Backend) state(ctx context.Context, meta metadata) (runtimeState, error) {
	output, err := b.runner.Run(ctx, b.runscPath, b.runscArguments(meta, "state", meta.SandboxID)...)
	if err != nil {
		return runtimeState{}, fmt.Errorf("query rootless sandbox state: %w", err)
	}
	var state runtimeState
	decoder := json.NewDecoder(bytes.NewReader(output))
	if err := decoder.Decode(&state); err != nil || state.Status == "" {
		return runtimeState{}, errors.New("runsc returned an invalid state object")
	}
	return state, nil
}

func (b *Backend) waitForState(ctx context.Context, meta metadata, wanted map[string]bool, timeout time.Duration) (runtimeState, error) {
	waitContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		state, err := b.state(waitContext, meta)
		if err == nil && wanted[state.Status] {
			return state, nil
		}
		if err == nil && wanted["stopped"] && state.Status == "running" && hostProcessExited(state.PID) {
			state.Status = "stopped"
			return state, nil
		}
		if runtimeAbsent(err) && wanted["stopped"] {
			return runtimeState{ID: meta.SandboxID, Status: "stopped"}, nil
		}
		if err != nil {
			last = err
		}
		select {
		case <-waitContext.Done():
			if last != nil {
				return runtimeState{}, last
			}
			return runtimeState{}, waitContext.Err()
		case <-ticker.C:
		}
	}
}

func (b *Backend) forceDelete(ctx context.Context, meta metadata) error {
	output, err := b.runner.Run(ctx, b.runscPath, b.runscArguments(meta, "delete", "--force", meta.SandboxID)...)
	if err != nil && !runtimeAbsent(err) {
		return fmt.Errorf("delete rootless sandbox: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (b *Backend) runscArguments(meta metadata, command string, arguments ...string) []string {
	return append([]string{
		"--allow-rootfs-tar-annotation", "--root=" + b.runtimeRoot, "--rootless=true", "--platform=systrap", "--directfs=false",
		"--file-access=exclusive", "--file-access-mounts=shared", "--network=host",
		"--overlay2=root:dir=" + filepath.Join(b.sandboxPath(meta.SandboxID), "overlay"),
		"--log=" + filepath.Join(b.sandboxLogRoot(meta.SandboxID), "runsc-"+command+".log"), command,
	}, arguments...)
}

func (b *Backend) loadMetadata(sandboxID string) (metadata, error) {
	if !safeID(sandboxID) {
		return metadata{}, errors.New("safe sandbox ID is required")
	}
	file, err := os.Open(filepath.Join(b.sandboxPath(sandboxID), "metadata.json"))
	if err != nil {
		return metadata{}, err
	}
	defer file.Close()
	var value metadata
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return metadata{}, fmt.Errorf("decode rootless sandbox metadata: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return metadata{}, errors.New("decode rootless sandbox metadata: trailing data")
	}
	if value.SandboxID != sandboxID || value.InstanceUUID != b.instanceUUID || value.RuntimeGroupID == "" || value.Labels[labelManaged] != "true" || value.Labels[labelInstance] != b.instanceUUID {
		return metadata{}, errors.New("rootless sandbox metadata is not owned by this kernel instance")
	}
	return value, nil
}

func (b *Backend) labels(sandbox model.SandboxSpec) (map[string]string, error) {
	labels := map[string]string{
		labelManaged: "true", labelInstance: b.instanceUUID, labelRuntimeGroup: sandbox.RuntimeGroupID,
		labelWorkloadType: string(sandbox.WorkloadType), labelProfileHash: sandbox.ProfileHash, labelImageDigest: sandbox.ImageDigest,
	}
	for key, value := range sandbox.Labels {
		if strings.HasPrefix(key, "the8020.runtime.") {
			return nil, fmt.Errorf("sandbox label %q is reserved", key)
		}
		labels[key] = value
	}
	return labels, nil
}

func validateLabelUpdates(labels map[string]string) error {
	for key, value := range labels {
		if key != labelOwner && key != labelOwners && key != labelServices && key != labelGroupKey && key != labelAssignedAt {
			return fmt.Errorf("sandbox label %q cannot be updated", key)
		}
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("sandbox label %q cannot be empty", key)
		}
	}
	return nil
}

func observation(meta metadata, state runtimeState) backend.Observation {
	pid := uint32(0)
	if state.PID > 0 && state.PID <= int64(^uint32(0)) {
		pid = uint32(state.PID)
	}
	return backend.Observation{
		ContainerID: meta.SandboxID, Runtime: backend.RootlessRuntimeName, RuntimeGroupID: meta.RuntimeGroupID,
		TaskStatus: state.Status, TaskPID: pid, Labels: cloneMap(meta.Labels),
	}
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".rootless-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(append(data, '\n'))
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func copyHostFile(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read host %s: %w", source, err)
	}
	if err := os.WriteFile(destination, data, 0o644); err != nil {
		return fmt.Errorf("write sandbox %s: %w", filepath.Base(destination), err)
	}
	return nil
}

func runtimeAbsent(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "does not exist") || strings.Contains(message, "no such file") || strings.Contains(message, "not found")
}

func staleControl(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "connection refused") || strings.Contains(message, "control server") && strings.Contains(message, "unavailable")
}

func hostProcessExited(pid int64) bool {
	if pid <= 0 {
		return true
	}
	data, err := os.ReadFile(filepath.Join("/proc", fmt.Sprintf("%d", pid), "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	closing := bytes.LastIndexByte(data, ')')
	return closing >= 0 && len(data) > closing+2 && data[closing+2] == 'Z'
}

func (b *Backend) sandboxPIDs(sandboxID string) []int {
	if !b.subreaper || !safeID(sandboxID) {
		return nil
	}
	taskRoot := filepath.Join(b.procRoot, strconv.Itoa(os.Getpid()), "task")
	tasks, err := os.ReadDir(taskRoot)
	if err != nil {
		return nil
	}
	children := map[int]bool{}
	for _, task := range tasks {
		if !task.IsDir() {
			continue
		}
		if _, parseErr := strconv.Atoi(task.Name()); parseErr != nil {
			continue
		}
		data, readErr := readLimitedFile(filepath.Join(taskRoot, task.Name(), "children"), 1<<20)
		if readErr != nil {
			continue
		}
		for _, field := range strings.Fields(string(data)) {
			pid, parseErr := strconv.Atoi(field)
			if parseErr == nil && pid > 1 {
				children[pid] = true
			}
		}
	}
	result := make([]int, 0, len(children))
	for pid := range children {
		commandLine, readErr := readLimitedFile(filepath.Join(b.procRoot, strconv.Itoa(pid), "cmdline"), 1<<20)
		if readErr != nil {
			continue
		}
		command := strings.ReplaceAll(string(commandLine), "\x00", " ")
		if strings.Contains(command, sandboxID) && (strings.Contains(command, "runsc-sandbox") || strings.Contains(command, "runsc-gofer")) {
			result = append(result, pid)
		}
	}
	sort.Ints(result)
	return result
}

func readLimitedFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maximum)
	}
	return data, nil
}

func (b *Backend) reapPIDs(pids []int) {
	if !b.subreaper || len(pids) == 0 {
		return
	}
	deadline := time.Now().Add(2 * time.Second)
	pending := append([]int(nil), pids...)
	for len(pending) > 0 && time.Now().Before(deadline) {
		next := pending[:0]
		for _, pid := range pending {
			var status unix.WaitStatus
			waited, err := unix.Wait4(pid, &status, unix.WNOHANG, nil)
			if err == nil && waited == 0 {
				next = append(next, pid)
			}
		}
		pending = next
		if len(pending) > 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func safeID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return value != "." && value != ".."
}

func (b *Backend) sandboxPath(sandboxID string) string {
	return filepath.Join(b.stateRoot, sandboxID)
}

func (b *Backend) sandboxLogRoot(sandboxID string) string {
	return filepath.Join(b.logRoot, sandboxID)
}

func cloneMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
