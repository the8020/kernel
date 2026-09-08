package webservices

import (
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

func indexedTestSpecification(id string) Specification {
	return Specification{
		ServiceID: id, Version: 1, CodeRevision: "commit-a", Enabled: true,
		EntrypointURL: "file:///workspace/packages/acme/api/service.ts",
		Access:        AccessPolicy{Mode: "public"},
		Effective: Configuration{
			Execution: ExecutionConfiguration{AnonymousUser: "system"},
			Lifecycle: LifecycleConfiguration{ServiceType: "stateless", SessionKeepAlive: time.Minute},
			Scaling:   ScalingConfiguration{ConcurrencyPerWorker: 1, TargetUtilization: 1, WorkerKeepAlive: time.Minute},
			Placement: PlacementConfiguration{WorkersPerSandbox: 1},
			Timeouts:  TimeoutConfiguration{Request: time.Second, Drain: time.Second},
		},
	}
}

func TestRuntimeIndexPublishesOnlyCompleteValidatedPackageFragments(t *testing.T) {
	index := NewIndex()
	one, two, unrelated := indexedTestSpecification("acme/api/one"), indexedTestSpecification("acme/api/two"), indexedTestSpecification("acme/other/keep")
	if _, err := index.ReplacePackage("acme/api", []Specification{one, two}); err != nil {
		t.Fatal(err)
	}
	if _, err := index.ReplacePackage("acme/other", []Specification{unrelated}); err != nil {
		t.Fatal(err)
	}
	before, _ := index.ReadService(one.ServiceID)
	updated := one
	updated.Version++
	invalid := two
	invalid.Effective.Placement.WorkersPerSandbox = 0
	for _, draft := range [][]Specification{{updated, invalid}, {updated, unrelated}, {updated, updated}} {
		if _, err := index.ReplacePackage("acme/api", draft); err == nil {
			t.Fatal("invalid, out-of-scope, or duplicate specification was accepted")
		}
		if got, _ := index.ReadService(one.ServiceID); !reflect.DeepEqual(got, before) {
			t.Fatalf("partially published failed fragment: %#v", got)
		}
	}
	removed, err := index.ReplacePackage("acme/api", []Specification{updated})
	if err != nil || !reflect.DeepEqual(removed, []string{two.ServiceID}) {
		t.Fatalf("removed=%v error=%v", removed, err)
	}
	if _, err := index.ReadService(two.ServiceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("omitted service remained indexed: %v", err)
	}
	if got, _ := index.ReadService(unrelated.ServiceID); got.CodeRevision != unrelated.CodeRevision {
		t.Fatal("unrelated fragment changed")
	}
	if _, err := index.ReplacePackage("acme/api", nil); err != nil {
		t.Fatal(err)
	}
	if got := index.ServiceIDs(); !reflect.DeepEqual(got, []string{unrelated.ServiceID}) {
		t.Fatalf("package removal left services: %v", got)
	}
}

func TestRuntimeIndexReleaseIncludesConfigurationButSourceChangesRequireObservedImports(t *testing.T) {
	index := NewIndex()
	spec := indexedTestSpecification("acme/api/one")
	publish := func() Specification {
		t.Helper()
		if _, err := index.ReplacePackage("acme/api", []Specification{spec}); err != nil {
			t.Fatal(err)
		}
		got, err := index.ReadService(spec.ServiceID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := publish()
	if publish().Release != first.Release {
		t.Fatal("unchanged resolved specification invalidated reuse")
	}
	spec.CodeRevision = "changed-commit"
	second := publish()
	if second.Release != first.Release {
		t.Fatal("source identity alone replaced Workers without an import match")
	}
	spec.Effective.Scaling.ConcurrencyPerWorker = 2
	third := publish()
	if third.Release == second.Release || third.Version != second.Version {
		t.Fatal("enhanced configuration must change runtime compatibility independently of application version numbering")
	}
	third.Effective.Scaling.ConcurrencyPerWorker = 9
	if got, _ := index.ReadService(spec.ServiceID); got.Effective.Scaling.ConcurrencyPerWorker != 2 {
		t.Fatal("caller mutated the accepted index")
	}
}

func TestRuntimeIndexSessionKeepAliveAllowsExplicitCompletionLifetime(t *testing.T) {
	spec := indexedTestSpecification("acme/api/one")
	spec.Effective.Lifecycle.ServiceType = "session"
	for _, duration := range []time.Duration{0, time.Millisecond, time.Minute} {
		spec.Effective.Lifecycle.SessionKeepAlive = duration
		if err := validateSpecification(spec); err != nil {
			t.Fatalf("keepalive %s: %v", duration, err)
		}
	}
	for _, duration := range []time.Duration{-time.Second, time.Nanosecond} {
		spec.Effective.Lifecycle.SessionKeepAlive = duration
		if err := validateSpecification(spec); err == nil {
			t.Fatalf("accepted invalid keepalive %s", duration)
		}
	}
	spec.Effective.Lifecycle.SessionKeepAlive = 0
	spec.Effective.Scaling.WorkerKeepAlive = 0
	if err := validateSpecification(spec); err == nil {
		t.Fatal("Worker idle cleanup still requires a positive keepalive")
	}
}
