package packagecommands

import (
	"context"
	"errors"
	"reflect"
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
	if !reflect.DeepEqual(results[0].RetiredServices, runtimeServices.retired) || !reflect.DeepEqual(results[0].RestartedServices, runtimeServices.reloaded) {
		t.Fatalf("synchronization result = %#v", results[0])
	}
}

func TestSynchronizeOfflineDoesNotRequireRuntimeServices(t *testing.T) {
	management := synchronizationManagement{results: []workspacepackages.PackageSynchronization{{
		PackageID: "example/offline", Success: true, Changed: true, Services: []string{"example/offline/service"},
	}}}
	results, err := Synchronize(context.Background(), &services.Services{PackageManagement: management}, []string{"example/offline"})
	if err != nil || len(results) != 1 || !results[0].Success || len(results[0].RestartedServices) != 0 {
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
	if len(results) != 1 || results[0].Success || results[0].Error == "" {
		t.Fatalf("failure result = %#v", results)
	}
}
