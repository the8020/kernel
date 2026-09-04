package core

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestRegistryValidatesAndConvertsArguments(t *testing.T) {
	command := Command{ID: "test.run", Parameters: []Parameter{{Name: "count", Type: "integer", Required: true}, {Name: "enabled", Type: "boolean", Position: 1, Required: true}}}
	registry := NewRegistry(nil)
	if err := registry.Register(command, func(_ context.Context, request Request) (Result, error) {
		return Result{"count": request.Arguments["count"], "enabled": request.Arguments["enabled"]}, nil
	}); err != nil {
		t.Fatal(err)
	}
	response := registry.Execute(context.Background(), Request{ProtocolVersion: ProtocolVersion, CommandID: "test.run", Arguments: map[string]any{"count": json.Number("2"), "enabled": "true"}})
	if !response.Success {
		t.Fatalf("response failed: %#v", response.Error)
	}
	result := response.Result.(Result)
	if result["count"] != int64(2) || result["enabled"] != true {
		t.Fatalf("wrong typed result: %#v", response.Result)
	}
	missing := registry.Execute(context.Background(), Request{ProtocolVersion: ProtocolVersion, CommandID: "test.run"})
	if missing.Error == nil || missing.Error.Code != CodeInvalidArguments {
		t.Fatalf("missing argument response: %#v", missing)
	}
	unknown := registry.Execute(context.Background(), Request{ProtocolVersion: ProtocolVersion, CommandID: "missing"})
	if unknown.Error.Code != CodeUnknownCommand {
		t.Fatalf("unknown response: %#v", unknown)
	}
}

func TestPackageCatalogSwapDoesNotInterruptAnInFlightSnapshot(t *testing.T) {
	registry := NewRegistry(nil)
	started := make(chan struct{})
	release := make(chan struct{})
	oldCommand := Command{ID: "example/tool/run", Name: "example.tool.run"}
	if err := registry.ReplacePackages([]Registration{{Command: oldCommand, Handler: func(_ context.Context, request Request) (Execution, error) {
		close(started)
		<-release
		return Execution{Result: append([]string{"old"}, request.Argv...)}, nil
	}}}, nil); err != nil {
		t.Fatal(err)
	}
	oldRevision := registry.Catalog().Revision
	oldResponse := make(chan Response, 1)
	go func() {
		oldResponse <- registry.Execute(context.Background(), Request{
			ProtocolVersion: ProtocolVersion, CommandID: oldCommand.ID,
			CatalogRevision: oldRevision, Argv: []string{"--literal", "value"},
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("old handler did not start")
	}
	if err := registry.ReplacePackages([]Registration{{Command: oldCommand, Handler: func(_ context.Context, request Request) (Execution, error) {
		return Execution{Result: append([]string{"new"}, request.Argv...)}, nil
	}}}, nil); err != nil {
		t.Fatal(err)
	}
	newCatalog := registry.Catalog()
	if newCatalog.Revision == oldRevision {
		t.Fatal("catalog revision did not change")
	}
	current := registry.Execute(context.Background(), Request{
		ProtocolVersion: ProtocolVersion, CommandID: oldCommand.ID,
		CatalogRevision: newCatalog.Revision, Argv: []string{"raw"},
	})
	if !current.Success || !reflect.DeepEqual(current.Result, []string{"new", "raw"}) {
		t.Fatalf("new response = %#v", current)
	}
	close(release)
	select {
	case response := <-oldResponse:
		if !response.Success || !reflect.DeepEqual(response.Result, []string{"old", "--literal", "value"}) {
			t.Fatalf("old response = %#v", response)
		}
	case <-time.After(time.Second):
		t.Fatal("old handler did not finish")
	}
}

func TestSecureInputValidationAllowsOptionalAndRejectsMissingOrUnknownValues(t *testing.T) {
	registry := NewRegistry(nil)
	command := Command{
		ID: "example/tool/secure", Name: "example.tool.secure",
		Secrets: []SecretInput{{Name: "optional"}, {Name: "required", Required: true}},
	}
	if err := registry.ReplacePackages([]Registration{{Command: command, Handler: func(_ context.Context, request Request) (Execution, error) {
		return Execution{Result: request.Secrets}, nil
	}}}, nil); err != nil {
		t.Fatal(err)
	}
	revision := registry.Catalog().Revision
	missing := registry.Execute(context.Background(), Request{ProtocolVersion: ProtocolVersion, CommandID: command.ID, CatalogRevision: revision})
	if missing.Error == nil || missing.Error.Code != CodeInvalidArguments {
		t.Fatalf("missing response = %#v", missing)
	}
	unknown := registry.Execute(context.Background(), Request{
		ProtocolVersion: ProtocolVersion, CommandID: command.ID, CatalogRevision: revision,
		Secrets: map[string]string{"required": "present", "unknown": "value"},
	})
	if unknown.Error == nil || unknown.Error.Code != CodeInvalidArguments {
		t.Fatalf("unknown response = %#v", unknown)
	}
	valid := registry.Execute(context.Background(), Request{
		ProtocolVersion: ProtocolVersion, CommandID: command.ID, CatalogRevision: revision,
		Secrets: map[string]string{"required": "present"},
	})
	if !valid.Success || !reflect.DeepEqual(valid.Result, map[string]string{"required": "present"}) {
		t.Fatalf("valid response = %#v", valid)
	}
}
