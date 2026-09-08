package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"the8020/kernel/cbus/core"
	"the8020/kernel/database"
	executionservices "the8020/kernel/execution/services"
	workspacepackages "the8020/kernel/packages"
	runtimehost "the8020/kernel/runtime"
	containerdbackend "the8020/kernel/sandbox/backend/containerd"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/settings"
	"the8020/kernel/webservices"
)

func TestZeroDurationSettingDisablesSandboxRetention(t *testing.T) {
	definitions := []settings.Definition{{Key: "runtime.sandbox.keep_alive", Type: settings.TypeInteger, Storage: settings.StorageNode, Default: int64(120000), Environment: "THE8020_RUNTIME_SANDBOX_KEEP_ALIVE", Description: "Sandbox keepalive"}}
	configured, err := settings.New(definitions, settings.PersistencePaths{Node: filepath.Join(t.TempDir(), "kernel.toml")}, map[string]string{"runtime.sandbox.keep_alive": "0"}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if got := activeDuration(configured, "runtime.sandbox.keep_alive", 2*time.Minute); got != 0 {
		t.Fatalf("explicit zero became the fallback: %s", got)
	}
	if got := activeDuration(configured, "runtime.sandbox.unconfigured", 2*time.Minute); got != 2*time.Minute {
		t.Fatalf("missing setting lost the fallback: %s", got)
	}
}

type sharedStateDatabaseStub struct {
	context context.Context
	status  database.Status
	err     error
	marked  error
}

func (s *sharedStateDatabaseStub) Check(ctx context.Context) (database.Status, error) {
	s.context = ctx
	return s.status, s.err
}
func (s *sharedStateDatabaseStub) MarkUnavailable(err error) { s.marked = err }

type sharedSettingsStub struct {
	calls int
	err   error
}

func (s *sharedSettingsStub) RefreshGlobal(context.Context) (bool, error) {
	s.calls++
	return true, s.err
}

type sharedPackageStateStub struct {
	context context.Context
	calls   int
	err     error
}

func (s *sharedPackageStateStub) Refresh(ctx context.Context) error {
	s.context = ctx
	s.calls++
	return s.err
}

type servicePlaneGateStub struct {
	available bool
	reason    string
}

type packageRevisionFollowerStub struct {
	update workspacepackages.PackageSetUpdate
	acks   []uint64
}

func (s *packageRevisionFollowerStub) Poll(context.Context) (workspacepackages.PackageSetUpdate, error) {
	return s.update, nil
}
func (s *packageRevisionFollowerStub) Acknowledge(revision uint64) error {
	s.acks = append(s.acks, revision)
	s.update = workspacepackages.PackageSetUpdate{}
	return nil
}

type targetedServiceReconcilerStub struct {
	calls []string
	fail  string
}

func (s *targetedServiceReconcilerStub) Reconcile(_ context.Context, serviceID string) (webservices.Status, error) {
	s.calls = append(s.calls, "reconcile:"+serviceID)
	if serviceID == s.fail {
		return webservices.Status{}, errors.New("reconcile failed")
	}
	return webservices.Status{ServiceID: serviceID}, nil
}
func (s *targetedServiceReconcilerStub) Retire(_ context.Context, serviceID string) error {
	s.calls = append(s.calls, "retire:"+serviceID)
	if serviceID == s.fail {
		return errors.New("retire failed")
	}
	return nil
}

func (s *servicePlaneGateStub) SetAvailable(available bool, reason string) {
	s.available, s.reason = available, reason
}

func TestCloseConcurrentlyStartsAllTasksBeforeWaiting(t *testing.T) {
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- closeConcurrently(
			func() error { close(firstStarted); <-release; return nil },
			func() error { close(secondStarted); <-release; return nil },
		)
	}()
	for name, started := range map[string]<-chan struct{}{"first": firstStarted, "second": secondStarted} {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("%s cleanup task did not start concurrently", name)
		}
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent cleanup did not finish")
	}
}

func TestRuntimeCleanupReportsOrderedStages(t *testing.T) {
	var stages []string
	cleanup := &runtimeCleanup{}
	if err := cleanup.Close(context.Background(), func(started bool, _, step, _ string) {
		if started {
			stages = append(stages, step)
		}
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"public HTTP", "runtime controllers", "runtime ports", "runtime sandboxes", "runtime backends"}
	if !reflect.DeepEqual(stages, want) {
		t.Fatalf("stages=%#v want=%#v", stages, want)
	}
}

func TestSharedStateReconciliationGatesFailuresAndRestoresReadyDatabase(t *testing.T) {
	db := &sharedStateDatabaseStub{status: database.Status{State: database.StateReady}}
	settings := &sharedSettingsStub{}
	packages := &sharedPackageStateStub{}
	gate := &servicePlaneGateStub{}
	if err := reconcileSharedState(context.Background(), db, settings, gate, packages); err != nil || !gate.available || settings.calls != 1 || packages.calls != 1 {
		t.Fatalf("ready reconciliation gate=%#v settings=%#v packages=%#v err=%v", gate, settings, packages, err)
	}
	db.err = errors.New("connection lost")
	if err := reconcileSharedState(context.Background(), db, settings, gate, packages); !errors.Is(err, db.err) || gate.available || gate.reason != "database unavailable" || settings.calls != 1 || packages.calls != 1 {
		t.Fatalf("connection failure gate=%#v settings=%#v err=%v", gate, settings, err)
	}
	db.err = nil
	settings.err = errors.New("invalid shared setting")
	if err := reconcileSharedState(context.Background(), db, settings, gate, packages); !errors.Is(err, settings.err) || gate.available || gate.reason != "global settings unavailable" || !errors.Is(db.marked, settings.err) || packages.calls != 1 {
		t.Fatalf("configuration failure gate=%#v marked=%v err=%v", gate, db.marked, err)
	}
	settings.err = nil
	packages.err = errors.New("package checkout unavailable")
	if err := reconcileSharedState(context.Background(), db, settings, gate, packages); !errors.Is(err, packages.err) || gate.available || gate.reason != "package state unavailable" || packages.calls != 2 {
		t.Fatalf("package failure gate=%#v packages=%#v err=%v", gate, packages, err)
	}
	packages.err = nil
	if err := reconcileSharedState(context.Background(), db, settings, gate, packages); err != nil || !gate.available || packages.calls != 3 {
		t.Fatalf("recovery gate=%#v err=%v", gate, err)
	}
}

func TestRuntimeSharedStateReindexesOnlyChangedPackages(t *testing.T) {
	indexes := &packageRevisionFollowerStub{update: workspacepackages.PackageSetUpdate{Revision: 7, Packages: []string{"acme/orders"}}}
	topology := &sharedPackageStateStub{}
	now := time.Unix(1_700_000_000, 0)
	var calls [][]string
	shared := &runtimeSharedState{indexes: indexes, topology: topology, now: func() time.Time { return now }, reindex: func(_ context.Context, ids []string) (core.Result, error) {
		calls = append(calls, ids)
		return core.Result{}, nil
	}}
	if err := shared.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, [][]string{{"acme/orders"}}) || !reflect.DeepEqual(indexes.acks, []uint64{7}) || topology.calls != 1 {
		t.Fatalf("calls=%#v acks=%#v topology=%d", calls, indexes.acks, topology.calls)
	}
	if err := shared.Refresh(context.Background()); err != nil || len(calls) != 1 || topology.calls != 1 {
		t.Fatalf("unchanged refresh rescanned: calls=%#v topology=%d error=%v", calls, topology.calls, err)
	}
	now = now.Add(30 * time.Second)
	indexes.update = workspacepackages.PackageSetUpdate{Revision: 8, Packages: []string{"acme/other"}}
	if err := shared.Refresh(context.Background()); err != nil || topology.calls != 2 || !reflect.DeepEqual(indexes.acks, []uint64{7, 8}) {
		t.Fatalf("refresh=%v acks=%v", err, indexes.acks)
	}
}

func TestSourceUpdatePublishesRestartBeforeReindexAndRetriesWithoutGating(t *testing.T) {
	packages := &packageRevisionFollowerStub{update: workspacepackages.PackageSetUpdate{Revision: 7, Packages: []string{"acme/shared"}, Paths: []string{"/workspace/packages/acme/shared/value.ts"}}}
	indexes := &packageRevisionFollowerStub{}
	var calls []string
	fail := true
	shared := &runtimeSharedState{packages: packages, indexes: indexes,
		sourceUpdate: func(_ context.Context, update workspacepackages.PackageSetUpdate) error {
			calls = append(calls, "scan")
			if update.Revision != 7 || len(update.Paths) != 1 {
				t.Fatalf("source update=%#v", update)
			}
			if fail {
				return errors.New("supervisor temporarily unavailable")
			}
			indexes.update = workspacepackages.PackageSetUpdate{Revision: 3, Restarts: []string{"acme/consumer/api"}}
			return nil
		},
		reindex: func(_ context.Context, ids []string) (core.Result, error) {
			calls = append(calls, "index:"+ids[0])
			return nil, nil
		},
		restart: func(_ context.Context, id string) error { calls = append(calls, "restart:"+id); return nil },
	}
	if err := shared.Refresh(context.Background()); err != nil || len(packages.acks) != 0 {
		t.Fatalf("failed scan gated traffic/acknowledged update: %v %v", err, packages.acks)
	}
	fail = false
	if err := shared.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"scan", "scan", "index:acme/shared", "restart:acme/consumer/api"}) || !reflect.DeepEqual(packages.acks, []uint64{7}) || !reflect.DeepEqual(indexes.acks, []uint64{3}) {
		t.Fatalf("calls=%v package acknowledgements=%v index acknowledgements=%v", calls, packages.acks, indexes.acks)
	}
	if err := shared.Refresh(context.Background()); err != nil || len(calls) != 4 {
		t.Fatalf("idle refresh rescanned imports: %v %v", calls, err)
	}
}

func TestTargetedIndexFailureDoesNotGateHealthyServicesOrRepeatDiscovery(t *testing.T) {
	db := &sharedStateDatabaseStub{status: database.Status{State: database.StateReady}}
	settings := &sharedSettingsStub{}
	gate := &servicePlaneGateStub{}
	indexes := &packageRevisionFollowerStub{update: workspacepackages.PackageSetUpdate{Revision: 9, Packages: []string{"acme/orders"}}}
	calls, retries := 0, 0
	failure := &indexPublicationError{[]error{errors.New("invalid service specification")}}
	shared := &runtimeSharedState{indexes: indexes, reindex: func(context.Context, []string) (core.Result, error) {
		calls++
		return nil, failure
	}, retry: func(context.Context) error { retries++; return failure }}
	for range 2 {
		if err := reconcileSharedState(context.Background(), db, settings, gate, shared); err != nil || !gate.available {
			t.Fatalf("gate=%#v error=%v", gate, err)
		}
	}
	if calls != 1 || retries != 2 || !reflect.DeepEqual(indexes.acks, []uint64{9}) {
		t.Fatalf("calls=%d retries=%d acks=%v", calls, retries, indexes.acks)
	}
}

func TestRuntimeProfileSeparatesBoundedTemporaryAndDenoCacheMounts(t *testing.T) {
	resources := model.ResourceLimits{TmpfsMaximum: 64 << 20}
	profile := runtimeProfile(model.WorkloadJob, "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil, resources, false)
	if len(profile.Mounts) != 2 {
		t.Fatalf("mounts=%#v", profile.Mounts)
	}
	want := map[string]bool{"/tmp": false, "/runtime-cache": false}
	for _, mount := range profile.Mounts {
		if _, exists := want[mount.Target]; !exists || mount.Purpose != "temporary" || mount.Persistence != "ephemeral" || mount.MaximumSize != resources.TmpfsMaximum {
			t.Fatalf("mount=%#v", mount)
		}
		want[mount.Target] = true
	}
	for target, found := range want {
		if !found {
			t.Fatalf("bounded mount %s missing: %#v", target, profile.Mounts)
		}
	}
}

func TestSharedDenoCacheSurvivesRuntimeRecreation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	for _, workload := range []model.WorkloadType{model.WorkloadJob, model.WorkloadService} {
		mounts, err := sharedDenoCacheMounts(root)
		if err != nil {
			t.Fatal(err)
		}
		profile := runtimeProfile(workload, "sha256:"+strings.Repeat("a", 64), mounts, model.ResourceLimits{TmpfsMaximum: 64 << 20}, true)
		var shared []string
		for _, mount := range profile.Mounts {
			if mount.Target == "/runtime-cache" {
				if mount.Source != "" || mount.Persistence != "ephemeral" {
					t.Fatalf("SQLite caches must remain private: %#v", mount)
				}
				continue
			}
			if !strings.HasPrefix(mount.Target, "/runtime-cache/") {
				continue
			}
			shared = append(shared, mount.Target)
			if mount.Source != filepath.Join(root, strings.TrimPrefix(mount.Target, "/runtime-cache/")) || mount.ReadOnly || mount.Persistence != "node" {
				t.Fatalf("shared cache must be writable and node-local: %#v", mount)
			}
			info, err := os.Stat(mount.Source)
			if err != nil || info.Mode().Perm() != 0777 {
				t.Fatalf("cache must admit the runtime image user: %v, %v", info, err)
			}
			entry := filepath.Join(mount.Source, "retained-entry")
			if workload == model.WorkloadJob {
				if err := os.WriteFile(entry, []byte("cached"), 0644); err != nil {
					t.Fatal(err)
				}
			} else if data, err := os.ReadFile(entry); err != nil || string(data) != "cached" {
				t.Fatalf("cache was not retained across workloads/recreation: %q, %v", data, err)
			}
		}
		slices.Sort(shared)
		if !slices.Equal(shared, []string{"/runtime-cache/gen", "/runtime-cache/npm", "/runtime-cache/remote"}) {
			t.Fatalf("expected npm, remote and gen cache mounts, got %#v", profile.Mounts)
		}
	}
}

func TestRuntimeProfileEnablesRemoteImportsWithEgress(t *testing.T) {
	profile := runtimeProfile(model.WorkloadJob, "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil, model.ResourceLimits{}, true)
	if !profile.EgressAllowed || profile.DependencyMode != model.DependencyOnline {
		t.Fatalf("networked runtime profile = %#v", profile)
	}
	for _, path := range []string{"/tmp", "/runtime-cache"} {
		if !slices.Contains(profile.Permissions.ReadPaths, path) || !slices.Contains(profile.Permissions.WritePaths, path) {
			t.Fatalf("temporary path %s is not readable and writable: %#v", path, profile.Permissions)
		}
	}
}

func TestConfiguredRuntimeIdentityRequiresPinnedBackendAndImage(t *testing.T) {
	versions := runtimehost.Versions{RuntimeImage: runtimehost.RuntimeImageVersion{Name: "the8020/runtime-deno:1"}}
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := validateConfiguredRuntimeIdentity(containerdbackend.RuntimeName, versions.RuntimeImage.Name, zeroImageDigest, versions, digest); err != nil {
		t.Fatal(err)
	}
	if err := validateConfiguredRuntimeIdentity(containerdbackend.RuntimeName, versions.RuntimeImage.Name, digest, versions, digest); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, runtimeName, reference, configured, observed string
	}{
		{"runtime", "io.containerd.runc.v2", versions.RuntimeImage.Name, zeroImageDigest, digest},
		{"reference", containerdbackend.RuntimeName, "other/runtime:1", zeroImageDigest, digest},
		{"invalid digest", containerdbackend.RuntimeName, versions.RuntimeImage.Name, "latest", digest},
		{"digest mismatch", containerdbackend.RuntimeName, versions.RuntimeImage.Name, "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", digest},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateConfiguredRuntimeIdentity(test.runtimeName, test.reference, test.configured, versions, test.observed); err == nil {
				t.Fatal("invalid configured runtime identity was accepted")
			}
		})
	}
}

type recordedFailureSink struct {
	calls []failureCall
	err   error
}

type failureCall struct {
	sandboxID string
	reason    string
}

func (s *recordedFailureSink) FailSandbox(sandboxID, reason string) error {
	s.calls = append(s.calls, failureCall{sandboxID: sandboxID, reason: reason})
	return s.err
}

func (s *recordedFailureSink) RetireUnavailable(serviceID, reason string) error {
	s.calls = append(s.calls, failureCall{sandboxID: serviceID, reason: reason})
	return s.err
}

func TestPropagateReconciledFailuresSelectsHealthySandboxesAndFailsUnavailableSandboxes(t *testing.T) {
	items := []manager.Inspection{
		{Spec: model.SandboxSpec{SandboxID: "sandbox-ready"}, Status: model.SandboxStatus{ObservedState: model.StateReady, SupervisorHealthy: true}},
		{Spec: model.SandboxSpec{SandboxID: "sandbox-active"}, Status: model.SandboxStatus{ObservedState: model.StateActive, SupervisorHealthy: true}},
		{Spec: model.SandboxSpec{SandboxID: "sandbox-unhealthy"}, Status: model.SandboxStatus{ObservedState: model.StateReady}},
		{Spec: model.SandboxSpec{SandboxID: "sandbox-failed"}, Status: model.SandboxStatus{ObservedState: model.StateFailed, FailureReason: "containerd task is missing"}},
	}
	first, second := &recordedFailureSink{}, &recordedFailureSink{}
	healthy, err := propagateReconciledFailures(items, first, second)
	if err != nil {
		t.Fatal(err)
	}
	wantHealthy := map[string]bool{"sandbox-ready": true, "sandbox-active": true}
	if !reflect.DeepEqual(healthy, wantHealthy) {
		t.Fatalf("healthy=%#v want=%#v", healthy, wantHealthy)
	}
	for _, sink := range []*recordedFailureSink{first, second} {
		if len(sink.calls) != 2 || sink.calls[0].sandboxID != "sandbox-unhealthy" || !strings.Contains(sink.calls[0].reason, "supervisor_healthy=false") || sink.calls[1] != (failureCall{sandboxID: "sandbox-failed", reason: "containerd task is missing"}) {
			t.Fatalf("calls=%#v", sink.calls)
		}
	}
}

func TestPropagateReconciledFailuresReturnsSinkErrors(t *testing.T) {
	sinkFailure := errors.New("persist workload failure")
	_, err := propagateReconciledFailures([]manager.Inspection{{Spec: model.SandboxSpec{SandboxID: "sandbox"}, Status: model.SandboxStatus{ObservedState: model.StateFailed}}}, &recordedFailureSink{err: sinkFailure})
	if !errors.Is(err, sinkFailure) {
		t.Fatalf("error=%v", err)
	}
}

func TestFailUnavailableServicePoolsRetiresEveryMissingPoolWithoutSupervisorProbes(t *testing.T) {
	sink := &recordedFailureSink{}
	records := []executionservices.Record{
		{ServiceID: "healthy", SandboxID: "sandbox-healthy", State: "READY"},
		{ServiceID: "missing-ready", SandboxID: "sandbox-missing", State: "READY"},
		{ServiceID: "missing-idle-same-group", SandboxID: "sandbox-missing", State: "IDLE"},
		{ServiceID: "missing-starting", SandboxID: "sandbox-starting", State: "STARTING"},
		{ServiceID: "already-failed", SandboxID: "sandbox-failed", State: "FAILED"},
	}
	if err := failUnavailableServicePools(records, map[string]bool{"sandbox-healthy": true}, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.calls) != 4 || sink.calls[0].sandboxID != "missing-ready" || sink.calls[1].sandboxID != "missing-idle-same-group" || sink.calls[2].sandboxID != "missing-starting" || sink.calls[3].sandboxID != "already-failed" {
		t.Fatalf("failure calls=%#v", sink.calls)
	}
	for _, call := range sink.calls {
		if !strings.Contains(call.reason, "is not healthy") {
			t.Fatalf("failure reason=%q", call.reason)
		}
	}
}

func TestSharedStateHealthDeadlineDoesNotBoundOrdinaryIndexJobs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := &sharedStateDatabaseStub{status: database.Status{State: database.StateReady}}
	packages := &sharedPackageStateStub{}
	if err := reconcileSharedState(ctx, db, &sharedSettingsStub{}, &servicePlaneGateStub{}, packages); err != nil {
		t.Fatal(err)
	}
	probeDeadline, _ := db.context.Deadline()
	jobDeadline, _ := packages.context.Deadline()
	if jobDeadline.Sub(probeDeadline) < time.Minute || packages.context.Err() != nil {
		t.Fatal("index jobs inherited the short or canceled health probe context")
	}
}
