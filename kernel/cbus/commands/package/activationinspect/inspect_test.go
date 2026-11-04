package activationinspect

import (
	"context"
	"testing"

	"the8020/kernel/cbus/core"
	"the8020/kernel/packages"
	"the8020/kernel/services"
)

type inspector struct {
	services.PackageManagementService
}

func (inspector) InspectActivation(_ context.Context, id string) (packages.ActivationInspection, error) {
	return packages.ActivationInspection{ID: id, Stage: "code_switched"}, nil
}

func TestCommandDelegatesExactActivation(t *testing.T) {
	request := core.Request{Arguments: map[string]any{"activation_id": "act-0123456789"}}
	result, err := New(&services.Services{PackageManagement: inspector{}})(t.Context(), request)
	if err != nil || result["activation"].(packages.ActivationInspection).ID != "act-0123456789" {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if _, err := New(&services.Services{})(t.Context(), request); err == nil {
		t.Fatal("unavailable management accepted")
	}
}
