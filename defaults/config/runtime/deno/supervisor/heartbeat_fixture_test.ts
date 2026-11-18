import { assertEquals } from "../test/assert.ts";
import { Supervisor } from "./supervisor.ts";
import { postUnixHTTP } from "./unix_http.ts";

// Invoked by the Go callback integration test against its private Unix socket.
if (import.meta.main) {
  const [socket, workload] = Deno.args;
  if (!socket || (workload !== "service" && workload !== "job")) {
    throw new Error("socket and workload are required");
  }
  const token = "0123456789abcdef0123456789abcdef";
  const supervisor = new Supervisor({
    sandboxId: "sbx-aaaaaaaaaa",
    nodeId: "nod-0000000001",
    workloadType: workload,
    token,
    supervisorVersion: "1.0.0",
  });
  const send = async (
    path: string,
    body: unknown,
    expected = 200,
    bearer = token,
  ) => {
    const response = await postUnixHTTP(socket, path, bearer, body);
    assertEquals(response.status, expected);
    await response.text();
  };
  try {
    const registration = supervisor.registration();
    registration.payload!.future_snapshot = { metadata: true };
    await send("/v1/runtime/register", registration);
    await supervisor.startWorker({
      metadata: {
        sandboxId: supervisor.options.sandboxId,
        nodeId: supervisor.options.nodeId,
        workerId: "wrk-0000000001",
        workloadType: workload,
        ownerId: "fixture",
        workloadId: "fixture",
        releaseId: "fixture",
        debuggerName: "fixture",
        entrypoint: new URL(`../examples/${workload}.ts`, import.meta.url).href,
        databaseBackend: "sqlite",
        user: { userId: "user:system", username: "system" },
        origin: workload === "service"
          ? { type: "service", id: "fixture" }
          : { type: "module", id: "fixture" },
      },
      permissions: {
        read: [new URL("../examples/", import.meta.url).pathname],
      },
    });
    await supervisor.refreshResources();
    const heartbeat = supervisor.heartbeat();
    const resources = supervisor.workers()[0]?.resources;
    assertEquals(Number.isSafeInteger(resources?.cpu_usage_micros), true);
    assertEquals(Number.isSafeInteger(resources?.heap_used_bytes), true);
    assertEquals(typeof resources?.sampled_at, "string");
    heartbeat.payload!.future_snapshot = { metadata: true };
    const workers = heartbeat.payload!.workers as Array<
      Record<string, unknown>
    >;
    workers[0]!.future_worker = true;
    (workers[0]!.resources as Record<string, unknown>).future_resource = true;
    await send("/v1/runtime/heartbeat", heartbeat);
    await send("/v1/runtime/heartbeat", heartbeat);
    const stale = structuredClone(heartbeat);
    stale.payload!.supervisor_started_at_ms = supervisor.options.startedAt - 1;
    stale.payload!.revision = Number(stale.payload!.revision) + 1;
    stale.payload!.worker_count = 0;
    await send("/v1/runtime/heartbeat", stale);
    const invalid = structuredClone(heartbeat);
    invalid.payload!.worker_count = "private-invalid-value";
    await send("/v1/runtime/heartbeat", invalid, 400);
    await send("/v1/runtime/heartbeat", invalid, 401, "incorrect-token");
  } finally {
    await supervisor.stopWorker("wrk-0000000001", true);
  }
}
