package delete

import (
	"context"
	"errors"
	"os"
	"testing"

	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

type deletionRecorder struct {
	services.PackageManagementService
	id  string
	err error
}

func (r *deletionRecorder) DeletePackage(_ context.Context, id string) error {
	r.id = id
	return r.err
}

func TestDeleteRequiresConfirmationAndReportsOwnerFailure(t *testing.T) {
	r := &deletionRecorder{}
	handler := New(&services.Services{PackageManagement: r})
	request := core.Request{Arguments: map[string]any{"package_id": "acme/example"}}
	if _, err := handler(context.Background(), request); err == nil || r.id != "" {
		t.Fatalf("unconfirmed deletion: id=%q err=%v", r.id, err)
	}
	request.Arguments["confirm"] = true
	result, err := handler(context.Background(), request)
	if err != nil || result["deleted"] != true || r.id != "acme/example" {
		t.Fatalf("deletion: result=%v id=%q err=%v", result, r.id, err)
	}
	r.err = os.ErrNotExist
	result, err = handler(context.Background(), request)
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != core.CodeNotFound || result["deleted"] != false {
		t.Fatalf("failed deletion: result=%v err=%v", result, err)
	}
}
