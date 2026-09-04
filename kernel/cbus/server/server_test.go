package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"the8020/kernel/cbus/client"
	"the8020/kernel/cbus/core"
)

func TestUnixHTTPTransport(t *testing.T) {
	registry := core.NewRegistry(nil)
	command := core.Command{ID: "test.echo", Parameters: []core.Parameter{{Name: "value", Type: "integer", Required: true}}}
	if err := registry.Register(command, func(_ context.Context, request core.Request) (core.Result, error) {
		return core.Result{"value": request.Arguments["value"]}, nil
	}); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "admin.sock")
	commandServer := New(socket, registry)
	if err := commandServer.Start(); err != nil {
		t.Fatal(err)
	}
	defer commandServer.Shutdown(context.Background())
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %o", info.Mode().Perm())
	}
	commandClient := client.New(socket)
	defer commandClient.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := commandClient.Execute(ctx, core.Request{CommandID: "test.echo", Arguments: map[string]any{"value": int64(3)}})
	if err != nil {
		t.Fatal(err)
	}
	result := response.Result.(map[string]any)
	if !response.Success || result["value"] != json.Number("3") {
		t.Fatalf("response: %#v", response)
	}
	unknown, err := commandClient.Execute(ctx, core.Request{CommandID: "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Error == nil || unknown.Error.Code != core.CodeUnknownCommand {
		t.Fatalf("unknown response: %#v", unknown)
	}
	catalog, unchanged, err := commandClient.Catalog(ctx, "")
	if err != nil || unchanged || catalog.Revision == "" || len(catalog.Commands) != 1 || catalog.Commands[0].ID != "test.echo" {
		t.Fatalf("catalog=%#v unchanged=%t error=%v", catalog, unchanged, err)
	}
	if _, unchanged, err = commandClient.Catalog(ctx, catalog.Revision); err != nil || !unchanged {
		t.Fatalf("conditional catalog unchanged=%t error=%v", unchanged, err)
	}
	statusCommand := core.Command{ID: "test.status"}
	if err := registry.Register(statusCommand, func(_ context.Context, _ core.Request) (core.Result, error) {
		return core.Result{"state": "shutting_down"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	updated, unchanged, err := commandClient.Catalog(ctx, catalog.Revision)
	if err != nil || unchanged || updated.Revision == catalog.Revision || len(updated.Commands) != 2 {
		t.Fatalf("updated catalog=%#v unchanged=%t error=%v", updated, unchanged, err)
	}
	commandServer.BeginShutdown("test.status")
	rejected, err := commandClient.Execute(ctx, core.Request{CommandID: "test.echo", Arguments: map[string]any{"value": int64(4)}})
	if err != nil || rejected.Error == nil || rejected.Error.Code != core.CodeShuttingDown {
		t.Fatalf("rejected response=%#v err=%v", rejected, err)
	}
	progress, err := commandClient.Execute(ctx, core.Request{CommandID: "test.status"})
	progressResult := progress.Result.(map[string]any)
	if err != nil || !progress.Success || progressResult["state"] != "shutting_down" {
		t.Fatalf("progress response=%#v err=%v", progress, err)
	}
}
