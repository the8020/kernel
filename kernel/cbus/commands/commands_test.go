package commands_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	debugclose "the8020/kernel/cbus/commands/debug/close"
	debugopen "the8020/kernel/cbus/commands/debug/open"
	debugtargets "the8020/kernel/cbus/commands/debug/targets"
	jobcancel "the8020/kernel/cbus/commands/job/cancel"
	jobinspect "the8020/kernel/cbus/commands/job/inspect"
	joblist "the8020/kernel/cbus/commands/job/list"
	jobrun "the8020/kernel/cbus/commands/job/run"
	packageinspect "the8020/kernel/cbus/commands/package/inspect"
	packagelist "the8020/kernel/cbus/commands/package/list"
	poolresize "the8020/kernel/cbus/commands/pool/resize"
	poolstatus "the8020/kernel/cbus/commands/pool/status"
	portclose "the8020/kernel/cbus/commands/port/close"
	portexpose "the8020/kernel/cbus/commands/port/expose"
	portlist "the8020/kernel/cbus/commands/port/list"
	runtimedoctor "the8020/kernel/cbus/commands/runtime/doctor"
	runtimeeval "the8020/kernel/cbus/commands/runtime/eval"
	runtimeimagestatus "the8020/kernel/cbus/commands/runtime/image/status"
	runtimerun "the8020/kernel/cbus/commands/runtime/run"
	runtimestatus "the8020/kernel/cbus/commands/runtime/status"
	sandboxdelete "the8020/kernel/cbus/commands/sandbox/delete"
	sandboxhistoryinspect "the8020/kernel/cbus/commands/sandbox/history/inspect"
	sandboxhistorylist "the8020/kernel/cbus/commands/sandbox/history/list"
	sandboxinspect "the8020/kernel/cbus/commands/sandbox/inspect"
	sandboxkill "the8020/kernel/cbus/commands/sandbox/kill"
	sandboxlist "the8020/kernel/cbus/commands/sandbox/list"
	sandboxmetrics "the8020/kernel/cbus/commands/sandbox/metrics"
	sandboxstop "the8020/kernel/cbus/commands/sandbox/stop"
	serviceinspect "the8020/kernel/cbus/commands/service/inspect"
	servicelist "the8020/kernel/cbus/commands/service/list"
	serviceopenapi "the8020/kernel/cbus/commands/service/openapi"
	servicerequest "the8020/kernel/cbus/commands/service/request"
	servicerestart "the8020/kernel/cbus/commands/service/restart"
	servicescale "the8020/kernel/cbus/commands/service/scale"
	servicestart "the8020/kernel/cbus/commands/service/start"
	servicestop "the8020/kernel/cbus/commands/service/stop"
	servicevalidate "the8020/kernel/cbus/commands/service/validate"
	workerinspect "the8020/kernel/cbus/commands/worker/inspect"
	workerkill "the8020/kernel/cbus/commands/worker/kill"
	workerlist "the8020/kernel/cbus/commands/worker/list"
	workerstop "the8020/kernel/cbus/commands/worker/stop"
	"the8020/kernel/cbus/core"
	"the8020/kernel/debugging"
	"the8020/kernel/execution/adminrun"
	"the8020/kernel/execution/groups"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/execution/workers"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/ports"
	runtimehost "the8020/kernel/runtime"
	"the8020/kernel/sandbox/history"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/services"
	"the8020/kernel/webservices"
)

type callRecorder struct {
	calls          map[string]int
	jobOptions     jobs.Options
	adminOptions   adminrun.Options
	scaleOptions   webservices.ScaleOptions
	requestOptions webservices.RequestOptions
}

func (r *callRecorder) record(name string) { r.calls[name]++ }

type fakeSandboxes struct{ *callRecorder }

func (f fakeSandboxes) inspection() manager.Inspection {
	return manager.Inspection{Spec: model.SandboxSpec{
		SandboxID: "sandbox-1", RuntimeGroupID: "group-1", WorkloadType: model.WorkloadJob,
		GroupKey: "job:test", Network: model.NetworkConfiguration{SandboxIP: "10.88.0.2"}, InternalPorts: []int{8000, 9229}, Lifecycle: model.LifecyclePolicy{Warm: true},
	}, Status: model.SandboxStatus{DesiredState: model.StateReady, ObservedState: model.StateReady, WorkerCount: 1}}
}
func (f fakeSandboxes) List() ([]manager.Inspection, error) {
	f.record("sandbox.list")
	return []manager.Inspection{f.inspection()}, nil
}
func (f fakeSandboxes) ListHistory(int, string) (history.Page, error) {
	f.record("sandbox.history.list")
	return history.Page{Sandboxes: []history.Summary{{HistoryID: "20260827T130405.123456789Z-sbx-ax9thsl3", SandboxID: "sbx-ax9thsl3", RuntimeGroupID: "group-1", WorkloadType: model.WorkloadJob, State: model.StateFailed, Reason: "heartbeat timeout", ArchivedAt: time.Unix(2, 0), ExpiresAt: time.Unix(3, 0)}}}, nil
}
func (f fakeSandboxes) InspectHistory(historyID string) (history.Inspection, error) {
	f.record("sandbox.history.inspect")
	return history.Inspection{Record: history.Record{HistoryID: historyID, Spec: f.inspection().Spec, Status: model.SandboxStatus{ObservedState: model.StateFailed, FailureReason: "heartbeat timeout"}}, Logs: []history.Log{{Name: "runtime.log", Size: 4, Content: "test"}}}, nil
}
func (f fakeSandboxes) Inspect(context.Context, string) (manager.Inspection, error) {
	f.record("sandbox.inspect")
	return f.inspection(), nil
}
func (f fakeSandboxes) Metrics(string) (model.ResourceMetrics, error) {
	f.record("sandbox.metrics")
	return model.ResourceMetrics{MemoryCurrent: 1024}, nil
}
func (f fakeSandboxes) Stop(context.Context, string) error {
	f.record("sandbox.stop")
	return nil
}
func (f fakeSandboxes) Kill(context.Context, string) error {
	f.record("sandbox.kill")
	return nil
}
func (f fakeSandboxes) Delete(context.Context, string) error {
	f.record("sandbox.delete")
	return nil
}

type fakeWorkers struct{ *callRecorder }

func (f fakeWorkers) recordValue() workers.Record {
	return workers.Record{SandboxID: "sandbox-1", RuntimeGroupID: "group-1", WorkloadType: model.WorkloadJob, Worker: supervisor.WorkerStatus{WorkerID: "worker-1", WorkloadID: "job-1", OwnerID: "owner-1", State: "READY"}}
}
func (f fakeWorkers) List(context.Context, string) ([]workers.Record, error) {
	f.record("worker.list")
	return []workers.Record{f.recordValue()}, nil
}
func (f fakeWorkers) Inspect(context.Context, string) (workers.Record, error) {
	f.record("worker.inspect")
	return f.recordValue(), nil
}
func (f fakeWorkers) Stop(_ context.Context, _ string, immediate bool) error {
	if immediate {
		f.record("worker.kill")
	} else {
		f.record("worker.stop")
	}
	return nil
}

type fakePackages struct{ *callRecorder }

func (f fakePackages) ListPackages() ([]workspacepackages.Package, error) {
	f.record("package.list")
	return []workspacepackages.Package{{ID: "the8020/demo", Description: "Example package", Valid: true, ServiceCount: 1}}, nil
}

func (f fakePackages) InspectPackage(string) (workspacepackages.Package, error) {
	f.record("package.inspect")
	return workspacepackages.Package{ID: "the8020/demo", Valid: true, ServiceCount: 1}, nil
}

type fakeWebServices struct{ *callRecorder }

func (f fakeWebServices) status() webservices.Status {
	return webservices.Status{ServiceID: "the8020/demo/http", Description: "Example service", CanonicalBasePath: "/the8020/demo/http", Enabled: true, DesiredVersion: 1, LoadedVersion: 1, VersionCount: 1, State: webservices.StateReady, SandboxCount: 1, WorkerCount: 1, Sandboxes: []webservices.ServiceSandboxStatus{{Version: 1, SandboxID: "sandbox-1", RuntimeGroupID: "group-1"}}}
}

func (f fakeWebServices) Start(context.Context, string) (webservices.Status, error) {
	f.record("service.start")
	return f.status(), nil
}

func (f fakeWebServices) Stop(context.Context, string) (webservices.Status, error) {
	f.record("service.stop")
	status := f.status()
	status.State = webservices.StateStopped
	return status, nil
}

func (f fakeWebServices) Restart(context.Context, string) (webservices.Status, error) {
	f.record("service.restart")
	return f.status(), nil
}

func (f fakeWebServices) Reload(context.Context, string) (webservices.Status, error) {
	f.record("service.reload")
	return f.status(), nil
}

func (f fakeWebServices) Retire(context.Context, string) error {
	f.record("service.retire")
	return nil
}

func (f fakeWebServices) Scale(_ context.Context, _ string, options webservices.ScaleOptions) (webservices.Status, error) {
	f.record("service.scale")
	f.scaleOptions = options
	return f.status(), nil
}

func (f fakeWebServices) List() ([]webservices.Status, error) {
	f.record("service.list")
	return []webservices.Status{f.status()}, nil
}

func (f fakeWebServices) Inspect(string) (webservices.Status, error) {
	f.record("service.inspect")
	return f.status(), nil
}

func (f fakeWebServices) Validate(context.Context, string) webservices.ValidationResult {
	f.record("service.validate")
	return webservices.ValidationResult{ServiceID: "the8020/demo/http", Valid: true, OpenAPI: map[string]any{"openapi": "3.1.0"}}
}

func (f fakeWebServices) Request(_ context.Context, _, _, _ string, options webservices.RequestOptions) (webservices.RequestResult, error) {
	f.record("service.request")
	f.requestOptions = options
	return webservices.RequestResult{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"application/json"}}, Body: `{"ok":true}`}, nil
}

func (f fakeWebServices) OpenAPI(context.Context, string) (map[string]any, error) {
	f.record("service.openapi")
	return map[string]any{"openapi": "3.1.0"}, nil
}

type invalidWebServices struct{ fakeWebServices }

func (f invalidWebServices) Validate(context.Context, string) webservices.ValidationResult {
	f.record("service.validate")
	return webservices.ValidationResult{ServiceID: "the8020/demo/broken", Error: "entrypoint failed type checking"}
}

type fakeJobs struct{ *callRecorder }

func (f fakeJobs) recordValue() jobs.Record {
	return jobs.Record{ExecutionID: "execution-1", JobID: "job-1", OwnerID: "owner-1", WorkerID: "worker-1", State: "SUCCEEDED", Result: map[string]any{"ok": true}, Duration: 1250 * time.Millisecond}
}
func (f fakeJobs) Run(_ context.Context, _, _ string, options jobs.Options) (jobs.Record, error) {
	f.record("job.run")
	f.jobOptions = options
	return f.recordValue(), nil
}
func (f fakeJobs) List() ([]jobs.Record, error) {
	f.record("job.list")
	return []jobs.Record{f.recordValue()}, nil
}
func (f fakeJobs) Inspect(string) (jobs.Record, error) {
	f.record("job.inspect")
	return f.recordValue(), nil
}
func (f fakeJobs) Cancel(context.Context, string) error {
	f.record("job.cancel")
	return nil
}

type fakePorts struct{ *callRecorder }

func (f fakePorts) recordValue() ports.Lease {
	return ports.Lease{LeaseID: "lease-1", SandboxID: "sandbox-1", OwnerID: "group-1", SandboxIP: "10.88.0.2", BindAddress: "127.0.0.1", HostPort: 18000, InternalPort: 8000, Protocol: "tcp", Purpose: "administrative", State: "ACTIVE"}
}
func (f fakePorts) Expose(context.Context, ports.Request) (ports.Lease, error) {
	f.record("port.expose")
	return f.recordValue(), nil
}
func (f fakePorts) List() []ports.Lease {
	f.record("port.list")
	return []ports.Lease{f.recordValue()}
}
func (f fakePorts) Close(string) error {
	f.record("port.close")
	return nil
}

type fakeDebugging struct{ *callRecorder }

func (f fakeDebugging) targetValue() debugging.Target {
	return debugging.Target{ID: "target-1", Type: "node", Title: "job:owner:execution:worker", ExecutionID: "execution-1"}
}
func (f fakeDebugging) Targets(context.Context, model.SandboxSpec) ([]debugging.Target, error) {
	f.record("debug.targets")
	return []debugging.Target{f.targetValue()}, nil
}
func (f fakeDebugging) Open(context.Context, model.SandboxSpec, time.Duration) (debugging.Lease, error) {
	f.record("debug.open")
	return debugging.Lease{PortLease: ports.Lease{LeaseID: "debug-lease-1", BindAddress: "127.0.0.1", HostPort: 19229}, AccessToken: "test-token", Targets: []debugging.Target{f.targetValue()}}, nil
}
func (f fakeDebugging) Close(string) error {
	f.record("debug.close")
	return nil
}

type fakePool struct{ *callRecorder }

func (f fakePool) Resize(string, int) error {
	f.record("pool.resize")
	return nil
}
func (f fakePool) Status() []groups.PoolStatus {
	f.record("pool.status")
	return []groups.PoolStatus{{ProfileHash: "sha256:test", Desired: 1, Ready: 1}}
}
func (f fakePool) Forget(string) error {
	f.record("pool.forget")
	return nil
}

type fakeAdminRun struct{ *callRecorder }

func (f fakeAdminRun) result() adminrun.Result {
	return adminrun.Result{
		ArtifactID: "artifact-1",
		Entrypoint: "file:///runtime/artifact-1/main.ts",
		Execution: jobs.Record{
			ExecutionID: "execution-1",
			State:       "SUCCEEDED",
			Result:      map[string]any{"ok": true},
			Duration:    1250 * time.Millisecond,
		},
		Resources: &model.ResourceMetrics{CPUUsageMicros: 12345678901, MemoryCurrent: 4096},
	}
}
func (f fakeAdminRun) Eval(_ context.Context, _ string, options adminrun.Options) (adminrun.Result, error) {
	f.record("runtime.eval")
	f.adminOptions = options
	return f.result(), nil
}
func (f fakeAdminRun) Run(_ context.Context, _ string, options adminrun.Options) (adminrun.Result, error) {
	f.record("runtime.run")
	f.adminOptions = options
	return f.result(), nil
}

func TestEveryPhase1BHandlerSurvivesDegradedRuntime(t *testing.T) {
	serviceSet := &services.Services{Runtime: &services.RuntimeServices{Failure: "runtime host is not ready"}}
	handlers := map[string]core.Handler{
		"debug.close": debugclose.New(serviceSet), "debug.open": debugopen.New(serviceSet), "debug.targets": debugtargets.New(serviceSet),
		"job.cancel": jobcancel.New(serviceSet), "job.inspect": jobinspect.New(serviceSet), "job.list": joblist.New(serviceSet), "job.run": jobrun.New(serviceSet),
		"pool.resize": poolresize.New(serviceSet), "pool.status": poolstatus.New(serviceSet),
		"port.close": portclose.New(serviceSet), "port.expose": portexpose.New(serviceSet), "port.list": portlist.New(serviceSet),
		"runtime.eval": runtimeeval.New(serviceSet), "runtime.run": runtimerun.New(serviceSet),
		"sandbox.delete": sandboxdelete.New(serviceSet), "sandbox.inspect": sandboxinspect.New(serviceSet), "sandbox.kill": sandboxkill.New(serviceSet), "sandbox.list": sandboxlist.New(serviceSet), "sandbox.metrics": sandboxmetrics.New(serviceSet), "sandbox.stop": sandboxstop.New(serviceSet),
		"sandbox.history.list": sandboxhistorylist.New(serviceSet), "sandbox.history.inspect": sandboxhistoryinspect.New(serviceSet),
		"service.inspect": serviceinspect.New(serviceSet), "service.list": servicelist.New(serviceSet), "service.openapi": serviceopenapi.New(serviceSet), "service.request": servicerequest.New(serviceSet), "service.restart": servicerestart.New(serviceSet), "service.scale": servicescale.New(serviceSet), "service.start": servicestart.New(serviceSet), "service.stop": servicestop.New(serviceSet), "service.validate": servicevalidate.New(serviceSet),
		"worker.inspect": workerinspect.New(serviceSet), "worker.kill": workerkill.New(serviceSet), "worker.list": workerlist.New(serviceSet), "worker.stop": workerstop.New(serviceSet),
	}
	if len(handlers) != 35 {
		t.Fatalf("degraded handler count = %d, want 35", len(handlers))
	}
	for id, handler := range handlers {
		t.Run(id, func(t *testing.T) {
			_, err := handler(context.Background(), core.Request{Arguments: map[string]any{}})
			var commandError *core.Error
			if !errors.As(err, &commandError) || commandError.Code != core.CodeRuntimeUnavailable {
				t.Fatalf("error = %#v, want runtime_unavailable", err)
			}
		})
	}
}

func TestDiagnosticHandlersRemainAvailableWhenLifecycleFailed(t *testing.T) {
	doctor := runtimehost.NewDoctor(runtimehost.DoctorConfig{OperatingSystem: "unsupported", Architecture: "unsupported", EffectiveUID: 1})
	serviceSet := &services.Services{Runtime: &services.RuntimeServices{Failure: "runtime host is not ready", Doctor: doctor}}
	for id, handler := range map[string]core.Handler{
		"runtime.doctor":       runtimedoctor.New(serviceSet),
		"runtime.image.status": runtimeimagestatus.New(serviceSet),
		"runtime.status":       runtimestatus.New(serviceSet),
	} {
		t.Run(id, func(t *testing.T) {
			result, err := handler(context.Background(), core.Request{})
			if err != nil || result == nil {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}

func TestEveryPhase1BHandlerSuccessfulPath(t *testing.T) {
	recorder := &callRecorder{calls: map[string]int{}}
	doctor := runtimehost.NewDoctor(runtimehost.DoctorConfig{Root: t.TempDir(), OperatingSystem: "unsupported", Architecture: "unsupported", EffectiveUID: 1})
	serviceSet := &services.Services{Packages: fakePackages{recorder}, Runtime: &services.RuntimeServices{
		Doctor: doctor, Sandboxes: fakeSandboxes{recorder}, Workers: fakeWorkers{recorder},
		Services: fakeWebServices{recorder}, Jobs: fakeJobs{recorder}, Ports: fakePorts{recorder}, Debugging: fakeDebugging{recorder},
		Pool: fakePool{recorder}, AdminRun: fakeAdminRun{recorder},
	}}
	type handlerCase struct {
		handler   core.Handler
		arguments map[string]any
		wantCall  string
	}
	cases := map[string]handlerCase{
		"package.list":         {handler: packagelist.New(serviceSet), wantCall: "package.list"},
		"package.inspect":      {handler: packageinspect.New(serviceSet), wantCall: "package.inspect", arguments: map[string]any{"package_id": "the8020/demo"}},
		"runtime.doctor":       {handler: runtimedoctor.New(serviceSet)},
		"runtime.image.status": {handler: runtimeimagestatus.New(serviceSet)},
		"runtime.status":       {handler: runtimestatus.New(serviceSet), wantCall: "sandbox.list"},
		"runtime.eval": {handler: runtimeeval.New(serviceSet), wantCall: "runtime.eval", arguments: map[string]any{
			"code": "export default () => ({ ok: true })", "workload_type": "job", "owner_id": "owner-1", "group_key": "group-key", "namespace": "tests",
			"timeout": int64(1000), "detached": false, "input": `{"value":1}`, "read": "/data", "write": "/tmp", "network": "example.com:443",
			"imports": "deno.land", "environment": "MODE", "system_info": true, "workspace": "development", "workspace_write": true,
		}},
		"runtime.run": {handler: runtimerun.New(serviceSet), wantCall: "runtime.run", arguments: map[string]any{
			"path": "/tmp/module.ts", "workload_type": "job", "owner_id": "owner-1", "timeout": int64(1000), "input": `[1,2,3]`, "workspace": "development", "workspace_write": true,
		}},
		"sandbox.list":            {handler: sandboxlist.New(serviceSet), wantCall: "sandbox.list"},
		"sandbox.history.list":    {handler: sandboxhistorylist.New(serviceSet), wantCall: "sandbox.history.list", arguments: map[string]any{"limit": int64(100)}},
		"sandbox.history.inspect": {handler: sandboxhistoryinspect.New(serviceSet), wantCall: "sandbox.history.inspect", arguments: map[string]any{"history_id": "20260827T130405.123456789Z-sbx-ax9thsl3"}},
		"sandbox.inspect":         {handler: sandboxinspect.New(serviceSet), wantCall: "sandbox.inspect", arguments: map[string]any{"sandbox_id": "sandbox-1"}},
		"sandbox.metrics":         {handler: sandboxmetrics.New(serviceSet), wantCall: "sandbox.metrics", arguments: map[string]any{"sandbox_id": "sandbox-1"}},
		"sandbox.stop":            {handler: sandboxstop.New(serviceSet), wantCall: "sandbox.stop", arguments: map[string]any{"sandbox_id": "sandbox-1"}},
		"sandbox.kill":            {handler: sandboxkill.New(serviceSet), wantCall: "sandbox.kill", arguments: map[string]any{"sandbox_id": "sandbox-1"}},
		"sandbox.delete":          {handler: sandboxdelete.New(serviceSet), wantCall: "sandbox.delete", arguments: map[string]any{"sandbox_id": "sandbox-1"}},
		"worker.list":             {handler: workerlist.New(serviceSet), wantCall: "worker.list", arguments: map[string]any{"sandbox_id": "sandbox-1"}},
		"worker.inspect":          {handler: workerinspect.New(serviceSet), wantCall: "worker.inspect", arguments: map[string]any{"worker_id": "worker-1"}},
		"worker.stop":             {handler: workerstop.New(serviceSet), wantCall: "worker.stop", arguments: map[string]any{"worker_id": "worker-1"}},
		"worker.kill":             {handler: workerkill.New(serviceSet), wantCall: "worker.kill", arguments: map[string]any{"worker_id": "worker-1"}},
		"service.start":           {handler: servicestart.New(serviceSet), wantCall: "service.start", arguments: map[string]any{"service_id": "the8020/demo/http"}},
		"service.list":            {handler: servicelist.New(serviceSet), wantCall: "service.list"},
		"service.inspect":         {handler: serviceinspect.New(serviceSet), wantCall: "service.inspect", arguments: map[string]any{"service_id": "the8020/demo/http"}},
		"service.validate":        {handler: servicevalidate.New(serviceSet), wantCall: "service.validate", arguments: map[string]any{"service_id": "the8020/demo/http"}},
		"service.openapi":         {handler: serviceopenapi.New(serviceSet), wantCall: "service.openapi", arguments: map[string]any{"service_id": "the8020/demo/http"}},
		"service.request": {handler: servicerequest.New(serviceSet), wantCall: "service.request", arguments: map[string]any{
			"service_id": "the8020/demo/http", "method": "POST", "relative_path": "/echo", "headers": `{"X-Test":"yes"}`, "json": `{"value":42}`, "timeout": int64(1000),
		}},
		"service.restart": {handler: servicerestart.New(serviceSet), wantCall: "service.restart", arguments: map[string]any{"service_id": "the8020/demo/http"}},
		"service.scale": {handler: servicescale.New(serviceSet), wantCall: "service.scale", arguments: map[string]any{
			"service_id": "the8020/demo/http", "minimum_workers": int64(2), "maximum_workers": int64(8), "concurrency_per_worker": int64(8), "target_utilization": "0.65", "worker_keep_alive": "3m", "workers_per_sandbox": int64(4), "sandbox_group": "shared", "minimum_sandboxes": int64(1), "service_type": "session", "session_keep_alive": "10m",
		}},
		"service.stop": {handler: servicestop.New(serviceSet), wantCall: "service.stop", arguments: map[string]any{"service_id": "the8020/demo/http"}},
		"job.run": {handler: jobrun.New(serviceSet), wantCall: "job.run", arguments: map[string]any{
			"job_id": "job-1", "entrypoint": "file:///artifacts/job.ts", "input": `{"task":1}`, "detached": false, "group_key": "group-key", "namespace": "tests",
			"timeout": int64(1000), "parallelism": int64(2), "reuse": true, "workspace": "development", "workspace_write": true,
		}},
		"job.list":    {handler: joblist.New(serviceSet), wantCall: "job.list"},
		"job.inspect": {handler: jobinspect.New(serviceSet), wantCall: "job.inspect", arguments: map[string]any{"execution_id": "execution-1"}},
		"job.cancel":  {handler: jobcancel.New(serviceSet), wantCall: "job.cancel", arguments: map[string]any{"execution_id": "execution-1"}},
		"port.list":   {handler: portlist.New(serviceSet), wantCall: "port.list"},
		"port.expose": {handler: portexpose.New(serviceSet), wantCall: "port.expose", arguments: map[string]any{
			"sandbox_id": "sandbox-1", "internal_port": int64(8000), "bind_address": "127.0.0.1", "host_port": int64(0), "protocol": "tcp", "purpose": "administrative", "expiration": int64(60),
		}},
		"port.close":    {handler: portclose.New(serviceSet), wantCall: "port.close", arguments: map[string]any{"lease_id": "lease-1"}},
		"debug.targets": {handler: debugtargets.New(serviceSet), wantCall: "debug.targets", arguments: map[string]any{"sandbox_id": "sandbox-1"}},
		"debug.open":    {handler: debugopen.New(serviceSet), wantCall: "debug.open", arguments: map[string]any{"sandbox_id": "sandbox-1", "duration": int64(60)}},
		"debug.close":   {handler: debugclose.New(serviceSet), wantCall: "debug.close", arguments: map[string]any{"lease_id": "debug-lease-1"}},
		"pool.status":   {handler: poolstatus.New(serviceSet), wantCall: "pool.status"},
		"pool.resize":   {handler: poolresize.New(serviceSet), wantCall: "pool.resize", arguments: map[string]any{"profile": "sha256:test", "count": int64(2)}},
	}
	if len(cases) != 40 {
		t.Fatalf("successful Phase 1D handler count = %d, want 40", len(cases))
	}
	for id, testCase := range cases {
		t.Run(id, func(t *testing.T) {
			before := recorder.calls[testCase.wantCall]
			result, err := testCase.handler(context.Background(), core.Request{Arguments: testCase.arguments})
			if err != nil {
				t.Fatalf("handler returned error: %v", err)
			}
			if len(result) == 0 {
				t.Fatal("handler returned an empty result")
			}
			if testCase.wantCall != "" && recorder.calls[testCase.wantCall] != before+1 {
				t.Fatalf("%s call count = %d, want %d", testCase.wantCall, recorder.calls[testCase.wantCall], before+1)
			}
		})
	}
	if recorder.jobOptions.Workspace != "development" || !recorder.jobOptions.WorkspaceWritable || recorder.adminOptions.Workspace != "development" || !recorder.adminOptions.WorkspaceWritable {
		t.Fatalf("workspace options were not propagated: job=%#v admin=%#v", recorder.jobOptions, recorder.adminOptions)
	}
	if recorder.scaleOptions.MinimumWorkers == nil || *recorder.scaleOptions.MinimumWorkers != 2 || recorder.scaleOptions.MaximumWorkers == nil || *recorder.scaleOptions.MaximumWorkers != 8 || recorder.scaleOptions.WorkersPerSandbox == nil || *recorder.scaleOptions.WorkersPerSandbox != 4 || recorder.scaleOptions.ConcurrencyPerWorker == nil || *recorder.scaleOptions.ConcurrencyPerWorker != 8 || recorder.scaleOptions.TargetUtilization == nil || *recorder.scaleOptions.TargetUtilization != 0.65 || recorder.scaleOptions.SandboxGroup == nil || *recorder.scaleOptions.SandboxGroup != "shared" || recorder.scaleOptions.WorkerKeepAlive == nil || *recorder.scaleOptions.WorkerKeepAlive != "3m" || recorder.scaleOptions.ServiceType == nil || *recorder.scaleOptions.ServiceType != "session" || recorder.scaleOptions.SessionKeepAlive == nil || *recorder.scaleOptions.SessionKeepAlive != "10m" {
		t.Fatalf("service scale options were not propagated: %#v", recorder.scaleOptions)
	}
	if recorder.requestOptions.Headers.Get("X-Test") != "yes" || recorder.requestOptions.Headers.Get("Content-Type") != "application/json" || recorder.requestOptions.Timeout != time.Second {
		t.Fatalf("service request options were not propagated: %#v", recorder.requestOptions)
	}
}

func TestResourceListHandlersExposeOnlyReadableSummaryFields(t *testing.T) {
	recorder := &callRecorder{calls: map[string]int{}}
	serviceSet := &services.Services{Packages: fakePackages{recorder}, Runtime: &services.RuntimeServices{
		Sandboxes: fakeSandboxes{recorder}, Workers: fakeWorkers{recorder},
		Services: fakeWebServices{recorder}, Jobs: fakeJobs{recorder}, Ports: fakePorts{recorder},
		Debugging: fakeDebugging{recorder}, Pool: fakePool{recorder},
	}}
	tests := []struct {
		name       string
		collection string
		handler    core.Handler
		arguments  map[string]any
		fields     []string
	}{
		{name: "sandboxes", collection: "sandboxes", handler: sandboxlist.New(serviceSet), fields: []string{"sandbox_id", "workload_type", "state", "worker_count", "warm", "runtime_group_id", "reason"}},
		{name: "sandbox history", collection: "sandboxes", handler: sandboxhistorylist.New(serviceSet), fields: []string{"history_id", "sandbox_id", "runtime_group_id", "workload_type", "state", "reason", "archived_at", "expires_at", "log_files", "log_bytes"}},
		{name: "workers", collection: "workers", handler: workerlist.New(serviceSet), fields: []string{"worker_id", "workload_type", "state", "workload_id", "owner_id", "sandbox_id", "in_flight"}},
		{name: "jobs", collection: "executions", handler: joblist.New(serviceSet), fields: []string{"execution_id", "job_id", "state", "owner_id", "detached", "duration"}},
		{name: "packages", collection: "packages", handler: packagelist.New(serviceSet), fields: []string{"package_id", "description", "valid", "service_count"}},
		{name: "services", collection: "services", handler: servicelist.New(serviceSet), fields: []string{"service_id", "description", "canonical_base_path", "state", "enabled", "version_count", "sandbox_count", "worker_count", "service_type", "access_mode"}},
		{name: "ports", collection: "ports", handler: portlist.New(serviceSet), fields: []string{"lease_id", "protocol", "state", "bind_address", "host_port", "sandbox_id", "internal_port", "purpose"}},
		{name: "debug targets", collection: "targets", handler: debugtargets.New(serviceSet), arguments: map[string]any{"sandbox_id": "sandbox-1"}, fields: []string{"id", "type", "title", "execution_id"}},
		{name: "warm pools", collection: "profiles", handler: poolstatus.New(serviceSet), fields: []string{"profile_hash", "desired_warm_count", "ready_warm_count", "creating_count", "reserved_count", "assigned_count", "failed_count", "replenish_count"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := test.handler(context.Background(), core.Request{Arguments: test.arguments})
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(result[test.collection])
			if err != nil {
				t.Fatal(err)
			}
			var items []map[string]any
			if err := json.Unmarshal(data, &items); err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || len(items[0]) != len(test.fields) {
				t.Fatalf("summary fields = %#v, want exactly %v", items, test.fields)
			}
			for _, field := range test.fields {
				if _, exists := items[0][field]; !exists {
					t.Fatalf("summary missing %q: %#v", field, items[0])
				}
			}
		})
	}
	detail, err := sandboxinspect.New(serviceSet)(context.Background(), core.Request{Arguments: map[string]any{"sandbox_id": "sandbox-1"}})
	if err != nil {
		t.Fatal(err)
	}
	detailData, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	var readable struct {
		Reason   string           `json:"reason"`
		Services []map[string]any `json:"services"`
	}
	if err := json.Unmarshal(detailData, &readable); err != nil {
		t.Fatal(err)
	}
	if readable.Reason != "warm pool" || len(readable.Services) != 1 {
		t.Fatalf("sandbox relationships = %#v", readable)
	}
	if _, exists := detail["sessions"]; exists {
		t.Fatalf("sandbox inspection exposed service-owned sessions: %#v", detail)
	}

	status, err := runtimestatus.New(serviceSet)(context.Background(), core.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"sandboxes", "ports", "pool"} {
		if _, exists := status[forbidden]; exists {
			t.Fatalf("runtime status exposed %s records: %#v", forbidden, status)
		}
	}
	for field, want := range map[string]int{
		"sandbox_count": 1, "worker_count": 1, "port_count": 1,
		"warm_pool_profile_count": 1, "warm_pool_desired_count": 1, "warm_pool_ready_count": 1, "warm_pool_failed_count": 0,
	} {
		if status[field] != want {
			t.Fatalf("runtime status %s = %#v, want %d", field, status[field], want)
		}
	}
}

func TestAdministrativeExecutionHandlersUseConciseDefaultAndExplicitDetail(t *testing.T) {
	recorder := &callRecorder{calls: map[string]int{}}
	serviceSet := &services.Services{Runtime: &services.RuntimeServices{AdminRun: fakeAdminRun{recorder}}}

	concise, err := runtimeeval.New(serviceSet)(context.Background(), core.Request{Arguments: map[string]any{"code": "export default 1"}})
	if err != nil {
		t.Fatal(err)
	}
	if concise["state"] != "SUCCEEDED" || concise["duration"] != "1.25s" {
		t.Fatalf("concise result = %#v", concise)
	}
	for _, internal := range []string{"execution", "execution_id", "resources"} {
		if _, exists := concise[internal]; exists {
			t.Fatalf("concise result exposed %s: %#v", internal, concise)
		}
	}
	if result, ok := concise["result"].(map[string]any); !ok || result["ok"] != true {
		t.Fatalf("concise execution value = %#v", concise["result"])
	}

	detailed, err := runtimerun.New(serviceSet)(context.Background(), core.Request{Arguments: map[string]any{"path": "example.ts", "detail": true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(detailed) != 1 || detailed["execution"] == nil {
		t.Fatalf("detailed result = %#v", detailed)
	}
}

func TestServiceLifecycleHandlersUseConciseDefaultAndExplicitDetail(t *testing.T) {
	recorder := &callRecorder{calls: map[string]int{}}
	serviceSet := &services.Services{Runtime: &services.RuntimeServices{Services: fakeWebServices{recorder}}}

	concise, err := servicestart.New(serviceSet)(context.Background(), core.Request{Arguments: map[string]any{"service_id": "the8020/demo/variables"}})
	if err != nil {
		t.Fatal(err)
	}
	if concise["state"] != webservices.StateReady || concise["service_id"] != "the8020/demo/http" || concise["version_count"] != 1 || concise["sandbox_count"] != 1 {
		t.Fatalf("concise status = %#v", concise)
	}
	if _, exists := concise["service"]; exists {
		t.Fatalf("concise status exposed detail: %#v", concise)
	}

	detailed, err := servicerestart.New(serviceSet)(context.Background(), core.Request{Arguments: map[string]any{"service_id": "the8020/demo/variables", "detail": true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(detailed) != 1 || detailed["service"] == nil {
		t.Fatalf("detailed status = %#v", detailed)
	}
}

func TestServiceValidationAndRequestArgumentFailuresReturnCommandErrors(t *testing.T) {
	recorder := &callRecorder{calls: map[string]int{}}
	serviceSet := &services.Services{Runtime: &services.RuntimeServices{Services: invalidWebServices{fakeWebServices{recorder}}}}
	_, err := servicevalidate.New(serviceSet)(context.Background(), core.Request{Arguments: map[string]any{"service_id": "the8020/demo/broken"}})
	var commandError *core.Error
	if !errors.As(err, &commandError) || commandError.Code != core.CodeRuntimeOperation || !strings.Contains(commandError.Message, "type checking") {
		t.Fatalf("validation error = %#v", err)
	}

	serviceSet.Runtime.Services = fakeWebServices{recorder}
	_, err = servicerequest.New(serviceSet)(context.Background(), core.Request{Arguments: map[string]any{
		"service_id": "the8020/demo/variables", "method": "POST", "relative_path": "/echo", "body": "text", "json": `{"value":42}`,
	}})
	if !errors.As(err, &commandError) || commandError.Code != core.CodeInvalidArguments {
		t.Fatalf("body conflict error = %#v", err)
	}
}
