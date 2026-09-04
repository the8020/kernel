package sandbox

import (
	"testing"

	"the8020/kernel/execution/supervisor"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/services"
	"the8020/kernel/webservices"
)

type targetedWebServices struct {
	services.WebServiceService
	inspected []string
	statuses  map[string]webservices.Status
}

func (f *targetedWebServices) Inspect(serviceID string) (webservices.Status, error) {
	f.inspected = append(f.inspected, serviceID)
	return f.statuses[serviceID], nil
}

func (f *targetedWebServices) List() ([]webservices.Status, error) {
	panic("sandbox detail must not scan every service")
}

func TestReasonUsesLogicalServiceIDForListAndDetailInspections(t *testing.T) {
	summary := manager.Inspection{Spec: model.SandboxSpec{
		WorkloadType: model.WorkloadService,
		GroupKey:     "service:placement:dXVpLWxvZ2lu",
		ServiceIDs: []string{
			"the8020/uui/shell",
			"the8020/uui/login",
		},
	}}
	detail := summary
	detail.Workers = []supervisor.WorkerStatus{
		{OwnerID: "the8020/uui/login"},
		{OwnerID: "the8020/uui/shell"},
	}

	const want = "service:the8020/uui/login"
	if got := Reason(summary); got != want {
		t.Fatalf("list reason = %q, want %q", got, want)
	}
	if got := Reason(detail); got != want {
		t.Fatalf("detail reason = %q, want %q", got, want)
	}
}

func TestDetailLooksUpOnlyServicesIndexedByTheSandbox(t *testing.T) {
	serviceView := &targetedWebServices{statuses: map[string]webservices.Status{
		"example/catalog/api": {ServiceID: "example/catalog/api", State: webservices.StateReady, Enabled: true, SandboxCount: 2, WorkerCount: 3},
	}}
	runtimeServices := &services.RuntimeServices{Services: serviceView}
	result, err := Detail(runtimeServices, manager.Inspection{Spec: model.SandboxSpec{
		SandboxID: "sandbox-a", WorkloadType: model.WorkloadService,
		ServiceIDs: []string{"example/catalog/api"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(serviceView.inspected) != 1 || serviceView.inspected[0] != "example/catalog/api" {
		t.Fatalf("inspected=%#v", serviceView.inspected)
	}
	associated, ok := result["services"].([]serviceAssociation)
	if !ok || len(associated) != 1 || associated[0].WorkerCount != 3 {
		t.Fatalf("associated=%#v", result["services"])
	}
}
