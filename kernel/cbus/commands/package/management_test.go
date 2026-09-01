package packagecommands

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"the8020/kernel/cbus/core"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/services"
	"the8020/kernel/webservices"
)

type synchronizationManagement struct {
	services.PackageManagementService
	results []workspacepackages.PackageSynchronization
	err     error
}

func (management synchronizationManagement) SynchronizePackages(context.Context, []string) ([]workspacepackages.PackageSynchronization, error) {
	return append([]workspacepackages.PackageSynchronization(nil), management.results...), management.err
}

type synchronizationWebServices struct {
	services.WebServiceService
	reloaded []string
	retired  []string
	fail     string
}

func (service *synchronizationWebServices) Reload(_ context.Context, serviceID string) (webservices.Status, error) {
	service.reloaded = append(service.reloaded, serviceID)
	if serviceID == service.fail {
		return webservices.Status{}, errors.New("reload failed")
	}
	return webservices.Status{ServiceID: serviceID}, nil
}

func (service *synchronizationWebServices) Retire(_ context.Context, serviceID string) error {
	service.retired = append(service.retired, serviceID)
	if serviceID == service.fail {
		return errors.New("retire failed")
	}
	return nil
}

func TestSynchronizeRefreshesOnlyChangedPackageServices(t *testing.T) {
	management := synchronizationManagement{results: []workspacepackages.PackageSynchronization{
		{
			PackageID:        "example/changed",
			Commit:           "1111111111111111111111111111111111111111",
			Success:          true,
			Changed:          true,
			PreviousServices: []string{"example/changed/kept", "example/changed/removed"},
			Services:         []string{"example/changed/kept", "example/changed/added"},
		},
		{PackageID: "example/unchanged", Success: true, Services: []string{"example/unchanged/service"}},
	}}
	runtimeServices := &synchronizationWebServices{}
	serviceSet := &services.Services{
		PackageManagement: management,
		Runtime:           &services.RuntimeServices{Services: runtimeServices},
	}
	results, err := Synchronize(context.Background(), serviceSet, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runtimeServices.retired, []string{"example/changed/removed"}) {
		t.Fatalf("retired services = %v", runtimeServices.retired)
	}
	if !reflect.DeepEqual(runtimeServices.reloaded, []string{"example/changed/kept", "example/changed/added"}) {
		t.Fatalf("reloaded services = %v", runtimeServices.reloaded)
	}
	want := SynchronizationResult{PackageID: "example/changed", Commit: "1111111111111111111111111111111111111111", Success: true}
	if results[0] != want {
		t.Fatalf("synchronization result = %#v, want %#v", results[0], want)
	}
}

func TestSynchronizeOfflineDoesNotRequireRuntimeServices(t *testing.T) {
	management := synchronizationManagement{results: []workspacepackages.PackageSynchronization{{
		PackageID: "example/offline", Success: true, Changed: true, Services: []string{"example/offline/service"},
	}}}
	results, err := Synchronize(context.Background(), &services.Services{PackageManagement: management}, []string{"example/offline"})
	if err != nil || len(results) != 1 || !results[0].Success {
		t.Fatalf("offline synchronization = %#v, err=%v", results, err)
	}
}

func TestSynchronizeReportsServiceRefreshFailureAfterPackageCommit(t *testing.T) {
	management := synchronizationManagement{results: []workspacepackages.PackageSynchronization{{
		PackageID: "example/failure", Success: true, Changed: true, Services: []string{"example/failure/service"},
	}}}
	runtimeServices := &synchronizationWebServices{fail: "example/failure/service"}
	serviceSet := &services.Services{
		PackageManagement: management,
		Runtime:           &services.RuntimeServices{Services: runtimeServices},
	}
	results, err := Synchronize(context.Background(), serviceSet, nil)
	var commandError *core.Error
	if !errors.As(err, &commandError) || commandError.Code != core.CodeRuntimeOperation {
		t.Fatalf("error = %#v", err)
	}
	if len(results) != 1 || results[0].Success || !strings.Contains(commandError.Message, "example/failure: package synchronized but service refresh failed: reload failed") {
		t.Fatalf("failure result = %#v", results)
	}
	details, ok := commandError.Details["packages"].([]SynchronizationResult)
	if !ok || !reflect.DeepEqual(details, results) {
		t.Fatalf("failure details = %#v, want %#v", commandError.Details, results)
	}
}

func TestRepositoryMutationRefreshesOnlyAffectedServices(t *testing.T) {
	runtimeServices := &synchronizationWebServices{}
	serviceSet := &services.Services{Runtime: &services.RuntimeServices{Services: runtimeServices}}
	err := RefreshRepositoryMutation(context.Background(), serviceSet, workspacepackages.RepositoryMutation{
		Changed:          true,
		PreviousServices: []string{"example/repo/kept", "example/repo/removed"},
		Services:         []string{"example/repo/kept", "example/repo/added"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runtimeServices.retired, []string{"example/repo/removed"}) {
		t.Fatalf("retired services = %v", runtimeServices.retired)
	}
	if !reflect.DeepEqual(runtimeServices.reloaded, []string{"example/repo/kept", "example/repo/added"}) {
		t.Fatalf("reloaded services = %v", runtimeServices.reloaded)
	}
}

func TestUnchangedRepositoryMutationDoesNotTouchServices(t *testing.T) {
	runtimeServices := &synchronizationWebServices{}
	serviceSet := &services.Services{Runtime: &services.RuntimeServices{Services: runtimeServices}}
	if err := RefreshRepositoryMutation(context.Background(), serviceSet, workspacepackages.RepositoryMutation{Services: []string{"example/repo/service"}}); err != nil {
		t.Fatal(err)
	}
	if len(runtimeServices.reloaded)+len(runtimeServices.retired) != 0 {
		t.Fatalf("unexpected service refresh: reload=%v retire=%v", runtimeServices.reloaded, runtimeServices.retired)
	}
}
