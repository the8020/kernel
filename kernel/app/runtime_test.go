package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	executionservices "the8020/kernel/execution/services"
	runtimehost "the8020/kernel/runtime"
	containerdbackend "the8020/kernel/sandbox/backend/containerd"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
)

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
	want := []string{"runtime controllers", "runtime ports", "runtime sandboxes", "runtime backends"}
	if !reflect.DeepEqual(stages, want) {
		t.Fatalf("stages=%#v want=%#v", stages, want)
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
	groupID string
	reason  string
}

func (s *recordedFailureSink) FailGroup(groupID, reason string) error {
	s.calls = append(s.calls, failureCall{groupID: groupID, reason: reason})
	return s.err
}

func (s *recordedFailureSink) RetireUnavailable(serviceID, reason string) error {
	s.calls = append(s.calls, failureCall{groupID: serviceID, reason: reason})
	return s.err
}

func TestPropagateReconciledFailuresSelectsHealthySandboxesAndFailsUnavailableGroups(t *testing.T) {
	items := []manager.Inspection{
		{Spec: model.SandboxSpec{RuntimeGroupID: "group-ready", SandboxID: "sandbox-ready"}, Status: model.SandboxStatus{ObservedState: model.StateReady, SupervisorHealthy: true}},
		{Spec: model.SandboxSpec{RuntimeGroupID: "group-active", SandboxID: "sandbox-active"}, Status: model.SandboxStatus{ObservedState: model.StateActive, SupervisorHealthy: true}},
		{Spec: model.SandboxSpec{RuntimeGroupID: "group-unhealthy", SandboxID: "sandbox-unhealthy"}, Status: model.SandboxStatus{ObservedState: model.StateReady}},
		{Spec: model.SandboxSpec{RuntimeGroupID: "group-failed", SandboxID: "sandbox-failed"}, Status: model.SandboxStatus{ObservedState: model.StateFailed, FailureReason: "containerd task is missing"}},
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
		if len(sink.calls) != 2 || sink.calls[0].groupID != "group-unhealthy" || !strings.Contains(sink.calls[0].reason, "supervisor_healthy=false") || sink.calls[1] != (failureCall{groupID: "group-failed", reason: "containerd task is missing"}) {
			t.Fatalf("calls=%#v", sink.calls)
		}
	}
}

func TestPropagateReconciledFailuresReturnsSinkErrors(t *testing.T) {
	sinkFailure := errors.New("persist workload failure")
	_, err := propagateReconciledFailures([]manager.Inspection{{Spec: model.SandboxSpec{RuntimeGroupID: "group", SandboxID: "sandbox"}, Status: model.SandboxStatus{ObservedState: model.StateFailed}}}, &recordedFailureSink{err: sinkFailure})
	if !errors.Is(err, sinkFailure) {
		t.Fatalf("error=%v", err)
	}
}

func TestFailUnavailableServicePoolsRetiresEveryMissingPoolWithoutSupervisorProbes(t *testing.T) {
	sink := &recordedFailureSink{}
	records := []executionservices.Record{
		{ServiceID: "healthy", RuntimeGroupID: "group-healthy", SandboxID: "sandbox-healthy", State: "READY"},
		{ServiceID: "missing-ready", RuntimeGroupID: "group-missing", SandboxID: "sandbox-missing", State: "READY"},
		{ServiceID: "missing-idle-same-group", RuntimeGroupID: "group-missing", SandboxID: "sandbox-missing", State: "IDLE"},
		{ServiceID: "missing-starting", RuntimeGroupID: "group-starting", SandboxID: "sandbox-starting", State: "STARTING"},
		{ServiceID: "already-failed", RuntimeGroupID: "group-failed", SandboxID: "sandbox-failed", State: "FAILED"},
	}
	if err := failUnavailableServicePools(records, map[string]bool{"sandbox-healthy": true}, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.calls) != 4 || sink.calls[0].groupID != "missing-ready" || sink.calls[1].groupID != "missing-idle-same-group" || sink.calls[2].groupID != "missing-starting" || sink.calls[3].groupID != "already-failed" {
		t.Fatalf("failure calls=%#v", sink.calls)
	}
	for _, call := range sink.calls {
		if !strings.Contains(call.reason, "is not healthy") {
			t.Fatalf("failure reason=%q", call.reason)
		}
	}
}
