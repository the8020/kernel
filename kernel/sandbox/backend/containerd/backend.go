// Package containerd implements gVisor sandbox lifecycle with containerd's official Go client.
package containerd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"the8020/kernel/sandbox/backend"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/sandbox/resources"
)

const RuntimeName = "io.containerd.runsc.v1"

const (
	labelManaged          = "the8020.runtime.managed"
	labelInstance         = "the8020.runtime.instance_uuid"
	labelRuntimeGroup     = "the8020.runtime.group_id"
	labelWorkloadType     = "the8020.runtime.workload_type"
	labelProfileHash      = "the8020.runtime.profile_hash"
	labelImageDigest      = "the8020.runtime.image_digest"
	labelOwner            = "the8020.owner"
	labelOwners           = "the8020.owners"
	labelServices         = "the8020.services"
	labelGroupKey         = "the8020.group_key"
	labelAssignedAt       = "the8020.assigned_at"
	defaultSnapshotter    = "overlayfs"
	defaultSupervisorPort = model.DefaultSupervisorPort
)

type Config struct {
	Socket                      string
	InstanceUUID                string
	Snapshotter                 string
	LogRoot                     string
	CallbackAddress             string
	SupervisorPort              int
	SupervisorHeartbeatInterval time.Duration
	WorkerStopGrace             time.Duration
	Logger                      *slog.Logger
}

type Backend struct {
	client                      *containerdclient.Client
	namespace                   string
	instanceUUID                string
	snapshotter                 string
	logRoot                     string
	callbackAddress             string
	supervisorPort              int
	supervisorHeartbeatInterval time.Duration
	workerStopGrace             time.Duration
	logger                      *slog.Logger
}

type Observation = backend.Observation

func Connect(ctx context.Context, config Config) (*Backend, error) {
	if config.Socket == "" {
		config.Socket = "/run/containerd/containerd.sock"
	}
	if !filepath.IsAbs(config.Socket) || strings.TrimSpace(config.InstanceUUID) == "" {
		return nil, errors.New("absolute containerd socket and instance UUID are required")
	}
	if config.Snapshotter == "" {
		config.Snapshotter = defaultSnapshotter
	}
	if config.SupervisorPort == 0 {
		config.SupervisorPort = defaultSupervisorPort
	}
	if config.SupervisorPort < 1 || config.SupervisorPort > 65535 {
		return nil, errors.New("supervisor port must be between 1 and 65535")
	}
	if config.SupervisorHeartbeatInterval == 0 {
		config.SupervisorHeartbeatInterval = 5 * time.Second
	}
	if config.SupervisorHeartbeatInterval < 100*time.Millisecond || config.SupervisorHeartbeatInterval > time.Minute {
		return nil, errors.New("supervisor heartbeat interval must be between 100 milliseconds and 1 minute")
	}
	if config.WorkerStopGrace == 0 {
		config.WorkerStopGrace = time.Second
	}
	if config.WorkerStopGrace < 10*time.Millisecond || config.WorkerStopGrace > time.Minute {
		return nil, errors.New("Worker stop grace must be between 10 milliseconds and 1 minute")
	}
	client, err := containerdclient.New(config.Socket, containerdclient.WithDefaultNamespace(NamespaceForInstance(config.InstanceUUID)))
	if err != nil {
		return nil, fmt.Errorf("connect containerd: %w", err)
	}
	backend := &Backend{client: client, namespace: NamespaceForInstance(config.InstanceUUID), instanceUUID: config.InstanceUUID, snapshotter: config.Snapshotter, logRoot: config.LogRoot, callbackAddress: config.CallbackAddress, supervisorPort: config.SupervisorPort, supervisorHeartbeatInterval: config.SupervisorHeartbeatInterval, workerStopGrace: config.WorkerStopGrace, logger: config.Logger}
	probeContext, cancel := context.WithTimeout(backend.withNamespace(ctx), 5*time.Second)
	defer cancel()
	if _, err := client.Version(probeContext); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("query containerd version at %s: %w", config.Socket, err)
	}
	return backend, nil
}

func NamespaceForInstance(instanceUUID string) string {
	var builder strings.Builder
	builder.WriteString("the8020-")
	for _, character := range strings.ToLower(instanceUUID) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('-')
		}
	}
	return strings.TrimRight(builder.String(), "-")
}

func (b *Backend) Close() error {
	if b == nil || b.client == nil {
		return nil
	}
	return b.client.Close()
}

type containerConsole struct {
	backend   *Backend
	process   containerdclient.Process
	input     *io.PipeWriter
	output    *io.PipeReader
	stderr    *io.PipeReader
	done      chan struct{}
	closeOnce sync.Once
}

func (b *Backend) OpenConsole(ctx context.Context, sandboxID string, options backend.ConsoleOptions) (backend.Console, error) {
	if err := backend.ValidateConsoleOptions(options); err != nil {
		return nil, err
	}
	ctx = b.withNamespace(ctx)
	container, err := b.ownedContainer(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("load sandbox task for console: %w", err)
	}
	status, err := task.Status(ctx)
	if err != nil {
		return nil, err
	}
	if status.Status != containerdclient.Running {
		return nil, errors.New("sandbox is not running")
	}
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	processID, err := consoleProcessID()
	if err != nil {
		_ = inputReader.Close()
		_ = inputWriter.Close()
		_ = outputReader.Close()
		_ = outputWriter.Close()
		_ = stderrReader.Close()
		_ = stderrWriter.Close()
		_ = stderrWriter.Close()
		return nil, err
	}
	process, err := task.Exec(ctx, processID, &specs.Process{
		Terminal:        options.Terminal,
		User:            specs.User{UID: 0, GID: 0},
		Args:            append([]string(nil), options.Arguments...),
		Env:             append([]string(nil), options.Environment...),
		Cwd:             options.WorkingDir,
		Capabilities:    &specs.LinuxCapabilities{},
		NoNewPrivileges: true,
	}, func() cio.Creator {
		if options.Terminal {
			return cio.NewCreator(cio.WithStreams(inputReader, outputWriter, outputWriter), cio.WithTerminal)
		}
		return cio.NewCreator(cio.WithStreams(inputReader, outputWriter, stderrWriter))
	}())
	if err != nil {
		_ = inputReader.Close()
		_ = inputWriter.Close()
		_ = outputReader.Close()
		_ = outputWriter.Close()
		_ = stderrReader.Close()
		_ = stderrWriter.Close()
		return nil, fmt.Errorf("create sandbox console process: %w", err)
	}
	processContext := b.withNamespace(context.Background())
	wait, err := process.Wait(processContext)
	if err != nil {
		_, _ = process.Delete(processContext, containerdclient.WithProcessKill)
		_ = inputReader.Close()
		_ = inputWriter.Close()
		_ = outputReader.Close()
		_ = outputWriter.Close()
		return nil, fmt.Errorf("wait for sandbox console process: %w", err)
	}
	if err := process.Start(ctx); err != nil {
		_, _ = process.Delete(processContext, containerdclient.WithProcessKill)
		_ = inputReader.Close()
		_ = inputWriter.Close()
		_ = outputReader.Close()
		_ = outputWriter.Close()
		_ = stderrReader.Close()
		_ = stderrWriter.Close()
		return nil, fmt.Errorf("start sandbox console process: %w", err)
	}
	var processStderr *io.PipeReader
	if options.Terminal {
		_ = stderrReader.Close()
		_ = stderrWriter.Close()
	} else {
		processStderr = stderrReader
	}
	value := &containerConsole{
		backend: b, process: process, input: inputWriter, output: outputReader, stderr: processStderr,
		done: make(chan struct{}),
	}
	if options.Terminal {
		if err := value.Resize(ctx, options.Size); err != nil {
			_ = value.Close()
			return nil, fmt.Errorf("size sandbox console: %w", err)
		}
	}
	go func() {
		<-wait
		_ = inputWriter.Close()
		process.IO().Wait()
		_ = outputWriter.Close()
		_ = stderrWriter.Close()
		_ = inputReader.Close()
		_ = process.IO().Close()
		_, _ = process.Delete(processContext)
		close(value.done)
	}()
	return value, nil
}

func consoleProcessID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return "console-" + hex.EncodeToString(data), nil
}

func (c *containerConsole) Read(data []byte) (int, error) { return c.output.Read(data) }

func (c *containerConsole) Write(data []byte) (int, error) { return c.input.Write(data) }
func (c *containerConsole) Stderr() io.Reader {
	if c.stderr == nil {
		return nil
	}
	return c.stderr
}

func (c *containerConsole) CloseWrite() error { return c.input.Close() }

func (c *containerConsole) Resize(ctx context.Context, size backend.ConsoleSize) error {
	return c.process.Resize(c.backend.withNamespace(ctx), uint32(size.Columns), uint32(size.Rows))
}

func (c *containerConsole) Done() <-chan struct{} { return c.done }

func (c *containerConsole) Close() error {
	var result error
	c.closeOnce.Do(func() {
		result = c.input.Close()
		ctx, cancel := context.WithTimeout(c.backend.withNamespace(context.Background()), 2*time.Second)
		defer cancel()
		if err := c.process.Kill(ctx, syscall.SIGKILL); err != nil && !errdefs.IsNotFound(err) {
			result = errors.Join(result, err)
		}
	})
	return result
}

func (b *Backend) Version(ctx context.Context) (string, error) {
	version, err := b.client.Version(b.withNamespace(ctx))
	if err != nil {
		return "", err
	}
	return version.Version, nil
}

func (b *Backend) ImagePresent(ctx context.Context, digest string) (bool, string, error) {
	image, err := b.imageByDigest(b.withNamespace(ctx), digest)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, "", nil
		}
		return false, "", err
	}
	return true, image.Name(), nil
}

func (b *Backend) Create(ctx context.Context, sandbox model.SandboxSpec) (observation Observation, returnError error) {
	if err := sandbox.Validate(); err != nil {
		return observation, err
	}
	if sandbox.InternalToken == "" {
		return observation, errors.New("sandbox internal token is required")
	}
	if sandbox.Network.NamespacePath == "" {
		return observation, errors.New("sandbox network namespace path is required")
	}
	labels, err := b.labels(sandbox)
	if err != nil {
		return observation, err
	}
	ctx = b.withNamespace(ctx)
	image, err := b.imageByDigest(ctx, sandbox.ImageDigest)
	if err != nil {
		return observation, fmt.Errorf("find immutable runtime image: %w", err)
	}
	unpacked, err := image.IsUnpacked(ctx, b.snapshotter)
	if err != nil {
		return observation, fmt.Errorf("inspect runtime image snapshot: %w", err)
	}
	if !unpacked {
		if err := image.Unpack(ctx, b.snapshotter); err != nil {
			return observation, fmt.Errorf("unpack runtime image: %w", err)
		}
	}
	container, err := b.client.NewContainer(ctx, sandbox.SandboxID,
		containerdclient.WithImage(image),
		containerdclient.WithNewSnapshotView(sandbox.SandboxID+"-rootfs", image),
		containerdclient.WithNewSpec(oci.WithImageConfig(image), sandboxSpecOption(sandbox, b.instanceUUID, b.callbackAddress, b.supervisorPort, b.supervisorHeartbeatInterval, b.workerStopGrace)),
		containerdclient.WithContainerLabels(labels),
		containerdclient.WithRuntime(RuntimeName, nil),
	)
	if err != nil {
		return observation, fmt.Errorf("create gVisor container: %w", err)
	}
	defer func() {
		if returnError != nil {
			_ = container.Delete(b.withNamespace(context.Background()), containerdclient.WithSnapshotCleanup)
		}
	}()
	creator := cio.NullIO
	if b.logRoot != "" {
		if err := os.MkdirAll(b.logRoot, 0o700); err != nil {
			return observation, fmt.Errorf("create sandbox log directory: %w", err)
		}
		creator = cio.LogFile(filepath.Join(b.logRoot, sandbox.SandboxID+".log"))
	}
	task, err := container.NewTask(ctx, creator)
	if err != nil {
		return observation, fmt.Errorf("create gVisor task: %w", err)
	}
	defer func() {
		if returnError != nil {
			_, _ = task.Delete(b.withNamespace(context.Background()), containerdclient.WithProcessKill)
		}
	}()
	if err := task.Start(ctx); err != nil {
		return observation, fmt.Errorf("start gVisor task: %w", err)
	}
	if b.logger != nil {
		b.logger.Info("sandbox task started", "sandbox_id", sandbox.SandboxID, "runtime_group_id", sandbox.RuntimeGroupID, "runtime", RuntimeName, "pid", task.Pid())
	}
	return Observation{ContainerID: container.ID(), Runtime: RuntimeName, RuntimeGroupID: sandbox.RuntimeGroupID, TaskStatus: string(containerdclient.Running), TaskPID: task.Pid(), Labels: labels}, nil
}

func (b *Backend) Observe(ctx context.Context, sandboxID string) (Observation, error) {
	ctx = b.withNamespace(ctx)
	container, err := b.ownedContainer(ctx, sandboxID)
	if err != nil {
		return Observation{}, err
	}
	info, err := container.Info(ctx)
	if err != nil {
		return Observation{}, err
	}
	result := Observation{ContainerID: container.ID(), Runtime: info.Runtime.Name, RuntimeGroupID: info.Labels[labelRuntimeGroup], Labels: cloneMap(info.Labels), TaskStatus: "absent"}
	task, err := container.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return result, nil
		}
		return Observation{}, err
	}
	status, err := task.Status(ctx)
	if err != nil {
		return Observation{}, err
	}
	result.TaskStatus, result.TaskPID = string(status.Status), task.Pid()
	return result, nil
}

func (b *Backend) UpdateLabels(ctx context.Context, sandboxID string, labels map[string]string) error {
	if err := validateLabelUpdates(labels); err != nil {
		return err
	}
	container, err := b.ownedContainer(b.withNamespace(ctx), sandboxID)
	if err != nil {
		return err
	}
	if _, err := container.SetLabels(b.withNamespace(ctx), labels); err != nil {
		return fmt.Errorf("update sandbox labels: %w", err)
	}
	return nil
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

func (b *Backend) List(ctx context.Context) ([]Observation, error) {
	owned, err := b.ListOwned(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]Observation, 0, len(owned))
	for _, ownedSandbox := range owned {
		observation, observeErr := b.Observe(ctx, ownedSandbox.ContainerID)
		if observeErr != nil {
			return nil, observeErr
		}
		result = append(result, observation)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ContainerID < result[j].ContainerID })
	return result, nil
}

// ListOwned returns instance-owned container metadata without querying task
// state. Startup destruction does not need a task or supervisor health probe.
func (b *Backend) ListOwned(ctx context.Context) ([]Observation, error) {
	ctx = b.withNamespace(ctx)
	containersList, err := b.client.Containers(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]Observation, 0, len(containersList))
	for _, container := range containersList {
		labels, labelErr := container.Labels(ctx)
		if labelErr != nil {
			return nil, labelErr
		}
		if !b.owns(labels) {
			continue
		}
		result = append(result, Observation{ContainerID: container.ID(), Runtime: RuntimeName, RuntimeGroupID: labels[labelRuntimeGroup], Labels: labels})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ContainerID < result[j].ContainerID })
	return result, nil
}

func (b *Backend) Stop(ctx context.Context, sandboxID string, grace time.Duration) error {
	if grace <= 0 {
		grace = 10 * time.Second
	}
	return b.stopTask(ctx, sandboxID, syscall.SIGTERM, grace)
}

func (b *Backend) Kill(ctx context.Context, sandboxID string) error {
	return b.stopTask(ctx, sandboxID, syscall.SIGKILL, 2*time.Second)
}

func (b *Backend) Delete(ctx context.Context, sandboxID string) error {
	ctx = b.withNamespace(ctx)
	container, err := b.ownedContainer(ctx, sandboxID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	if task, taskErr := container.Task(ctx, nil); taskErr == nil {
		if _, deleteErr := task.Delete(ctx, containerdclient.WithProcessKill); deleteErr != nil && !errdefs.IsNotFound(deleteErr) {
			return fmt.Errorf("delete sandbox task: %w", deleteErr)
		}
	} else if !errdefs.IsNotFound(taskErr) {
		return taskErr
	}
	if err := container.Delete(ctx, containerdclient.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("delete sandbox container and snapshot: %w", err)
	}
	return nil
}

func (b *Backend) stopTask(ctx context.Context, sandboxID string, signal syscall.Signal, grace time.Duration) error {
	ctx = b.withNamespace(ctx)
	container, err := b.ownedContainer(ctx, sandboxID)
	if err != nil {
		return err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	status, err := task.Status(ctx)
	if err != nil {
		return err
	}
	if status.Status == containerdclient.Stopped {
		_, err = task.Delete(ctx)
		return err
	}
	wait, err := task.Wait(ctx)
	if err != nil {
		return err
	}
	if err := task.Kill(ctx, signal, containerdclient.WithKillAll); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-wait:
	case <-timer.C:
		if signal != syscall.SIGKILL {
			if err := task.Kill(ctx, syscall.SIGKILL, containerdclient.WithKillAll); err != nil && !errdefs.IsNotFound(err) {
				return err
			}
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	_, err = task.Delete(ctx)
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (b *Backend) imageByDigest(ctx context.Context, digest string) (containerdclient.Image, error) {
	images, err := b.client.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	for _, image := range images {
		if image.Target().Digest.String() == digest {
			return image, nil
		}
	}
	return nil, fmt.Errorf("image digest %s: %w", digest, errdefs.ErrNotFound)
}

func (b *Backend) ownedContainer(ctx context.Context, sandboxID string) (containerdclient.Container, error) {
	container, err := b.client.LoadContainer(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return nil, err
	}
	if !b.owns(labels) {
		return nil, fmt.Errorf("container %q is not owned by kernel instance %s", sandboxID, b.instanceUUID)
	}
	return container, nil
}

func (b *Backend) owns(labels map[string]string) bool {
	return labels[labelManaged] == "true" && labels[labelInstance] == b.instanceUUID
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

func (b *Backend) withNamespace(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, b.namespace)
}

func sandboxSpecOption(sandbox model.SandboxSpec, instanceUUID, callbackAddress string, supervisorPort int, heartbeatInterval, workerStopGrace time.Duration) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, generated *oci.Spec) error {
		if generated.Process == nil || generated.Root == nil || generated.Linux == nil {
			return errors.New("containerd generated an incomplete Linux OCI specification")
		}
		unified, err := resources.UnifiedSettings(sandbox.ResourceLimits)
		if err != nil {
			return err
		}
		generated.Root.Readonly = true
		generated.Process.Terminal = false
		generated.Process.NoNewPrivileges = true
		generated.Process.Capabilities = &specs.LinuxCapabilities{}
		generated.Linux.Resources = &specs.LinuxResources{Unified: unified}
		generated.Linux.CgroupsPath = filepath.ToSlash(filepath.Join("the8020", instanceUUID, sandbox.SandboxID))
		generated.Hostname = sandbox.SandboxID
		if len(generated.Hostname) > 63 {
			generated.Hostname = generated.Hostname[:63]
		}
		foundNetwork := false
		for index := range generated.Linux.Namespaces {
			if generated.Linux.Namespaces[index].Type == specs.NetworkNamespace {
				generated.Linux.Namespaces[index].Path = sandbox.Network.NamespacePath
				foundNetwork = true
			}
		}
		if !foundNetwork {
			generated.Linux.Namespaces = append(generated.Linux.Namespaces, specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: sandbox.Network.NamespacePath})
		}
		for _, mount := range sandbox.Mounts {
			generated.Mounts = append(generated.Mounts, backend.OCIMount(mount))
		}
		if sandbox.Network.SupervisorPort != 0 {
			supervisorPort = sandbox.Network.SupervisorPort
		}
		processConfig := backend.ProcessConfig{
			NodeID:          instanceUUID,
			CallbackAddress: callbackAddress, SupervisorHost: "0.0.0.0", SupervisorPort: supervisorPort,
			InspectorHost: "0.0.0.0", InspectorPort: sandbox.Network.InspectorEndpointPort(),
			SupervisorHeartbeatInterval: heartbeatInterval, WorkerStopGrace: workerStopGrace,
		}
		if err := backend.ValidateProcessConfig(processConfig); err != nil {
			return err
		}
		generated.Process.Env = backend.RuntimeEnvironment(generated.Process.Env, sandbox, processConfig)
		generated.Process.Args = backend.DenoProcessArguments(generated.Process.Args, sandbox, processConfig)
		return nil
	}
}

func ociMount(mount model.Mount) specs.Mount {
	return backend.OCIMount(mount)
}

func mergeEnvironment(existing []string, additions map[string]string) []string {
	values := map[string]string{}
	for _, entry := range existing {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	for key, value := range additions {
		if value == "" && key == "KERNEL_CALLBACK_ADDRESS" {
			continue
		}
		values[key] = value
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

func parentPermissionArgs(args []string, sandbox model.SandboxSpec, callbackAddress string, supervisorPort int) []string {
	return backend.DenoProcessArguments(args, sandbox, backend.ProcessConfig{
		NodeID:          sandbox.SandboxID,
		CallbackAddress: callbackAddress, SupervisorHost: "0.0.0.0", SupervisorPort: supervisorPort,
		InspectorHost: "0.0.0.0", InspectorPort: sandbox.Network.InspectorEndpointPort(),
		SupervisorHeartbeatInterval: 5 * time.Second, WorkerStopGrace: time.Second,
	})
}

func replaceFlag(args []string, prefix string, additions []string) []string {
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
	return append(result, prefix+strings.Join(sorted, ","))
}

func cloneMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
