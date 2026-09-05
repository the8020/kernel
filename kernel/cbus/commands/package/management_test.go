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
)

type synchronizationManagement struct {
	services.PackageManagementService
	results []workspacepackages.PackageSynchronization
	err     error
}

func (management synchronizationManagement) SynchronizePackages(context.Context, []string) ([]workspacepackages.PackageSynchronization, error) {
	return append([]workspacepackages.PackageSynchronization(nil), management.results...), management.err
}

func TestSynchronizeReturnsPackagePublicationResults(t *testing.T) {
	management := synchronizationManagement{results: []workspacepackages.PackageSynchronization{
		{
			PackageID: "example/changed",
			Commit:    "1111111111111111111111111111111111111111",
			Success:   true,
			Changed:   true,
		},
		{PackageID: "example/unchanged", Success: true},
	}}
	serviceSet := &services.Services{
		PackageManagement: management,
	}
	results, err := Synchronize(context.Background(), serviceSet, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := SynchronizationResult{PackageID: "example/changed", Commit: "1111111111111111111111111111111111111111", Success: true}
	if results[0] != want {
		t.Fatalf("synchronization result = %#v, want %#v", results[0], want)
	}
}

func TestSynchronizeOfflineDoesNotRequireRuntimeServices(t *testing.T) {
	management := synchronizationManagement{results: []workspacepackages.PackageSynchronization{{
		PackageID: "example/offline", Success: true, Changed: true,
	}}}
	results, err := Synchronize(context.Background(), &services.Services{PackageManagement: management}, []string{"example/offline"})
	if err != nil || len(results) != 1 || !results[0].Success {
		t.Fatalf("offline synchronization = %#v, err=%v", results, err)
	}
}

func TestSynchronizeReportsPublicationFailure(t *testing.T) {
	management := synchronizationManagement{results: []workspacepackages.PackageSynchronization{{
		PackageID: "example/failure", Changed: true, Error: "package synchronized but index publication failed",
	}}}
	serviceSet := &services.Services{
		PackageManagement: management,
	}
	results, err := Synchronize(context.Background(), serviceSet, nil)
	var commandError *core.Error
	if !errors.As(err, &commandError) || commandError.Code != core.CodeRuntimeOperation {
		t.Fatalf("error = %#v", err)
	}
	if len(results) != 1 || results[0].Success || !strings.Contains(commandError.Message, "example/failure: package synchronized but index publication failed") {
		t.Fatalf("failure result = %#v", results)
	}
	details, ok := commandError.Details["packages"].([]SynchronizationResult)
	if !ok || !reflect.DeepEqual(details, results) {
		t.Fatalf("failure details = %#v, want %#v", commandError.Details, results)
	}
}
