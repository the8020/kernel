package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
	"the8020/kernel/debugging"
	"the8020/kernel/execution/coordinator"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/execution/pool"
	executionservices "the8020/kernel/execution/services"
	"the8020/kernel/execution/workers"
	"the8020/kernel/runtime/callback"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/settings"
	"the8020/kernel/webservices"
)

var runtimeSettingKeys = []string{
	"runtime.supervisor.heartbeat_interval", "runtime.supervisor.heartbeat_timeout",
	"runtime.node.maximum_sandboxes", "runtime.node.maximum_workers", "runtime.node.temporary_storage_budget",
	"runtime.sandbox.maximum_workers", "runtime.sandbox.keep_alive", "runtime.sandbox.startup_timeout", "runtime.sandbox.stop_grace_period", "runtime.worker.stop_grace_period",
	"sandbox.resources.service.pid_maximum", "sandbox.resources.service.tmpfs_maximum", "sandbox.resources.job.pid_maximum", "sandbox.resources.job.tmpfs_maximum",
	"execution.grouping.job", "execution.grouping.service", "sandbox.warm_pool.size", "sandbox.history.retention", "sandbox.shutdown_policy",
	"sandbox.debug.enabled", "sandbox.debug.bind_address", "sandbox.debug.lease_timeout", "services.reconcile_interval", "services.startup_timeout",
}

// runtimeSettings coordinates existing owners without creating or restarting work.
type runtimeSettings struct {
	sandboxes                  *manager.Manager
	workers                    *workers.Manager
	coordinator                *coordinator.Coordinator
	jobs                       *jobs.Manager
	services                   *executionservices.Manager
	warm                       *pool.Controller
	callback                   *callback.Server
	debugging                  *debugging.Manager
	webservices                *webservices.Manager
	cleanup                    *runtimeCleanup
	backend                    interface{ SetStartupTimeout(time.Duration) }
	runtimeRoot                string
	networkName                string
	serviceProfile, jobProfile model.RuntimeProfile
}

type preparedRuntimeSettings struct {
	owner                                                                *runtimeSettings
	sandbox                                                              manager.Config
	maximumWorkersPerSandbox                                             int
	heartbeatTimeout, workerStopGrace, reconcileInterval, serviceStartup time.Duration
	jobStrategy, serviceStrategy                                         model.GroupingStrategy
	serviceProfile, jobProfile                                           model.RuntimeProfile
	serviceResources, jobResources                                       model.ResourceLimits
	lifecycle                                                            model.LifecyclePolicy
	warmCount                                                            int
	debugEnabled                                                         bool
	debugBind                                                            string
	debugDuration                                                        time.Duration
	shutdown                                                             manager.ShutdownPolicy
}

func (o *runtimeSettings) Prepare(_ context.Context, values settings.Values) (settings.Prepared, error) {
	var invalid error
	integer := func(key string) int64 {
		value, ok := values[key].(int64)
		if !ok || value < 0 || value > int64((1<<63-1)/time.Millisecond) {
			invalid = fmt.Errorf("invalid runtime integer %s", key)
		}
		return value
	}
	milliseconds := func(key string) time.Duration { return time.Duration(integer(key)) * time.Millisecond }
	bytes := func(key string) int64 {
		value, ok := values[key].(settings.ByteSize)
		if !ok || value < 0 {
			invalid = fmt.Errorf("invalid runtime byte size %s", key)
		}
		return int64(value)
	}
	text := func(key string) string {
		value, ok := values[key].(string)
		if !ok {
			invalid = fmt.Errorf("invalid runtime string %s", key)
		}
		return value
	}
	p := &preparedRuntimeSettings{
		owner:                    o,
		maximumWorkersPerSandbox: int(integer("runtime.sandbox.maximum_workers")),
		heartbeatTimeout:         milliseconds("runtime.supervisor.heartbeat_timeout"),
		workerStopGrace:          milliseconds("runtime.worker.stop_grace_period"),
		reconcileInterval:        milliseconds("services.reconcile_interval"), serviceStartup: milliseconds("services.startup_timeout"),
		jobStrategy: model.GroupingStrategy(text("execution.grouping.job")), serviceStrategy: model.GroupingStrategy(text("execution.grouping.service")),
		serviceProfile: o.serviceProfile, jobProfile: o.jobProfile,
		serviceResources: model.ResourceLimits{PIDMaximum: integer("sandbox.resources.service.pid_maximum"), TmpfsMaximum: bytes("sandbox.resources.service.tmpfs_maximum")},
		jobResources:     model.ResourceLimits{PIDMaximum: integer("sandbox.resources.job.pid_maximum"), TmpfsMaximum: bytes("sandbox.resources.job.tmpfs_maximum")},
		warmCount:        int(integer("sandbox.warm_pool.size")),
		debugBind:        text("sandbox.debug.bind_address"), debugDuration: time.Duration(integer("sandbox.debug.lease_timeout")) * time.Second,
		shutdown: manager.ShutdownPolicy(text("sandbox.shutdown_policy")),
	}
	p.sandbox = manager.Config{
		HeartbeatInterval: milliseconds("runtime.supervisor.heartbeat_interval"),
		StartupTimeout:    milliseconds("runtime.sandbox.startup_timeout"), StopGrace: milliseconds("runtime.sandbox.stop_grace_period"),
		KeepAlive: milliseconds("runtime.sandbox.keep_alive"), HistoryRetention: milliseconds("sandbox.history.retention"),
		NodeLimits: manager.NodeLimits{MaximumSandboxes: int(integer("runtime.node.maximum_sandboxes")), MaximumWorkers: int(integer("runtime.node.maximum_workers")), TemporaryStorageBytes: bytes("runtime.node.temporary_storage_budget")},
	}
	var ok bool
	p.debugEnabled, ok = values["sandbox.debug.enabled"].(bool)
	if !ok {
		invalid = errors.New("invalid debug admission policy")
	}
	if invalid != nil {
		return nil, invalid
	}
	if !p.jobStrategy.Valid() || !p.serviceStrategy.Valid() || p.maximumWorkersPerSandbox < 1 || p.sandbox.NodeLimits.MaximumWorkers < 1 || p.sandbox.NodeLimits.MaximumSandboxes < 1 || p.sandbox.StartupTimeout <= 0 || p.sandbox.StopGrace <= 0 || p.sandbox.HistoryRetention <= 0 || p.reconcileInterval <= 0 || p.serviceStartup <= 0 || p.sandbox.HeartbeatInterval < 100*time.Millisecond || p.sandbox.HeartbeatInterval > time.Minute || p.heartbeatTimeout <= p.sandbox.HeartbeatInterval || p.workerStopGrace < 10*time.Millisecond || p.workerStopGrace > time.Minute || p.debugDuration <= 0 || p.debugDuration > time.Hour || (p.shutdown != manager.ShutdownDestroy && p.shutdown != manager.ShutdownLeave) {
		return nil, errors.New("invalid runtime policy bounds")
	}
	if ip := net.ParseIP(p.debugBind); ip == nil || !ip.IsLoopback() {
		return nil, errors.New("debug bind address must be a loopback IP")
	}
	if err := p.serviceResources.Validate(); err != nil {
		return nil, err
	}
	if err := p.jobResources.Validate(); err != nil {
		return nil, err
	}
	if p.sandbox.NodeLimits.TemporaryStorageBytes == 0 {
		var statistics unix.Statfs_t
		if err := unix.Statfs(o.runtimeRoot, &statistics); err != nil {
			return nil, fmt.Errorf("derive temporary-storage budget: %w", err)
		}
		p.sandbox.NodeLimits.TemporaryStorageBytes = max(1, clampUint64(statistics.Bavail*uint64(statistics.Bsize)))
	}
	p.serviceProfile = profileResources(p.serviceProfile, p.serviceResources)
	p.jobProfile = profileResources(p.jobProfile, p.jobResources)
	if _, err := p.serviceProfile.Hash(); err != nil {
		return nil, err
	}
	if _, err := p.jobProfile.Hash(); err != nil {
		return nil, err
	}
	p.lifecycle = model.LifecyclePolicy{DestroyWhenIdle: true, StopGracePeriod: p.sandbox.StopGrace}
	return p, nil
}

func (p *preparedRuntimeSettings) Commit() {
	o := p.owner
	o.sandboxes.SetRuntimePolicy(p.sandbox)
	o.workers.SetLimits(p.sandbox.NodeLimits.MaximumWorkers, p.maximumWorkersPerSandbox)
	o.coordinator.SetMaximumWorkers(p.maximumWorkersPerSandbox)
	o.jobs.SetAllocationPolicy(p.jobStrategy, p.jobProfile, p.jobResources, p.lifecycle)
	o.services.SetAllocationPolicy(p.serviceStrategy, p.serviceProfile, p.serviceResources, p.lifecycle)
	o.callback.SetRuntimePolicy(p.sandbox.HeartbeatInterval, p.workerStopGrace)
	if o.backend != nil {
		o.backend.SetStartupTimeout(p.sandbox.StartupTimeout)
	}
	o.warm.SetTemplates([]pool.Template{
		{Profile: p.serviceProfile, Resources: p.serviceResources, Lifecycle: p.lifecycle, Network: o.networkName},
		{Profile: p.jobProfile, Resources: p.jobResources, Lifecycle: p.lifecycle, Network: o.networkName},
	}, p.warmCount)
	o.debugging.SetPolicy(p.debugEnabled, p.debugBind, p.debugDuration)
	o.webservices.SetRuntimePolicy(p.reconcileInterval, p.serviceStartup)
	o.cleanup.policyMu.Lock()
	o.cleanup.policy, o.cleanup.heartbeatTimeout = p.shutdown, p.heartbeatTimeout
	o.cleanup.policyMu.Unlock()
}
func (p *preparedRuntimeSettings) Discard() {}

// Profiles remain immutable for queued work and existing sandboxes.
func profileResources(profile model.RuntimeProfile, resources model.ResourceLimits) model.RuntimeProfile {
	profile.ResourceClass = resourceClass(profile.WorkloadType, resources)
	profile.Mounts = append([]model.Mount(nil), profile.Mounts...)
	for i := range profile.Mounts {
		if profile.Mounts[i].Purpose == "temporary" {
			profile.Mounts[i].MaximumSize = resources.TmpfsMaximum
		}
	}
	return profile
}
