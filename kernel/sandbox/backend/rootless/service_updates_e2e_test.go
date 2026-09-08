//go:build linux

package rootless

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/net/websocket"
	"the8020/kernel/execution"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/identity"
	"the8020/kernel/sandbox/model"
)

func verifyServiceSourceUpdate(t *testing.T, client *supervisor.Client, job, service model.SandboxSpec) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id := func(prefix string) string {
		value, err := identity.New(prefix)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	var root string
	for _, mount := range service.Mounts {
		if mount.Purpose == "workspace-packages" {
			root = filepath.Join(mount.Source, "acme", "updates")
		}
	}
	if root == "" {
		t.Fatal("missing shared source mount")
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	write := func(name, source string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(source), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("transitive.ts", `export const value = "before";`)
	write("direct.ts", `export { value } from "./transitive.ts";`)
	write("late.ts", `export const value = "late";`)
	write("gate.ts", `export let release: () => void; export const gate = new Promise<void>(resolve => release = resolve);`)
	write("job.ts", `import { value } from "./direct.ts"; import { gate, release } from "./gate.ts";
let calls = 0; export default async () => { calls++; await gate; return value + ":" + calls; };
export const workerFunctions = { "fixture.release": () => { release(); return true; } };`)
	write("service.ts", `import { defineService } from "@the8020/http";
import { value } from "./direct.ts"; import { gate, release } from "./gate.ts";
export const workerFunctions = { "fixture.release": () => { release(); return true; } };
export default defineService()
 .get("/", {}, () => new Response(value))
 .get("/late", {}, async () => new Response((await import("./late.ts")).value))
 .get("/hold", {}, () => new Response(new ReadableStream({ start(controller) {
   controller.enqueue(new TextEncoder().encode(value));
   void gate.then(() => { controller.enqueue(new TextEncoder().encode(value)); controller.close(); });
 } })))
 .websocket("/socket", async ({ socket }) => { socket.send(value); while (true) {
   const event = await socket.receive(); if (event.type === "close") return;
   socket.send(value + ":" + event.data);
 } });`)
	const sourceRoot = "/workspace/packages/acme/updates/"
	start := func(spec model.SandboxSpec, pool string, generation uint64) string {
		t.Helper()
		worker := id("wrk")
		entry, origin := "service.ts", execution.Origin{Type: "service", ID: "acme/updates/api"}
		var metadata *supervisor.ServiceExecutionMetadata
		if spec.WorkloadType == model.WorkloadJob {
			entry, origin = "job.ts", execution.Origin{Type: "module", ID: "acme/updates/job"}
		} else {
			metadata = &supervisor.ServiceExecutionMetadata{ServiceID: "acme/updates/api", Generation: generation, CanonicalBasePath: "/acme/updates/api", ExecutionMode: "persistent"}
		}
		_, err := client.StartWorker(ctx, spec, supervisor.StartWorkerRequest{Metadata: supervisor.ExecutionMetadata{
			WorkerID: worker, WorkloadType: spec.WorkloadType, WorkloadID: pool, OwnerID: origin.ID,
			ReleaseID: fmt.Sprint(generation), Entrypoint: "file://" + sourceRoot + entry, DebuggerName: worker,
			DatabaseBackend: "sqlite", DatabaseAccess: "none", User: execution.SystemUser(), Origin: origin, Service: metadata,
		}, Permissions: supervisor.WorkerPermissions{Read: spec.Permissions.ReadPaths}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.StopWorker(context.Background(), spec, worker, true) })
		return worker
	}
	header := func(worker, binding string, existing bool) http.Header {
		value := http.Header{}
		for name, text := range map[string]string{"context-id": id("ctx"), "target-worker-id": worker, "persistent-execution-id": binding, "persistent-keep-alive-ms": "0", "user-id": "user:system", "username": "system"} {
			value.Set("the8020-internal-"+name, text)
		}
		if existing {
			value.Set("the8020-internal-persistent-existing", "true")
		}
		return value
	}
	request := func(pool, worker, binding, path string, existing bool) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://service"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header = header(worker, binding, existing)
		response, err := client.DispatchService(ctx, service, pool, req)
		if err != nil {
			t.Fatalf("dispatch %s existing=%t: %v", path, existing, err)
		}
		t.Cleanup(func() { _ = response.Body.Close() })
		return response
	}
	read := func(response *http.Response, want string) {
		t.Helper()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != 200 || string(data) != want {
			t.Fatalf("response=%d %q error=%v", response.StatusCode, data, err)
		}
	}
	connect := func(pool, worker string) *websocket.Conn {
		t.Helper()
		config, err := websocket.NewConfig(fmt.Sprintf("ws://127.0.0.1:%d/v1/services/%s/websocket", service.Network.SupervisorPort, pool), "http://service")
		if err != nil {
			t.Fatal(err)
		}
		config.Header = header(worker, id("pex"), false)
		config.Header.Set("Authorization", "Bearer "+service.InternalToken)
		config.Header.Set("the8020-internal-url", "http://service/socket")
		socket, err := websocket.DialConfig(config)
		if err != nil {
			t.Fatal(err)
		}
		_ = socket.SetDeadline(time.Now().Add(15 * time.Second))
		t.Cleanup(func() { _ = socket.Close() })
		return socket
	}
	receive := func(socket *websocket.Conn, want string) {
		t.Helper()
		var data string
		err := websocket.Message.Receive(socket, &data)
		if err != nil || data != want {
			t.Fatalf("WebSocket=%q error=%v", data, err)
		}
	}
	release := func(spec model.SandboxSpec, worker string) {
		t.Helper()
		invocation, err := execution.NewInvocation("")
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.InvokeWorker(execution.WithInvocation(ctx, invocation), spec, worker, "", "fixture.release", nil, execution.SystemUser())
		if err != nil || !result.OK {
			t.Fatalf("release=%#v error=%v", result, err)
		}
	}
	oldPool, newPool := id("srv"), id("srv")
	old := start(service, oldPool, 1)
	if err := client.ConfigureService(ctx, service, oldPool, []string{old}, 8); err != nil {
		t.Fatal(err)
	}
	checkImports := func(name string, want bool) {
		t.Helper()
		matches, err := client.MatchingImports(ctx, service, []string{old}, []string{sourceRoot + name})
		if err != nil || slices.Contains(matches, old) != want {
			t.Fatalf("imports %s: %v error=%v", name, matches, err)
		}
	}
	for _, name := range []string{"service.ts", "direct.ts", "transitive.ts"} {
		checkImports(name, true)
	}
	checkImports("late.ts", false)
	binding := id("pex")
	read(request(oldPool, old, binding, "/late", false), "late")
	checkImports("late.ts", true)
	stream := request(oldPool, old, binding, "/hold", true)
	first := make([]byte, len("before"))
	if _, err := io.ReadFull(stream.Body, first); err != nil || string(first) != "before" {
		t.Fatalf("stream=%q error=%v", first, err)
	}
	oldSocket := connect(oldPool, old)
	receive(oldSocket, "before")
	jobWorker := start(job, "source-update-job", 1)
	invocation, err := execution.NewInvocation("")
	if err != nil {
		t.Fatal(err)
	}
	invocation.JobRunID = id("job")
	jobDone := make(chan error, 1)
	go func() {
		result, err := client.RunJob(execution.WithInvocation(ctx, invocation), job, jobWorker, nil, nil, nil)
		if err == nil && fmt.Sprint(result.Result) != "before:1" {
			err = fmt.Errorf("running job changed/replayed: %#v", result)
		}
		jobDone <- err
	}()
	for {
		live, err := client.Workers(ctx, job)
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(live, func(item supervisor.WorkerStatus) bool { return item.WorkerID == jobWorker && item.InFlight == 1 }) {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(5 * time.Millisecond)
	}
	write("transitive.ts", `export const value = "after";`)
	fresh := start(service, newPool, 2)
	if err := client.ConfigureService(ctx, service, newPool, []string{fresh}, 8); err != nil {
		t.Fatal(err)
	}
	if err := client.ConfigureService(ctx, service, oldPool, nil, 8); err != nil {
		t.Fatal(err)
	}
	read(request(newPool, fresh, id("pex"), "/", false), "after")
	read(request(oldPool, old, binding, "/", true), "before")
	if response := request(oldPool, old, id("pex"), "/", false); response.StatusCode != 503 {
		t.Fatalf("draining admission status=%d", response.StatusCode)
	}
	if err := websocket.Message.Send(oldSocket, "alive"); err != nil {
		t.Fatal(err)
	}
	receive(oldSocket, "before:alive")
	newSocket := connect(newPool, fresh)
	receive(newSocket, "after")
	release(service, old)
	read(stream, "before")
	active := request(newPool, fresh, id("pex"), "/hold", false)
	if _, err := io.ReadFull(active.Body, make([]byte, len("after"))); err != nil {
		t.Fatal(err)
	}
	for _, worker := range []string{old, fresh} {
		if err := client.StopWorker(ctx, service, worker, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, socket := range []*websocket.Conn{oldSocket, newSocket} {
		var text string
		if err := websocket.Message.Receive(socket, &text); err == nil {
			t.Fatal("hard stop retained WebSocket")
		}
	}
	if data, err := io.ReadAll(active.Body); err == nil {
		t.Fatalf("hard stop completed active stream: %q", data)
	}
	live, err := client.Workers(ctx, job)
	if err != nil || !slices.ContainsFunc(live, func(item supervisor.WorkerStatus) bool { return item.WorkerID == jobWorker && item.InFlight == 1 }) {
		t.Fatalf("job lost during restarts: %#v error=%v", live, err)
	}
	release(job, jobWorker)
	if err := <-jobDone; err != nil {
		t.Fatal(err)
	}
	if matches, err := client.MatchingImports(ctx, service, []string{old, fresh}, []string{sourceRoot + "transitive.ts"}); err != nil || len(matches) != 0 {
		t.Fatalf("terminated imports retained: %v error=%v", matches, err)
	}
}
