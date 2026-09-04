package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"the8020/kernel/auth"
	"the8020/kernel/cbus/core"
	"the8020/kernel/cbus/discovery"
	platformconsole "the8020/kernel/console"
	"the8020/kernel/database"
	databaseevaluator "the8020/kernel/database/evaluator"
	"the8020/kernel/debugging"
	"the8020/kernel/development"
	"the8020/kernel/execution/adminrun"
	"the8020/kernel/execution/coordinator"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/execution/pool"
	programrunner "the8020/kernel/execution/programs"
	"the8020/kernel/execution/records"
	executionservices "the8020/kernel/execution/services"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/execution/workers"
	"the8020/kernel/instance"
	mainnetwork "the8020/kernel/network"
	"the8020/kernel/nodes"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/ports"
	runtimehost "the8020/kernel/runtime"
	"the8020/kernel/runtime/callback"
	runtimeoperations "the8020/kernel/runtime/operations"
	"the8020/kernel/sandbox/backend"
	containerdbackend "the8020/kernel/sandbox/backend/containerd"
	rootlessbackend "the8020/kernel/sandbox/backend/rootless"
	sandboxhistory "the8020/kernel/sandbox/history"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
	sandboxmounts "the8020/kernel/sandbox/mounts"
	sandboxnetwork "the8020/kernel/sandbox/network"
	"the8020/kernel/sandbox/state"
	secretstore "the8020/kernel/secrets"
	"the8020/kernel/services"
	"the8020/kernel/settings"
	settingsdb "the8020/kernel/settings/dbstore"
	kernelssh "the8020/kernel/ssh"
	"the8020/kernel/webservices"
)

type runtimeCleanup struct {
	once          sync.Once
	monitorCancel context.CancelFunc
	monitorWait   sync.WaitGroup
	sandboxes     *manager.Manager
	ports         *ports.Manager
	backend       io.Closer
	callback      *callback.Server
	pool          *pool.Controller
	webservices   *webservices.Manager
	jobs          *jobs.Manager
	policy        manager.ShutdownPolicy
	console       *platformconsole.Manager
	publicNetwork *mainnetwork.Manager
	development   *development.Manager
	ssh           *kernelssh.Manager
	nodes         *nodes.Manager
	stopAuth      func()
	err           error
}

const sandboxKernelSocketPath = "/run/the8020/kernel.sock"

type shutdownProgressFunc func(started bool, stepID, step, message string)
type runtimeCleanupFunc func(context.Context, shutdownProgressFunc) error

func reportShutdownProgress(report shutdownProgressFunc, started bool, stepID, step, message string) {
	if report != nil {
		report(started, stepID, step, message)
	}
}

func closeConcurrently(tasks ...func() error) error {
	errorsChannel := make(chan error, len(tasks))
	var wait sync.WaitGroup
	for _, task := range tasks {
		if task == nil {
			continue
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsChannel <- task()
		}()
	}
	wait.Wait()
	close(errorsChannel)
	var joined error
	for err := range errorsChannel {
		joined = errors.Join(joined, err)
	}
	return joined
}

func (c *runtimeCleanup) Close(ctx context.Context, report shutdownProgressFunc) error {
	c.once.Do(func() {
		if c.console != nil {
			c.console.SetRuntime(nil)
		}
		reportShutdownProgress(report, true, "public_http", "public HTTP", "draining the public HTTP listener")
		if c.publicNetwork != nil {
			c.publicNetwork.Close()
		}
		reportShutdownProgress(report, false, "public_http", "public HTTP", "public HTTP listener closed")
		reportShutdownProgress(report, true, "authentication", "authentication maintenance", "stopping authentication-session cleanup")
		if c.stopAuth != nil {
			c.stopAuth()
		}
		reportShutdownProgress(report, false, "authentication", "authentication maintenance", "authentication maintenance stopped")
		reportShutdownProgress(report, true, "runtime_controllers", "runtime controllers", "stopping runtime monitors, reconcilers, schedulers, and timers")
		if c.monitorCancel != nil {
			c.monitorCancel()
			c.monitorWait.Wait()
		}
		var controllerTasks []func() error
		if c.webservices != nil {
			controllerTasks = append(controllerTasks, c.webservices.Close)
		}
		if c.jobs != nil {
			controllerTasks = append(controllerTasks, c.jobs.Close)
		}
		if c.pool != nil {
			controllerTasks = append(controllerTasks, c.pool.Close)
		}
		c.err = errors.Join(c.err, closeConcurrently(controllerTasks...))
		reportShutdownProgress(report, false, "runtime_controllers", "runtime controllers", "runtime background activity stopped")

		reportShutdownProgress(report, true, "runtime_ports", "runtime ports", "closing host port leases")
		if c.ports != nil {
			c.err = errors.Join(c.err, c.ports.CloseAll())
		}
		reportShutdownProgress(report, false, "runtime_ports", "runtime ports", "host port leases closed")

		reportShutdownProgress(report, true, "runtime_sandboxes", "runtime sandboxes", "stopping and deleting gVisor sandboxes")
		if c.sandboxes != nil {
			c.err = errors.Join(c.err, c.sandboxes.Shutdown(ctx, c.policy))
		}
		reportShutdownProgress(report, false, "runtime_sandboxes", "runtime sandboxes", "gVisor sandbox cleanup finished")

		reportShutdownProgress(report, true, "runtime_backends", "runtime backends", "closing callback and sandbox backend endpoints")
		var endpointTasks []func() error
		if c.callback != nil {
			endpointTasks = append(endpointTasks, func() error { return c.callback.Close(ctx) })
		}
		if c.backend != nil {
			endpointTasks = append(endpointTasks, c.backend.Close)
		}
		if c.ssh != nil {
			c.err = errors.Join(c.err, c.ssh.Close(ctx))
		}
		if c.console != nil {
			c.err = errors.Join(c.err, c.console.Close())
		}
		if c.development != nil {
			c.err = errors.Join(c.err, c.development.Close(ctx))
		}
		if c.nodes != nil {
			c.err = errors.Join(c.err, c.nodes.Close())
		}
		c.err = errors.Join(c.err, closeConcurrently(endpointTasks...))
		reportShutdownProgress(report, false, "runtime_backends", "runtime backends", "runtime endpoints closed")
	})
	return c.err
}

func initializeRuntime(ctx context.Context, root, instanceUUID string, paths instance.Paths, settingManager *settings.Manager, systemDatabase *database.Manager, commandRegistry *core.Registry, serviceSet *services.Services, repositoryMu *sync.RWMutex, logger *slog.Logger) (*services.RuntimeServices, runtimeCleanupFunc) {
	cleanup := &runtimeCleanup{policy: manager.ShutdownDestroy}
	closeRuntime := cleanup.Close
	if _, err := systemDatabase.InitializeCatalog(ctx); err != nil {
		return &services.RuntimeServices{Failure: "database catalog initialization failed: " + err.Error()}, closeRuntime
	}
	packageCatalog, err := workspacepackages.NewCatalog(paths.Packages, logger)
	if err != nil {
		return &services.RuntimeServices{Failure: "initialize package catalog: " + err.Error()}, closeRuntime
	}
	versions, err := runtimehost.LoadVersionsFile(paths.RuntimeVersionsFile)
	if err != nil {
		return &services.RuntimeServices{Failure: err.Error()}, closeRuntime
	}
	manifestRoot := paths.Root
	socket := activeString(settingManager, "sandbox.containerd.socket", "/run/containerd/containerd.sock")
	doctorConfig := runtimehost.DoctorConfig{
		Root: manifestRoot, Versions: versions, ContainerdSocket: socket,
		ImageRecordPath: filepath.Join(paths.RuntimeFullImage, "image.json"),
		SmokeRecordPath: filepath.Join(paths.RuntimeFullImage, "smoke.json"),
	}
	doctor := runtimehost.NewDoctor(doctorConfig)
	rootlessDoctor := runtimehost.NewRootlessDoctor(runtimehost.RootlessDoctorConfig{
		Root: manifestRoot, Versions: versions,
		RunscPath: paths.Runsc,
		RootFS:    filepath.Join(paths.RuntimeRootlessImage, "rootfs"), RecordPath: filepath.Join(paths.RuntimeRootlessImage, "image.json"),
		SmokeRecordPath: filepath.Join(paths.RuntimeRootlessImage, "smoke.json"),
	})
	configuredMode := activeString(settingManager, "sandbox.runtime.mode", string(runtimehost.ModeAuto))
	runtimeServices := &services.RuntimeServices{Versions: versions, Doctor: doctor, RootlessDoctor: rootlessDoctor}
	if enabled, ok := activeBool(settingManager, "sandbox.enabled"); !ok || !enabled {
		runtimeServices.Isolation = runtimehost.NewIsolationReport(configuredMode, runtimehost.ModeUnavailable, "sandbox runtime is disabled", false, false)
		runtimeServices.Failure = "sandbox runtime is disabled or its settings are unavailable"
		return runtimeServices, closeRuntime
	}
	fullReport := doctor.Inspect(ctx)
	if fullReport.Ready {
		probeBackend, probeErr := containerdbackend.Connect(ctx, containerdbackend.Config{Socket: socket, InstanceUUID: instanceUUID})
		if probeErr != nil {
			fullReport.Ready = false
			fullReport.Failures = append(fullReport.Failures, "containerd API probe failed: "+probeErr.Error())
		} else {
			doctorConfig.Probe = probeBackend
			doctor = runtimehost.NewDoctor(doctorConfig)
			runtimeServices.Doctor = doctor
			fullReport = doctor.Inspect(ctx)
			_ = probeBackend.Close()
		}
	}
	rootlessReport := rootlessDoctor.Inspect(ctx)
	selectedMode, selectionReason, selectionErr := runtimehost.SelectMode(configuredMode, fullReport.Ready, rootlessReport.Ready)
	runtimeServices.Isolation = runtimehost.NewIsolationReport(configuredMode, selectedMode, selectionReason, fullReport.Ready, rootlessReport.Ready)
	if selectionErr != nil {
		runtimeServices.Failure = fmt.Sprintf("%v; full: %s; rootless: %s", selectionErr, strings.Join(fullReport.Failures, "; "), strings.Join(rootlessReport.Failures, "; "))
		return runtimeServices, closeRuntime
	}
	configuredRuntimeName := activeString(settingManager, "sandbox.runtime.name", containerdbackend.RuntimeName)
	if selectedMode == runtimehost.ModeRootless {
		configuredRuntimeName = containerdbackend.RuntimeName
	}
	selectedDigest := fullReport.RuntimeImageDigest
	if selectedMode == runtimehost.ModeRootless {
		selectedDigest = rootlessReport.RuntimeImageDigest
	}
	if err := validateConfiguredRuntimeIdentity(
		configuredRuntimeName,
		activeString(settingManager, "sandbox.image.reference", versions.RuntimeImage.Name),
		activeString(settingManager, "sandbox.image.digest", zeroImageDigest),
		versions,
		selectedDigest,
	); err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	stateStore, err := state.New(paths.RuntimeGroups)
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	subnet := activeString(settingManager, "sandbox.network.subnet", "10.88.0.0/16")
	callbackServer, err := callback.New(callback.Config{Store: stateStore, ProtocolVersion: versions.RuntimeProtocolVersion, SocketPath: paths.RuntimeKernelSocket, Database: systemDatabase, AdminBus: commandRegistry})
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	if err := callbackServer.Start(); err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	cleanup.callback = callbackServer
	heartbeatInterval := activeDuration(settingManager, "runtime.supervisor.heartbeat_interval", 5*time.Second)
	heartbeatTimeout := activeDuration(settingManager, "runtime.supervisor.heartbeat_timeout", 15*time.Second)
	if heartbeatTimeout <= heartbeatInterval {
		runtimeServices.Failure = "runtime supervisor heartbeat timeout must exceed its interval"
		return runtimeServices, closeRuntime
	}
	portManager, err := ports.New(paths.RuntimePorts, false, logger)
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	cleanup.ports, runtimeServices.Ports = portManager, portManager
	workerStopGrace := activeDuration(settingManager, "runtime.worker.stop_grace_period", time.Second)
	var sandboxBackend backend.Backend
	var networkManager manager.Network
	var sandboxLogPath func(string) string
	if selectedMode == runtimehost.ModeFull {
		logRoot := filepath.Join(paths.Runtime, "logs")
		fullBackend, connectErr := containerdbackend.Connect(ctx, containerdbackend.Config{
			Socket: socket, InstanceUUID: instanceUUID, LogRoot: logRoot, KernelSocketPath: sandboxKernelSocketPath,
			RunscConfigPath:             filepath.Join(paths.RuntimeDefinitions, "runsc.toml"),
			SupervisorHeartbeatInterval: heartbeatInterval, WorkerStopGrace: workerStopGrace, Logger: logger,
		})
		if connectErr != nil {
			runtimeServices.Failure = connectErr.Error()
			return runtimeServices, closeRuntime
		}
		sandboxBackend, cleanup.backend = fullBackend, fullBackend
		sandboxLogPath = func(sandboxID string) string { return filepath.Join(logRoot, sandboxID+".log") }
		firewall, firewallErr := sandboxnetwork.NewNFTFirewall(sandboxnetwork.NFTFirewallConfig{InstanceUUID: instanceUUID, SandboxSubnet: subnet})
		if firewallErr != nil {
			runtimeServices.Failure = firewallErr.Error()
			return runtimeServices, closeRuntime
		}
		networkManager, err = sandboxnetwork.New(sandboxnetwork.Config{
			InstanceUUID: instanceUUID,
			PluginPaths:  []string{"/opt/cni/bin"},
			ConfigPath:   "/etc/cni/net.d/the8020.conflist",
			NetworkName:  activeString(settingManager, "sandbox.network.name", "the8020"),
			Bridge:       activeString(settingManager, "sandbox.network.bridge", "the8020"), Subnet: subnet,
			CacheDir: filepath.Join(paths.Runtime, "cni-cache"), StateRoot: filepath.Join(paths.Runtime, "network"), Firewall: firewall,
		})
	} else {
		logRoot := filepath.Join(paths.Runtime, "logs", "rootless")
		rootlessBackend, backendErr := rootlessbackend.New(rootlessbackend.Config{
			RunscPath: rootlessReport.RunscPath, RootFS: rootlessReport.RootFS,
			StateRoot: filepath.Join(paths.Runtime, "rootless", "sandboxes"), RuntimeRoot: filepath.Join(paths.Runtime, "rootless", "runsc"),
			LogRoot: logRoot, InstanceUUID: instanceUUID, KernelSocketPath: sandboxKernelSocketPath,
			SupervisorHeartbeatInterval: heartbeatInterval, WorkerStopGrace: workerStopGrace,
			StartTimeout: activeDuration(settingManager, "runtime.sandbox.startup_timeout", 30*time.Second), Logger: logger,
		})
		if backendErr != nil {
			runtimeServices.Failure = backendErr.Error()
			return runtimeServices, closeRuntime
		}
		sandboxBackend, cleanup.backend = rootlessBackend, rootlessBackend
		sandboxLogPath = func(sandboxID string) string { return filepath.Join(logRoot, sandboxID) }
		networkManager, err = sandboxnetwork.NewLoopback(filepath.Join(paths.Runtime, "rootless", "network"))
	}
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	historyStore, err := sandboxhistory.New(sandboxhistory.Config{Root: paths.RuntimeSandboxHistory, LogPath: sandboxLogPath})
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	historyRetention := activeDuration(settingManager, "sandbox.history.retention", sandboxhistory.DefaultRetention)
	supervisorClient, err := supervisor.New(supervisor.Config{ProtocolVersion: versions.RuntimeProtocolVersion})
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	nodeLimits := runtimeNodeLimits(settingManager, paths.Runtime)
	maximumWorkersPerSandbox := activeInt(settingManager, "runtime.sandbox.maximum_workers", 64)
	sandboxManager, err := manager.New(manager.Config{
		InstanceUUID: instanceUUID, StartupTimeout: activeDuration(settingManager, "runtime.sandbox.startup_timeout", 30*time.Second),
		StopGrace: activeDuration(settingManager, "runtime.sandbox.stop_grace_period", 10*time.Second), Store: stateStore,
		Backend: sandboxBackend, Network: networkManager, Supervisor: supervisorClient, Ports: portManager,
		History: historyStore, HistoryRetention: historyRetention, NodeLimits: nodeLimits,
	})
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	cleanup.sandboxes, runtimeServices.Sandboxes = sandboxManager, sandboxManager
	if _, err := sandboxManager.CleanupHistory(); err != nil {
		if logger != nil {
			logger.Error("initial sandbox history cleanup failed", "error", err)
		}
	}
	if policy := activeString(settingManager, "sandbox.shutdown_policy", string(manager.ShutdownDestroy)); policy == string(manager.ShutdownLeave) {
		cleanup.policy = manager.ShutdownLeave
	}
	startupPolicy := manager.StartupPolicy(activeString(settingManager, "sandbox.startup_policy", string(manager.StartupDestroy)))
	startupReport, err := sandboxManager.Startup(ctx, startupPolicy)
	if err != nil {
		runtimeServices.Failure = "runtime startup failed: " + err.Error()
		return runtimeServices, closeRuntime
	}
	workerManager, err := workers.New(sandboxManager, supervisorClient, nodeLimits.MaximumWorkers, maximumWorkersPerSandbox, systemDatabase.Status().Backend)
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	runtimeServices.Workers = workerManager
	callbackServer.SetWorkerInvoker(workerManager)
	imageDigest := selectedDigest
	serviceResources := resourceLimits(settingManager, "service")
	jobResources := resourceLimits(settingManager, "job")
	egressAllowed := true
	baseMounts := []model.Mount{
		{Source: paths.RuntimeAttachments, Target: "/artifacts", ReadOnly: true, Purpose: "runtime-artifacts", Persistence: "kernel"},
		{Source: paths.RuntimeKernelSocketDir, Target: "/run/the8020", ReadOnly: true, Purpose: "kernel-api", Persistence: "kernel"},
	}
	serviceMounts := append(append([]model.Mount(nil), baseMounts...),
		model.Mount{Source: packageCatalog.PackagesRoot(), Target: "/workspace/packages", ReadOnly: true, Purpose: "workspace-packages", Persistence: "shared"},
	)
	serviceProfile := runtimeProfile(model.WorkloadService, imageDigest, serviceMounts, serviceResources, egressAllowed)
	serviceProfile.Permissions.ReadPaths = append(serviceProfile.Permissions.ReadPaths, "/opt/runtime", "/workspace/packages")
	jobProfile := runtimeProfile(model.WorkloadJob, imageDigest, serviceMounts, jobResources, egressAllowed)
	jobProfile.Permissions.ReadPaths = append(jobProfile.Permissions.ReadPaths, "/opt/runtime", "/workspace/packages")
	workspaceMounts, err := sandboxmounts.NewPolicy([]string{root}, paths.Kernel, socket, true)
	if err != nil {
		runtimeServices.Failure = "initialize development workspace policy: " + err.Error()
		return runtimeServices, closeRuntime
	}
	lifecycle := model.LifecyclePolicy{DestroyWhenIdle: true, StopGracePeriod: activeDuration(settingManager, "runtime.sandbox.stop_grace_period", 10*time.Second)}
	warmController, err := pool.New(sandboxManager, []pool.Template{
		{Profile: serviceProfile, Resources: serviceResources, Lifecycle: lifecycle, Network: activeString(settingManager, "sandbox.network.name", "the8020")},
		{Profile: jobProfile, Resources: jobResources, Lifecycle: lifecycle, Network: activeString(settingManager, "sandbox.network.name", "the8020")},
	}, logger)
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	cleanup.pool, runtimeServices.Pool = warmController, warmController
	if err := warmController.Start(ctx, activeInt(settingManager, "sandbox.warm_pool.size", 0)); err != nil {
		runtimeServices.Failure = "initialize warm pool: " + err.Error()
		return runtimeServices, closeRuntime
	}
	groupCoordinator, err := coordinator.New(sandboxManager, maximumWorkersPerSandbox, warmController)
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}

	serviceStore, err := records.New(paths.RuntimeServicePools)
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	serviceManager, err := executionservices.New(groupCoordinator, workerManager, serviceStore, executionservices.Policy{
		Strategy: grouping(settingManager, "execution.grouping.service"), Profile: serviceProfile, Resources: serviceResources, Lifecycle: lifecycle,
		WorkspaceMounts: workspaceMounts, Logger: logger,
	})
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	jobManager, err := jobs.New(groupCoordinator, workerManager, jobs.Policy{
		Strategy: grouping(settingManager, "execution.grouping.job"), Profile: jobProfile, Resources: jobResources, Lifecycle: lifecycle,
		MaximumParallel: activeInt(settingManager, "job.default.maximum_parallel_workers", 4), QueuedExecutionLimit: activeInt(settingManager, "job.default.queued_execution_limit", 1024),
		ExecutionTimeout: activeDuration(settingManager, "job.default.execution_timeout", 5*time.Minute),
		Reuse:            activeBoolDefault(settingManager, "job.default.worker_reuse", false), IdleRuntimeTimeout: activeDuration(settingManager, "job.default.idle_runtime_timeout", time.Minute),
		WorkspaceMounts: workspaceMounts, Logger: logger,
	})
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	cleanup.jobs = jobManager
	tableEvaluator, err := databaseevaluator.New(databaseevaluator.Config{Packages: packageCatalog, Jobs: jobManager, Database: systemDatabase})
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	systemDatabase.SetDefinitionEvaluator(tableEvaluator.Evaluate)
	systemDatabase.SetFullSynchronizer(tableEvaluator.SynchronizeAll)
	systemDatabase.SetSourceEvaluator(tableEvaluator.InspectDefinition)
	_, pendingDeployment, err := systemDatabase.PendingDeployment(ctx)
	if err != nil {
		runtimeServices.Failure = "inspect pending database schema deployment: " + err.Error()
		return runtimeServices, closeRuntime
	}
	freshDatabase := !systemDatabase.Status().Initialized
	if freshDatabase {
		if _, err := tableEvaluator.SynchronizeInitialSchemas(ctx, true); err != nil {
			runtimeServices.Failure = "initial database schema synchronization failed: " + err.Error()
			return runtimeServices, closeRuntime
		}
	}
	globalSettings, err := settingsdb.New(systemDatabase)
	if err != nil {
		runtimeServices.Failure = "initialize global settings: " + err.Error()
		return runtimeServices, closeRuntime
	}
	if err := settingManager.AttachGlobal(ctx, globalSettings); err != nil {
		runtimeServices.Failure = "load global settings: " + err.Error()
		return runtimeServices, closeRuntime
	}
	if err := settingManager.RegisterApplier([]string{"logging.enabled", "logging.split_period", "logging.max_file_size", "logging.max_total_size"}, serviceSet.Logging); err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	if err := settingManager.RegisterApplier([]string{
		"database.maximum_open_connections", "database.maximum_idle_connections",
		"database.maximum_result_rows", "database.maximum_result_bytes",
	}, systemDatabase); err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	secretManager, err := secretstore.New(secretstore.Config{Database: systemDatabase})
	if err != nil {
		runtimeServices.Failure = "initialize shared secrets: " + err.Error()
		return runtimeServices, closeRuntime
	}
	forwardingSecret, err := secretManager.EnsureRandom(ctx, "system.node.forwarding", 32)
	if err != nil {
		runtimeServices.Failure = "initialize node forwarding secret: " + err.Error()
		return runtimeServices, closeRuntime
	}
	packageStore, err := workspacepackages.New(workspacepackages.Config{
		WorkspaceRoot: root, PackagesRoot: paths.Packages, RepositoryMu: repositoryMu,
		Secrets: secretManager, Database: systemDatabase, Defaults: serviceFrameworkDefaults(settingManager), Logger: logger,
	})
	if err != nil {
		runtimeServices.Failure = "initialize packages: " + err.Error()
		return runtimeServices, closeRuntime
	}
	programRunner, err := programrunner.New(packageStore, jobManager)
	if err != nil {
		runtimeServices.Failure = "initialize program runner: " + err.Error()
		return runtimeServices, closeRuntime
	}
	commandIndexer, err := discovery.New(packageStore, programRunner, commandRegistry)
	if err != nil {
		runtimeServices.Failure = "initialize package command index: " + err.Error()
		return runtimeServices, closeRuntime
	}
	runtimeServices.Programs = programRunner
	runtimeServices.ReindexCommands = func(ctx context.Context) (core.Result, error) {
		report, err := commandIndexer.Reindex(ctx)
		return core.Result{"revision": report.Revision, "packages": report.Packages, "commands": report.Commands, "diagnostics": report.Diagnostics}, err
	}
	packageCommits, err := tableEvaluator.PackageSet(ctx)
	if err != nil {
		runtimeServices.Failure = "inspect installed package set: " + err.Error()
		return runtimeServices, closeRuntime
	}
	activationCoordinator, err := workspacepackages.NewActivationCoordinator(workspacepackages.ActivationCoordinatorConfig{
		Database: systemDatabase, Schema: tableEvaluator, Packages: packageStore, Jobs: jobManager,
		ValidateCandidates: commandIndexer.ValidateCandidates,
		RefreshCommands: func(ctx context.Context) error {
			_, err := commandIndexer.Reindex(ctx)
			return err
		},
	})
	if err != nil {
		runtimeServices.Failure = "initialize package activation: " + err.Error()
		return runtimeServices, closeRuntime
	}
	packageStore.SetSchemaDeployment(activationCoordinator)
	serviceSet.PublishPackageManagement(packageStore)
	if freshDatabase {
		pending, err := activationCoordinator.Pending(ctx)
		if err != nil {
			runtimeServices.Failure = "inspect bootstrap package activation: " + err.Error()
			return runtimeServices, closeRuntime
		}
		if pending {
			err = activationCoordinator.Recover(ctx)
		} else {
			err = activationCoordinator.Bootstrap(ctx, packageCommits)
		}
		if err != nil {
			systemDatabase.SetInitializationFailure(context.WithoutCancel(ctx), err)
			runtimeServices.Failure = "activate bootstrap packages: " + err.Error()
			return runtimeServices, closeRuntime
		}
		if err := systemDatabase.CompleteInitialization(ctx, packageCommits); err != nil {
			systemDatabase.SetInitializationFailure(context.WithoutCancel(ctx), err)
			runtimeServices.Failure = "complete database initialization: " + err.Error()
			return runtimeServices, closeRuntime
		}
	} else {
		if pendingDeployment {
			if err := activationCoordinator.Recover(ctx); err != nil {
				runtimeServices.Failure = "recover package activation: " + err.Error()
				return runtimeServices, closeRuntime
			}
		}
		if err := packageStore.ValidateInstalled(ctx, packageCommits); err != nil {
			runtimeServices.Failure = "validate installed package set: " + err.Error()
			return runtimeServices, closeRuntime
		}
	}
	if err := tableEvaluator.UseActivatedPackages(packageStore); err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	argonParameters := auth.Argon2Parameters{
		Memory:       uint32(activeInt(settingManager, "auth.argon2.memory", 64*1024)),
		Iterations:   uint32(activeInt(settingManager, "auth.argon2.iterations", 3)),
		Parallelism:  uint8(activeInt(settingManager, "auth.argon2.parallelism", 1)),
		SaltLength:   16,
		OutputLength: 32,
	}
	passwordHasher, err := auth.NewPasswordHasher(argonParameters, nil)
	if err != nil {
		runtimeServices.Failure = "initialize password hashing: " + err.Error()
		return runtimeServices, closeRuntime
	}
	operationDispatcher, err := runtimeoperations.New(serviceSet, passwordHasher)
	if err != nil {
		runtimeServices.Failure = "initialize runtime operations: " + err.Error()
		return runtimeServices, closeRuntime
	}
	callbackServer.SetRuntimeOperations(operationDispatcher)
	if _, err := commandIndexer.Reindex(ctx); err != nil {
		runtimeServices.Failure = "index package commands: " + err.Error()
		return runtimeServices, closeRuntime
	}
	packageFollower, err := workspacepackages.NewPackageRevisionFollower(ctx, packageStore, packageCommits)
	if err != nil {
		runtimeServices.Failure = "initialize package-set convergence: " + err.Error()
		return runtimeServices, closeRuntime
	}
	serviceFollower, err := workspacepackages.NewServiceRevisionFollower(ctx, packageStore)
	if err != nil {
		runtimeServices.Failure = "initialize service-state convergence: " + err.Error()
		return runtimeServices, closeRuntime
	}
	authentication, err := auth.New(auth.Config{
		Database: systemDatabase, SessionDuration: activeDuration(settingManager, "auth.session_duration", 12*time.Hour),
		CleanupInterval: activeDuration(settingManager, "auth.cleanup_interval", 15*time.Minute),
		Cookie: auth.CookieConfig{
			Name:     activeString(settingManager, "auth.cookie.name", "the8020_auth"),
			Secure:   activeBoolDefault(settingManager, "auth.cookie.secure", false),
			SameSite: activeString(settingManager, "auth.cookie.same_site", "lax"),
		},
		Argon2: argonParameters,
		Hasher: passwordHasher,
	})
	if err != nil {
		runtimeServices.Failure = "initialize authentication: " + err.Error()
		return runtimeServices, closeRuntime
	}
	authContext, cancelAuth := context.WithCancel(ctx)
	var authWait sync.WaitGroup
	authWait.Add(1)
	go func() {
		defer authWait.Done()
		authentication.RunCleanup(authContext, func(cleanupErr error) {
			logger.Error("authentication-session cleanup failed", "error", cleanupErr)
		})
	}()
	cleanup.stopAuth = func() { cancelAuth(); authWait.Wait() }
	nodeManager, err := nodes.New(systemDatabase, instanceUUID, forwardingSecret.Value)
	if err != nil {
		runtimeServices.Failure = "initialize node topology: " + err.Error()
		return runtimeServices, closeRuntime
	}
	workerManager.SetNodeRouter(nodeManager)
	nodeManager.SetWorkerInvoker(workerManager)
	cleanup.nodes = nodeManager
	developmentRunsc := developmentRunscConfig(ctx, root, paths, settingManager)
	developmentManager, err := development.New(development.Config{
		Root: root, PackagesRoot: paths.Packages, UsersRoot: paths.Users,
		RuntimeRoot: paths.RuntimeDevelopment, ImageRoot: filepath.Join(paths.DevelopmentImage, "rootfs"),
		ImageRecord: filepath.Join(paths.DevelopmentImage, "image.json"), MountProfile: development.DefaultMountProfile(),
		ActivationGateway: development.NewCommandBusGateway(commandRegistry), RepositoryMu: repositoryMu,
		Driver: development.NewRunscDriver(development.RunscConfig{
			RunscPath: developmentRunsc.path, RuntimeRoot: filepath.Join(paths.RuntimeDevelopment, "runsc"),
			SandboxRoot: filepath.Join(paths.RuntimeDevelopment, "sandboxes"), LogRoot: filepath.Join(paths.RuntimeDevelopment, "logs"),
			Rootless: developmentRunsc.rootless,
		}),
		Logger: logger,
	})
	if err != nil {
		runtimeServices.Failure = "initialize development sandboxes: " + err.Error()
		return runtimeServices, closeRuntime
	}
	cleanup.development = developmentManager
	developmentManager.SetSchemaDeployment(activationCoordinator)
	mainPort, ok := settingManager.Active("network.main_port")
	if !ok {
		runtimeServices.Failure = "network.main_port is not registered"
		return runtimeServices, closeRuntime
	}
	publicNetwork, err := mainnetwork.New(int(mainPort.(int64)), activeString(settingManager, "network.root_alias", "the8020/uui/shell/"))
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	cleanup.publicNetwork = publicNetwork
	if err := settingManager.RegisterApplier([]string{"network.main_port"}, publicNetwork); err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	consoleManager, err := platformconsole.New(platformconsole.Config{Authentication: authentication, Development: developmentManager})
	if err != nil {
		runtimeServices.Failure = "initialize sandbox console broker: " + err.Error()
		return runtimeServices, closeRuntime
	}
	cleanup.console = consoleManager
	consoleManager.SetRuntime(sandboxManager)
	if err := publicNetwork.RegisterRoute(platformconsole.Route, consoleManager); err != nil {
		runtimeServices.Failure = "register sandbox console route: " + err.Error()
		return runtimeServices, closeRuntime
	}
	sshPort, ok := settingManager.Active("network.ssh_port")
	if !ok {
		runtimeServices.Failure = "network.ssh_port is not registered"
		return runtimeServices, closeRuntime
	}
	sshManager, err := kernelssh.New(kernelssh.Config{
		Port: int(sshPort.(int64)), HostKeyPath: paths.SSHHostKey, Authentication: authentication,
		Development: developmentManager, Consoles: consoleManager, Logger: logger,
	})
	if err != nil {
		runtimeServices.Failure = "initialize SSH server: " + err.Error()
		return runtimeServices, closeRuntime
	}
	cleanup.ssh = sshManager
	if err := settingManager.RegisterApplier([]string{"network.ssh_port"}, sshManager); err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	callbackServer.SetAuthentication(authentication, authentication)
	adminManager, err := adminrun.New(adminrun.Config{InstanceRoot: root, ArtifactsRoot: paths.RuntimeAttachments, Jobs: jobManager})
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	debugManager, err := debugging.New(debugging.Config{
		Ports: portManager, Enabled: activeBoolDefault(settingManager, "sandbox.debug.enabled", false),
		BindAddress:     activeString(settingManager, "sandbox.debug.bind_address", "127.0.0.1"),
		DefaultDuration: time.Duration(activeInt(settingManager, "sandbox.debug.lease_timeout", 900)) * time.Second,
	})
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	runtimeServices.Jobs = jobManager
	runtimeServices.AdminRun, runtimeServices.Debugging = adminManager, debugManager
	if err := restoreRuntimeWorkloads(ctx, sandboxManager, serviceManager, jobManager, portManager, startupReport.Terminated, logger); err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	webServiceManager, err := webservices.New(webservices.Config{
		Definitions:       packageStore,
		Pools:             serviceManager,
		Router:            publicNetwork,
		ObservedRoot:      paths.RuntimeServices,
		ReconcileInterval: activeDuration(settingManager, "services.reconcile_interval", time.Second),
		StartupTimeout:    activeDuration(settingManager, "services.startup_timeout", 30*time.Second),
		Logger:            logger,
		Authentication:    authentication,
		RuntimeRequests:   authentication,
		NodeID:            instanceUUID,
		Database:          systemDatabase,
		Nodes:             nodeManager,
	})
	if err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	nodeManager.SetCapacityProvider(&runtimeNodeCapacityProvider{sandboxes: sandboxManager, workers: workerManager, services: serviceManager})
	if err := nodeManager.Start(webServiceManager); err != nil {
		runtimeServices.Failure = err.Error()
		return runtimeServices, closeRuntime
	}
	serviceSet.PublishPlatform(services.PlatformServices{
		Network: publicNetwork, Nodes: nodeManager, Auth: authentication, Secrets: secretManager,
		Packages: packageStore, Development: developmentManager,
	})
	cleanup.webservices, runtimeServices.Services = webServiceManager, webServiceManager
	callbackServer.SetPersistentExecutionCompleter(webServiceManager)
	webServiceManager.StartReconciler(ctx)
	startRuntimeMonitor(cleanup, sandboxManager, serviceManager, jobManager, systemDatabase, settingManager, &runtimeSharedState{packages: packageFollower, serviceChanges: serviceFollower, services: webServiceManager, commands: commandIndexer, topology: nodeManager, nextTopology: time.Now().Add(30 * time.Second)}, publicNetwork, heartbeatInterval, heartbeatTimeout, logger)
	return runtimeServices, closeRuntime
}

type runtimeNodeCapacityProvider struct {
	sandboxes *manager.Manager
	workers   *workers.Manager
	services  *executionservices.Manager
}

func (p *runtimeNodeCapacityProvider) NodeCapacity(ctx context.Context) (nodes.Capacity, error) {
	sandboxes, err := p.sandboxes.Capacity()
	if err != nil {
		return nodes.Capacity{}, err
	}
	workerRecords, err := p.workers.List(ctx, "")
	if err != nil {
		return nodes.Capacity{}, err
	}
	serviceRecords, err := p.services.List()
	if err != nil {
		return nodes.Capacity{}, err
	}
	capacity := nodes.Capacity{
		TemporaryStorageBudgetBytes: sandboxes.Limits.TemporaryStorageBytes, TemporaryStorageReservedBytes: sandboxes.TemporaryStorageBytes,
		MaximumSandboxes: sandboxes.Limits.MaximumSandboxes, SandboxCount: sandboxes.SandboxCount,
		MaximumWorkers: sandboxes.Limits.MaximumWorkers, WorkerCount: len(workerRecords), UpdatedAt: time.Now().UTC(),
	}
	capacity.TemporaryStorageAvailableBytes = remainingInt64(capacity.TemporaryStorageBudgetBytes, capacity.TemporaryStorageReservedBytes)
	capacity.AvailableSandboxes = remainingInt(capacity.MaximumSandboxes, capacity.SandboxCount)
	capacity.AvailableWorkers = remainingInt(capacity.MaximumWorkers, capacity.WorkerCount)
	inFlight := make(map[string]int, len(workerRecords))
	for _, worker := range workerRecords {
		inFlight[worker.Worker.WorkerID] = worker.Worker.InFlight
	}
	byService := map[string]*nodes.ServiceCapacity{}
	for _, record := range serviceRecords {
		if record.State != "READY" {
			continue
		}
		serviceID := record.LogicalServiceID
		if serviceID == "" {
			serviceID = record.ServiceID
		}
		item := byService[serviceID]
		if item == nil {
			item = &nodes.ServiceCapacity{ServiceID: serviceID}
			byService[serviceID] = item
		}
		item.SandboxCount++
		item.HealthySandboxes++
		item.WorkerCount += len(record.WorkerIDs)
		item.ExecutionSlots += len(record.WorkerIDs) * record.ConcurrencyPerWorker
		for _, workerID := range record.WorkerIDs {
			item.OccupiedSlots += inFlight[workerID]
		}
	}
	for _, item := range byService {
		capacity.Services = append(capacity.Services, *item)
		capacity.RunningServiceSandboxes += item.SandboxCount
		capacity.HealthyServiceSandboxes += item.HealthySandboxes
		capacity.ExecutionSlots += item.ExecutionSlots
		capacity.OccupiedExecutionSlots += item.OccupiedSlots
	}
	sort.Slice(capacity.Services, func(i, j int) bool { return capacity.Services[i].ServiceID < capacity.Services[j].ServiceID })
	canCreateSandbox := capacity.AvailableSandboxes > 0 && capacity.TemporaryStorageAvailableBytes > 0
	capacity.Accepting = capacity.AvailableWorkers > 0 && (capacity.SandboxCount > 0 || canCreateSandbox)
	return capacity, nil
}

func remainingInt64(limit, used int64) int64 {
	if limit <= 0 {
		return int64(^uint64(0) >> 1)
	}
	return max(limit-used, int64(0))
}

func remainingInt(limit, used int) int {
	if limit <= 0 {
		return int(^uint(0) >> 1)
	}
	return max(limit-used, 0)
}

func serviceFrameworkDefaults(manager *settings.Manager) workspacepackages.FrameworkDefaults {
	return workspacepackages.FrameworkDefaults{
		SessionKeepAlive: activeDuration(manager, "services.default_session_keep_alive", 10*time.Minute),
		Scaling: workspacepackages.ScalingConfiguration{
			MinimumWorkers:       activeInt(manager, "services.default_minimum_workers", 0),
			MaximumWorkers:       activeInt(manager, "services.default_maximum_workers", 0),
			ConcurrencyPerWorker: activeInt(manager, "services.default_concurrency_per_worker", 32),
			TargetUtilization:    float64(activeInt(manager, "services.default_target_utilization_percent", 70)) / 100,
			WorkerKeepAlive:      activeDuration(manager, "services.default_worker_keep_alive", 2*time.Minute),
		},
		Placement: workspacepackages.PlacementConfiguration{
			MinimumSandboxes:  activeInt(manager, "services.default_minimum_sandboxes", 0),
			WorkersPerSandbox: activeInt(manager, "services.default_workers_per_sandbox", 4),
		},
		Timeouts: workspacepackages.TimeoutConfiguration{
			Request: activeDuration(manager, "services.default_request_timeout", 30*time.Second),
			Drain:   activeDuration(manager, "services.default_drain_timeout", 30*time.Second),
		},
		DependencyMode: "cached-only",
	}
}

const zeroImageDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

func validateConfiguredRuntimeIdentity(runtimeName, imageReference, imageDigest string, versions runtimehost.Versions, observedDigest string) error {
	if runtimeName != containerdbackend.RuntimeName {
		return fmt.Errorf("configured full sandbox runtime %q is unsupported; full mode requires %s", runtimeName, containerdbackend.RuntimeName)
	}
	if imageReference != versions.RuntimeImage.Name {
		return fmt.Errorf("configured sandbox image reference %q does not match pinned image %q", imageReference, versions.RuntimeImage.Name)
	}
	if imageDigest == zeroImageDigest {
		return nil
	}
	if len(imageDigest) != len("sha256:")+64 || !strings.HasPrefix(imageDigest, "sha256:") {
		return errors.New("configured sandbox image digest is not an immutable sha256 digest")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(imageDigest, "sha256:")); err != nil {
		return errors.New("configured sandbox image digest is not an immutable sha256 digest")
	}
	if observedDigest != "" && imageDigest != observedDigest {
		return fmt.Errorf("configured sandbox image digest %s does not match imported image %s", imageDigest, observedDigest)
	}
	return nil
}

type runtimeFailureSink interface {
	FailGroup(string, string) error
}

type unavailableServiceSink interface {
	RetireUnavailable(string, string) error
}

func restoreRuntimeWorkloads(ctx context.Context, sandboxManager *manager.Manager, serviceManager *executionservices.Manager, jobManager *jobs.Manager, portManager *ports.Manager, terminated []manager.HealthFailure, logger *slog.Logger) error {
	var terminationErr error
	for _, failure := range terminated {
		terminationErr = errors.Join(terminationErr, serviceManager.FailGroup(failure.RuntimeGroupID, failure.Reason), jobManager.FailGroup(failure.RuntimeGroupID, failure.Reason))
	}
	if terminationErr != nil {
		logRuntimeRecoveryError(logger, "propagate startup sandbox terminations", terminationErr)
	}
	items, err := sandboxManager.List()
	if err != nil {
		return fmt.Errorf("inspect reconciled runtime groups: %w", err)
	}
	healthySandboxes, err := propagateReconciledFailures(items, serviceManager, jobManager)
	if err != nil {
		logRuntimeRecoveryError(logger, "propagate reconciled runtime failures", err)
	}
	serviceRecords, err := serviceManager.List()
	if err != nil {
		logRuntimeRecoveryError(logger, "inspect persisted services before port restore", err)
		serviceRecords = nil
	}
	if err := failUnavailableServicePools(serviceRecords, healthySandboxes, serviceManager); err != nil {
		logRuntimeRecoveryError(logger, "fail services with unavailable runtime groups", err)
	}
	if _, err := portManager.RestoreFor(ctx, func(lease ports.Lease) bool {
		return healthySandboxes[lease.SandboxID]
	}); err != nil {
		logRuntimeRecoveryError(logger, "restore host ports", err)
	}
	if err := serviceManager.Restore(ctx); err != nil {
		logRuntimeRecoveryError(logger, "restore services", err)
	}
	return nil
}

func logRuntimeRecoveryError(logger *slog.Logger, operation string, err error) {
	if logger != nil {
		logger.Error("isolated runtime recovery failure", "operation", operation, "error", err)
	}
}

func failUnavailableServicePools(records []executionservices.Record, healthySandboxes map[string]bool, sink unavailableServiceSink) error {
	var joined error
	for _, record := range records {
		if healthySandboxes[record.SandboxID] || (record.State == "STOPPED" && len(record.WorkerIDs) == 0) {
			continue
		}
		reason := fmt.Sprintf("runtime group is unavailable after startup reconciliation: sandbox %s is not healthy", record.SandboxID)
		joined = errors.Join(joined, sink.RetireUnavailable(record.ServiceID, reason))
	}
	return joined
}

func propagateReconciledFailures(items []manager.Inspection, sinks ...runtimeFailureSink) (map[string]bool, error) {
	healthySandboxes := make(map[string]bool)
	var joined error
	for _, item := range items {
		state := item.Status.ObservedState
		if (state == model.StateReady || state == model.StateActive) && item.Status.SupervisorHealthy {
			healthySandboxes[item.Spec.SandboxID] = true
			continue
		}
		reason := item.Status.FailureReason
		if reason == "" {
			reason = fmt.Sprintf("runtime group is unavailable after startup reconciliation: state=%s supervisor_healthy=%t", state, item.Status.SupervisorHealthy)
		}
		for _, sink := range sinks {
			joined = errors.Join(joined, sink.FailGroup(item.Spec.RuntimeGroupID, reason))
		}
	}
	return healthySandboxes, joined
}

type sharedStateDatabase interface {
	Check(context.Context) (database.Status, error)
	MarkUnavailable(error)
}

type sharedSettings interface {
	RefreshGlobal(context.Context) (bool, error)
}

type sharedPackageState interface {
	Refresh(context.Context) error
}

type servicePlaneGate interface {
	SetAvailable(bool, string)
}

func reconcileSharedState(ctx context.Context, db sharedStateDatabase, global sharedSettings, gate servicePlaneGate, packages ...sharedPackageState) error {
	status, err := db.Check(ctx)
	if err != nil {
		gate.SetAvailable(false, "database unavailable")
		return err
	}
	if status.State != database.StateReady {
		err = fmt.Errorf("database state is %s", status.State)
		if status.Error != "" {
			err = errors.New(status.Error)
		}
		gate.SetAvailable(false, "database unavailable")
		return err
	}
	if _, err = global.RefreshGlobal(ctx); err != nil {
		db.MarkUnavailable(err)
		gate.SetAvailable(false, "global settings unavailable")
		return err
	}
	if len(packages) > 0 && packages[0] != nil {
		if err = packages[0].Refresh(ctx); err != nil {
			gate.SetAvailable(false, "package state unavailable")
			return err
		}
	}
	gate.SetAvailable(true, "")
	return nil
}

type packageRevisionFollower interface {
	Poll(context.Context) (workspacepackages.PackageSetUpdate, error)
	Acknowledge(uint64) error
}

type serviceRevisionFollower interface {
	Poll(context.Context) (workspacepackages.ServiceSetUpdate, error)
	Acknowledge(uint64) error
}

type targetedServiceReconciler interface {
	Reconcile(context.Context, string) (webservices.Status, error)
	Retire(context.Context, string) error
}

type commandReindexer interface {
	Reindex(context.Context) (discovery.Report, error)
}

type runtimeSharedState struct {
	packages       packageRevisionFollower
	serviceChanges serviceRevisionFollower
	services       targetedServiceReconciler
	commands       commandReindexer
	topology       interface{ Refresh(context.Context) error }
	now            func() time.Time
	nextTopology   time.Time
}

func (s *runtimeSharedState) Refresh(ctx context.Context) error {
	if s.topology != nil {
		now := time.Now()
		if s.now != nil {
			now = s.now()
		}
		if s.nextTopology.IsZero() || !now.Before(s.nextTopology) {
			if err := s.topology.Refresh(ctx); err != nil {
				return err
			}
			s.nextTopology = now.Add(30 * time.Second)
		}
	}
	if err := s.refreshPackages(ctx); err != nil {
		return err
	}
	return s.refreshServices(ctx)
}

func (s *runtimeSharedState) refreshPackages(ctx context.Context) error {
	update, err := s.packages.Poll(ctx)
	if err != nil || update.Revision == 0 {
		return err
	}
	if err := s.apply(ctx, update.ReconcileServices, update.RetireServices); err != nil {
		// Source and database state already converged. Keep the revision pending so
		// the affected services retry, but do not make their local runtime failure
		// look like a shared-state outage for every unrelated service.
		return nil
	}
	if s.commands != nil {
		if _, err := s.commands.Reindex(ctx); err != nil {
			return err
		}
	}
	return s.packages.Acknowledge(update.Revision)
}

func (s *runtimeSharedState) refreshServices(ctx context.Context) error {
	update, err := s.serviceChanges.Poll(ctx)
	if err != nil || update.Revision == 0 {
		return err
	}
	if err := s.apply(ctx, update.ReconcileServices, update.RetireServices); err != nil {
		// Reconciliation records and logs the service-local failure. Leaving the
		// revision unacknowledged makes the next cheap poll retry it idempotently.
		return nil
	}
	return s.serviceChanges.Acknowledge(update.Revision)
}

func (s *runtimeSharedState) apply(ctx context.Context, reconcile, retire []string) error {
	var joined error
	for _, serviceID := range retire {
		joined = errors.Join(joined, s.services.Retire(ctx, serviceID))
	}
	for _, serviceID := range reconcile {
		_, err := s.services.Reconcile(ctx, serviceID)
		joined = errors.Join(joined, err)
	}
	return joined
}

func startRuntimeMonitor(cleanup *runtimeCleanup, sandboxes *manager.Manager, serviceManager *executionservices.Manager, jobManager *jobs.Manager, systemDatabase sharedStateDatabase, global sharedSettings, packages sharedPackageState, publicNetwork servicePlaneGate, interval, timeout time.Duration, logger *slog.Logger) {
	monitorContext, cancel := context.WithCancel(context.Background())
	cleanup.monitorCancel = cancel
	cleanup.monitorWait.Add(1)
	go func() {
		defer cleanup.monitorWait.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-monitorContext.Done():
				return
			case <-ticker.C:
				report, err := sandboxes.CheckHealth(monitorContext, timeout)
				if err != nil {
					if !errors.Is(err, context.Canceled) && logger != nil {
						logger.Error("runtime health check failed", "error", err)
					}
					continue
				}
				for _, failure := range report.Failures {
					propagationErr := errors.Join(serviceManager.FailGroup(failure.RuntimeGroupID, failure.Reason), jobManager.FailGroup(failure.RuntimeGroupID, failure.Reason))
					if logger != nil {
						logger.Error("runtime group failed health monitoring", "runtime_group_id", failure.RuntimeGroupID, "sandbox_id", failure.SandboxID, "oom", failure.OOM, "reason", failure.Reason, "propagation_error", propagationErr)
					}
				}
			}
		}
	}()
	cleanup.monitorWait.Add(1)
	go func() {
		defer cleanup.monitorWait.Done()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-monitorContext.Done():
				return
			case <-ticker.C:
				removed, err := sandboxes.CleanupHistory()
				if err != nil {
					if logger != nil {
						logger.Error("sandbox history cleanup failed", "error", err)
					}
				} else if removed > 0 && logger != nil {
					logger.Info("expired sandbox history cleaned", "sandboxes", removed)
				}
			}
		}
	}()
	cleanup.monitorWait.Add(1)
	go func() {
		defer cleanup.monitorWait.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		available := true
		for {
			select {
			case <-monitorContext.Done():
				return
			case <-ticker.C:
				checkContext, cancel := context.WithTimeout(monitorContext, 5*time.Second)
				err := reconcileSharedState(checkContext, systemDatabase, global, publicNetwork, packages)
				cancel()
				if err != nil && available {
					available = false
					if logger != nil {
						logger.Error("shared database unavailable; public service plane gated", "error", err)
					}
				} else if err == nil && !available {
					available = true
					if logger != nil {
						logger.Info("shared database recovered; public service plane restored")
					}
				}
			}
		}
	}()
}

func runtimeProfile(workload model.WorkloadType, digest string, mounts []model.Mount, resources model.ResourceLimits, egressAllowed bool) model.RuntimeProfile {
	profileMounts := append([]model.Mount(nil), mounts...)
	profileMounts = append(profileMounts, model.Mount{Target: "/tmp", MaximumSize: resources.TmpfsMaximum, Purpose: "temporary", Persistence: "ephemeral"})
	profileMounts = append(profileMounts, model.Mount{Target: "/runtime-cache", MaximumSize: resources.TmpfsMaximum, Purpose: "temporary", Persistence: "ephemeral"})
	dependencyMode := model.DependencyCachedOnly
	if egressAllowed {
		dependencyMode = model.DependencyOnline
	}
	return model.RuntimeProfile{WorkloadType: workload, ImageDigest: digest, DependencyMode: dependencyMode,
		Permissions: model.Permissions{ReadPaths: []string{"/artifacts", "/tmp", "/runtime-cache"}, WritePaths: []string{"/tmp", "/runtime-cache"}}, Mounts: profileMounts,
		NetworkMode: "netstack", EgressAllowed: egressAllowed, ResourceClass: resourceClass(workload, resources),
	}
}

func resourceClass(workload model.WorkloadType, resources model.ResourceLimits) string {
	value := fmt.Sprintf("%s:%d:%d", workload, resources.PIDMaximum, resources.TmpfsMaximum)
	sum := sha256.Sum256([]byte(value))
	return string(workload) + ":sha256:" + hex.EncodeToString(sum[:])
}

func resourceLimits(manager *settings.Manager, workload string) model.ResourceLimits {
	prefix := "sandbox.resources." + workload + "."
	return model.ResourceLimits{
		PIDMaximum: int64(activeInt(manager, prefix+"pid_maximum", 256)), TmpfsMaximum: activeBytes(manager, prefix+"tmpfs_maximum", 128_000_000),
	}
}

func runtimeNodeLimits(settingManager *settings.Manager, runtimeRoot string) manager.NodeLimits {
	temporaryStorage := activeBytes(settingManager, "runtime.node.temporary_storage_budget", 0)
	if temporaryStorage <= 0 {
		temporaryStorage = 1
		var statistics unix.Statfs_t
		if unix.Statfs(runtimeRoot, &statistics) == nil {
			temporaryStorage = clampUint64(statistics.Bavail * uint64(statistics.Bsize))
		}
	}
	return manager.NodeLimits{
		TemporaryStorageBytes: temporaryStorage,
		MaximumSandboxes:      activeInt(settingManager, "runtime.node.maximum_sandboxes", 64),
		MaximumWorkers:        activeInt(settingManager, "runtime.node.maximum_workers", 1024),
	}
}

func clampUint64(value uint64) int64 {
	const maximum = uint64(^uint64(0) >> 1)
	if value > maximum {
		return int64(maximum)
	}
	return int64(value)
}

func grouping(manager *settings.Manager, key string) model.GroupingStrategy {
	return model.GroupingStrategy(activeString(manager, key, string(model.GroupingOwner)))
}
func activeString(manager *settings.Manager, key, fallback string) string {
	if value, ok := manager.Active(key); ok {
		if typed, valid := value.(string); valid {
			return typed
		}
	}
	return fallback
}
func activeInt(manager *settings.Manager, key string, fallback int) int {
	if value, ok := manager.Active(key); ok {
		if typed, valid := value.(int64); valid {
			return int(typed)
		}
	}
	return fallback
}
func activeBytes(manager *settings.Manager, key string, fallback int64) int64 {
	if value, ok := manager.Active(key); ok {
		if typed, valid := value.(settings.ByteSize); valid {
			return int64(typed)
		}
	}
	return fallback
}
func activeBool(manager *settings.Manager, key string) (bool, bool) {
	value, ok := manager.Active(key)
	if !ok {
		return false, false
	}
	typed, valid := value.(bool)
	return typed, valid
}
func activeBoolDefault(manager *settings.Manager, key string, fallback bool) bool {
	if value, ok := activeBool(manager, key); ok {
		return value
	}
	return fallback
}
func activeDuration(manager *settings.Manager, key string, fallback time.Duration) time.Duration {
	value := activeInt(manager, key, 0)
	if value <= 0 {
		return fallback
	}
	return time.Duration(value) * time.Millisecond
}
