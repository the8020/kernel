// Package backend defines the container-runtime boundary used by sandbox lifecycle management.
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"the8020/kernel/sandbox/model"
)

const RootlessRuntimeName = "runsc-rootless-systrap"

type Observation struct {
	ContainerID    string            `json:"container_id"`
	Runtime        string            `json:"runtime"`
	RuntimeGroupID string            `json:"runtime_group_id"`
	TaskStatus     string            `json:"task_status"`
	TaskPID        uint32            `json:"task_pid"`
	Labels         map[string]string `json:"labels"`
}

type Backend interface {
	Create(context.Context, model.SandboxSpec) (Observation, error)
	UpdateLabels(context.Context, string, map[string]string) error
	Observe(context.Context, string) (Observation, error)
	ListOwned(context.Context) ([]Observation, error)
	List(context.Context) ([]Observation, error)
	Stop(context.Context, string, time.Duration) error
	Kill(context.Context, string) error
	Delete(context.Context, string) error
}

type MetricsProvider interface {
	Metrics(context.Context, string) (model.ResourceMetrics, error)
}

// ConsoleSize is the host-authoritative pseudoterminal geometry.
type ConsoleSize struct {
	Columns int `json:"columns"`
	Rows    int `json:"rows"`
}

// ConsoleOptions describes one new interactive process in a running sandbox.
// Arguments are passed directly to the OCI runtime and never through a shell.
type ConsoleOptions struct {
	Arguments   []string
	Environment []string
	WorkingDir  string
	Size        ConsoleSize
	Terminal    bool
}

// Console is one bidirectional PTY process. Done closes when the process exits.
type Console interface {
	io.ReadWriteCloser
	CloseWrite() error
	Stderr() io.Reader
	Resize(context.Context, ConsoleSize) error
	Done() <-chan struct{}
}

// ConsoleExitStatus is implemented by attached process streams that can report
// the real process result after output reaches EOF.
type ConsoleExitStatus interface {
	ExitStatus() uint32
}

// ConsoleBackend is optional so lifecycle-only backend fakes remain small.
// Production full and rootless backends implement it.
type ConsoleBackend interface {
	OpenConsole(context.Context, string, ConsoleOptions) (Console, error)
}

const maximumConsoleArgumentBytes = 16 << 10

func ValidateConsoleOptions(options ConsoleOptions) error {
	if len(options.Arguments) == 0 || len(options.Arguments) > 32 {
		return errors.New("console requires between 1 and 32 arguments")
	}
	for _, argument := range options.Arguments {
		if argument == "" || len(argument) > maximumConsoleArgumentBytes || strings.ContainsRune(argument, '\x00') {
			return errors.New("console arguments must be non-empty, bounded, and contain no null bytes")
		}
	}
	if options.WorkingDir == "" || !strings.HasPrefix(options.WorkingDir, "/") || strings.ContainsRune(options.WorkingDir, '\x00') || len(options.WorkingDir) > 4096 {
		return errors.New("console working directory must be an absolute bounded path")
	}
	if len(options.Environment) > 64 {
		return errors.New("console environment is too large")
	}
	for _, entry := range options.Environment {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || len(entry) > 8192 || strings.ContainsRune(entry, '\x00') {
			return errors.New("console environment entries must be bounded KEY=VALUE strings")
		}
	}
	if options.Size.Columns < 2 || options.Size.Columns > 500 || options.Size.Rows < 1 || options.Size.Rows > 200 {
		return errors.New("console size is outside the supported range")
	}
	return nil
}

type ProcessConfig struct {
	NodeID                      string
	CallbackAddress             string
	SupervisorHost              string
	SupervisorPort              int
	InspectorHost               string
	InspectorPort               int
	SupervisorHeartbeatInterval time.Duration
	WorkerStopGrace             time.Duration
}

func RuntimeEnvironment(existing []string, sandbox model.SandboxSpec, config ProcessConfig) []string {
	values := map[string]string{}
	for _, entry := range existing {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	additions := map[string]string{
		"NODE_ID":    config.NodeID,
		"SANDBOX_ID": sandbox.SandboxID, "RUNTIME_GROUP_ID": sandbox.RuntimeGroupID,
		"WORKLOAD_TYPE": string(sandbox.WorkloadType), "IMAGE_DIGEST": sandbox.ImageDigest,
		"DEPENDENCY_MODE":    string(sandbox.DependencyMode),
		"INTERNAL_API_TOKEN": sandbox.InternalToken, "SUPERVISOR_HOST": config.SupervisorHost,
		"SUPERVISOR_PORT": strconv.Itoa(config.SupervisorPort), "INSPECTOR_PORT": strconv.Itoa(config.InspectorPort),
		"RUNTIME_PROFILE_HASH": sandbox.ProfileHash, "KERNEL_CALLBACK_ADDRESS": config.CallbackAddress,
		"HEARTBEAT_INTERVAL_MS": strconv.FormatInt(config.SupervisorHeartbeatInterval.Milliseconds(), 10),
		"WORKER_STOP_GRACE_MS":  strconv.FormatInt(config.WorkerStopGrace.Milliseconds(), 10),
	}
	for key, value := range additions {
		if value != "" || key != "KERNEL_CALLBACK_ADDRESS" {
			values[key] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func DenoProcessArguments(args []string, sandbox model.SandboxSpec, config ProcessConfig) []string {
	result := make([]string, 0, len(args)+6)
	for _, argument := range args {
		if sandbox.DependencyMode == model.DependencyOnline && argument == "--cached-only" {
			continue
		}
		if argument == "--inspect" || strings.HasPrefix(argument, "--inspect=") || argument == "--inspect-brk" || strings.HasPrefix(argument, "--inspect-brk=") {
			continue
		}
		result = append(result, argument)
	}
	readPaths := append([]string{"/opt/runtime", "/artifacts"}, sandbox.Permissions.ReadPaths...)
	writePaths := append([]string{"/tmp", "/runtime-cache"}, sandbox.Permissions.WritePaths...)
	networkHosts := append([]string{config.SupervisorHost + ":" + strconv.Itoa(config.SupervisorPort)}, sandbox.Permissions.NetworkHosts...)
	if callback, err := url.Parse(config.CallbackAddress); err == nil && callback.Host != "" {
		networkHosts = append(networkHosts, callback.Host)
	}
	result = replaceArgument(result, "--allow-read=", readPaths)
	result = replaceArgument(result, "--allow-write=", writePaths)
	result = replaceArgument(result, "--allow-net=", networkHosts)
	result = replaceArgument(result, "--allow-import=", sandbox.Permissions.ImportHosts)
	result = replaceArgument(result, "--allow-env=", append([]string{
		"NODE_ID", "SANDBOX_ID", "RUNTIME_GROUP_ID", "WORKLOAD_TYPE", "IMAGE_DIGEST", "DEPENDENCY_MODE", "INTERNAL_API_TOKEN", "KERNEL_CALLBACK_ADDRESS",
		"SUPERVISOR_HOST", "SUPERVISOR_PORT", "INSPECTOR_PORT", "RUNTIME_PROFILE_HASH", "HEARTBEAT_INTERVAL_MS", "WORKER_STOP_GRACE_MS",
	}, sandbox.Permissions.Environment...))
	if sandbox.Permissions.SystemInfo {
		result = insertBeforeEntrypoint(result, "--allow-sys=hostname,osRelease")
	}
	if sandbox.WorkloadType == model.WorkloadService {
		// The infrastructure supervisor uses the pinned Deno binary only for
		// service type checking. Program Workers never receive run permission.
		result = insertBeforeEntrypoint(result, "--allow-run=/usr/bin/deno")
	}
	result = insertBeforeEntrypoint(result, "--inspect="+config.InspectorHost+":"+strconv.Itoa(config.InspectorPort))
	for _, flag := range sandbox.RuntimeProfile.DenoStartupFlags {
		result = insertBeforeEntrypoint(result, flag)
	}
	return result
}

func OCIMount(mount model.Mount) specs.Mount {
	if mount.Purpose == "temporary" {
		return specs.Mount{Destination: mount.Target, Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "noexec", "mode=1777", "size=" + strconv.FormatInt(mount.MaximumSize, 10)}}
	}
	mode := "rw"
	if mount.ReadOnly {
		mode = "ro"
	}
	return specs.Mount{Destination: mount.Target, Type: "bind", Source: mount.Source, Options: []string{"rbind", mode, "nosuid", "nodev", "noexec", "rprivate"}}
}

func replaceArgument(args []string, prefix string, additions []string) []string {
	values := map[string]bool{}
	result := make([]string, 0, len(args)+1)
	for _, argument := range args {
		if strings.HasPrefix(argument, prefix) {
			for _, value := range strings.Split(strings.TrimPrefix(argument, prefix), ",") {
				if value != "" {
					values[value] = true
				}
			}
			continue
		}
		result = append(result, argument)
	}
	for _, value := range additions {
		if value != "" {
			values[value] = true
		}
	}
	sorted := make([]string, 0, len(values))
	for value := range values {
		sorted = append(sorted, value)
	}
	sort.Strings(sorted)
	if len(sorted) == 0 {
		return result
	}
	return insertBeforeEntrypoint(result, prefix+strings.Join(sorted, ","))
}

func insertBeforeEntrypoint(args []string, value string) []string {
	if value == "" {
		return args
	}
	index := len(args)
	for candidate := len(args) - 1; candidate >= 0; candidate-- {
		if strings.HasPrefix(args[candidate], "file:") || strings.HasSuffix(args[candidate], ".ts") || strings.HasSuffix(args[candidate], ".js") {
			index = candidate
			break
		}
	}
	args = append(args, "")
	copy(args[index+1:], args[index:])
	args[index] = value
	return args
}

func ValidateProcessConfig(config ProcessConfig) error {
	if config.SupervisorHost == "" || config.InspectorHost == "" || config.SupervisorPort < 1 || config.SupervisorPort > 65535 || config.InspectorPort < 1 || config.InspectorPort > 65535 || config.SupervisorPort == config.InspectorPort {
		return fmt.Errorf("valid distinct supervisor and inspector endpoints are required")
	}
	if config.SupervisorHeartbeatInterval < 100*time.Millisecond || config.SupervisorHeartbeatInterval > time.Minute {
		return errors.New("supervisor heartbeat interval must be between 100 milliseconds and 1 minute")
	}
	if config.WorkerStopGrace < 10*time.Millisecond || config.WorkerStopGrace > time.Minute {
		return errors.New("Worker stop grace must be between 10 milliseconds and 1 minute")
	}
	return nil
}
