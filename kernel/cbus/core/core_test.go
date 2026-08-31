package core

import (
	"context"
	"encoding/json"
	"testing"
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
	if response.Result["count"] != int64(2) || response.Result["enabled"] != true {
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
