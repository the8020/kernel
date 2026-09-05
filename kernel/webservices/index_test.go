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
	if _, err := index.ReplacePackage("acme/api", []Specification{one, two}, "hooks-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := index.ReplacePackage("acme/other", []Specification{unrelated}, "hooks-a"); err != nil {
		t.Fatal(err)
	}
	before, _ := index.ReadService(one.ServiceID)
	updated := one
	updated.Version++
	invalid := two
	invalid.Effective.Placement.WorkersPerSandbox = 0
	for _, draft := range [][]Specification{{updated, invalid}, {updated, unrelated}, {updated, updated}} {
		if _, err := index.ReplacePackage("acme/api", draft, "hooks-b"); err == nil {
			t.Fatal("invalid, out-of-scope, or duplicate specification was accepted")
		}
		if got, _ := index.ReadService(one.ServiceID); !reflect.DeepEqual(got, before) {
			t.Fatalf("partially published failed fragment: %#v", got)
		}
	}
	removed, err := index.ReplacePackage("acme/api", []Specification{updated}, "hooks-b")
	if err != nil || !reflect.DeepEqual(removed, []string{two.ServiceID}) {
		t.Fatalf("removed=%v error=%v", removed, err)
	}
	if _, err := index.ReadService(two.ServiceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("omitted service remained indexed: %v", err)
	}
	if got, _ := index.ReadService(unrelated.ServiceID); got.CodeRevision != unrelated.CodeRevision {
		t.Fatal("unrelated fragment changed")
	}
	if _, err := index.ReplacePackage("acme/api", nil, "hooks-b"); err != nil {
		t.Fatal(err)
	}
	if got := index.ServiceIDs(); !reflect.DeepEqual(got, []string{unrelated.ServiceID}) {
		t.Fatalf("package removal left services: %v", got)
	}
}

func TestRuntimeIndexReleaseIncludesResolvedConfigurationAndHookCode(t *testing.T) {
	index := NewIndex()
	spec := indexedTestSpecification("acme/api/one")
	publish := func(chain string) Specification {
		t.Helper()
		if _, err := index.ReplacePackage("acme/api", []Specification{spec}, chain); err != nil {
			t.Fatal(err)
		}
		got, err := index.ReadService(spec.ServiceID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := publish("hooks-a")
	if publish("hooks-a").Release != first.Release {
		t.Fatal("unchanged resolved specification invalidated reuse")
	}
	second := publish("hooks-b")
	if second.Release == first.Release {
		t.Fatal("changed provider code retained the old runtime release")
	}
	spec.Effective.Scaling.ConcurrencyPerWorker = 2
	third := publish("hooks-b")
	if third.Release == second.Release || third.Version != second.Version {
		t.Fatal("enhanced configuration must change runtime compatibility independently of application version numbering")
	}
	third.Effective.Scaling.ConcurrencyPerWorker = 9
	if got, _ := index.ReadService(spec.ServiceID); got.Effective.Scaling.ConcurrencyPerWorker != 2 {
		t.Fatal("caller mutated the accepted index")
	}
}
