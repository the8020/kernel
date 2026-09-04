package reindex

import (
	"context"
	"testing"

	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func TestCommandRunsTheLivePackageCatalogRefresh(t *testing.T) {
	called := false
	serviceSet := &services.Services{}
	serviceSet.PublishRuntime(&services.RuntimeServices{ReindexCommands: func(context.Context) (core.Result, error) {
		called = true
		return core.Result{"revision": "process-2", "commands": 7}, nil
	}})
	result, err := New(serviceSet)(context.Background(), core.Request{})
	if err != nil || !called || result["revision"] != "process-2" || result["commands"] != 7 {
		t.Fatalf("result=%#v called=%t error=%v", result, called, err)
	}
}
