import { assertEquals, assertRejects } from "../test/assert.ts";
import { RuntimeWorker, WorkerExecutionError } from "./runtime_worker.ts";
import type {
  ExecutionMetadata,
  KernelCallRequest,
  ServiceRequestMetadata,
  WorkloadType,
} from "./contracts.ts";

const example = (name: string): string =>
  new URL(`../examples/${name}.ts`, import.meta.url).href;

function metadata(
  workloadType: WorkloadType,
  entrypoint: string,
  suffix: string = workloadType,
): ExecutionMetadata {
  return {
    nodeId: "node-test",
    runtimeGroupId: "rgp-test0001",
    sandboxId: "sbx-test0001",
    workerId: `worker-${suffix}`,
    executionId: `execution-${suffix}`,
    workloadType,
    ownerId: `owner-${suffix}`,
    workloadId: `workload-${suffix}`,
    user: { userId: "user:system", username: "system" },
    origin: { type: workloadType, id: `owner-${suffix}` },
    releaseId: "test",
    databaseBackend: "sqlite",
    entrypoint,
    debuggerName:
      `${workloadType}:owner-${suffix}:execution-${suffix}:worker-${suffix}`,
  };
}

Deno.test("job Worker loads ES module and supports compatible reuse", async () => {
  const worker = new RuntimeWorker({
    metadata: metadata("job", example("job")),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
  });
  try {
    assertEquals(await worker.runJob([{ value: 1 }]), {
      input: { value: 1 },
    });
    assertEquals(worker.logs.map((event) => event.message), [
      'job input {"value":1}',
    ]);
    assertEquals(await worker.runJob(["again"]), {
      input: "again",
    });
    assertEquals(worker.logs.map((event) => event.message), [
      "job input again",
    ]);
  } finally {
    await worker.stop();
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
});

Deno.test("job Worker exposes its immutable system execution context", async () => {
  const worker = new RuntimeWorker({
    metadata: metadata("job", example("job_context"), "context"),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
  });
  try {
    const value = await worker.runJob([]) as Record<string, unknown>;
    assertEquals(value.type, "job");
    assertEquals(value.id, "owner-context");
    assertEquals(value.userId, "user:system");
    assertEquals(value.username, "system");
    assertEquals(value.nodeId, "node-test");
    assertEquals(value.runtimeGroupId, "rgp-test0001");
    assertEquals(value.sandboxId, "sbx-test0001");
    assertEquals(value.workerId, "worker-context");
    assertEquals(value.executionId, "execution-context");
    assertEquals(typeof value.requestId, "string");
  } finally {
    await worker.stop();
  }
});

Deno.test("hook dispatcher runs an ordered shared-state chain in one ordinary reusable job Worker", async () => {
  const worker = new RuntimeWorker({
    metadata: metadata(
      "job",
      new URL("./hook_dispatch.ts", import.meta.url).href,
      "hooks",
    ),
    permissions: { read: [new URL("..", import.meta.url).pathname] },
  });
  const handlers = ["build", "enhance", "filter"].map((name) => ({
    id: `acme/${name}/hooks/index.toml`,
    entrypoint: new URL(`./testdata/hook_${name}.ts`, import.meta.url).href,
  }));
  const run = (value: number, fail = false) =>
    worker.runJob([
      handlers,
      { package_id: "acme/service" },
      { trace: [], workers: [], value, fail },
    ]) as Promise<Record<string, unknown>>;
  try {
    for (const initial of [2, 5]) {
      const result = await run(initial);
      assertEquals(result.trace, ["build", "enhance", "filter"]);
      assertEquals(result.workers, [
        "worker-hooks",
        "worker-hooks",
        "worker-hooks",
      ]);
      assertEquals(result.value, (initial + 1) * 3 - 1);
      assertEquals(result.packageId, "acme/service");
      assertEquals(result.scopeFrozen, true);
      assertEquals(result.user, "user:system");
    }
    await assertRejects(
      () => run(0, true),
      Error,
      "hook acme/enhance/hooks/index.toml failed: enhancement failed",
    );
    assertEquals(
      worker.logs.some((log) => log.message === "filter ran"),
      false,
    );
    assertEquals((await run(0)).value, 2);
  } finally {
    await worker.stop();
  }
});

Deno.test("program job exposes the logical program origin", async () => {
  const programMetadata = metadata(
    "job",
    example("job_context"),
    "program-context",
  );
  programMetadata.origin = {
    type: "program",
    id: "the8020/example/program",
  };
  const worker = new RuntimeWorker({
    metadata: programMetadata,
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
  });
  try {
    const value = await worker.runJob([]) as Record<string, unknown>;
    assertEquals(value.type, "program");
    assertEquals(value.id, "the8020/example/program");
    assertEquals(value.username, "system");
  } finally {
    await worker.stop();
  }
});

Deno.test("job Worker spreads arguments into only the default export", async () => {
  const spread = new RuntimeWorker({
    metadata: metadata("job", example("job_spread"), "spread"),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
  });
  try {
    assertEquals(await spread.runJob(["Alice Smith", "--admin"]), [
      "Alice Smith",
      "--admin",
    ]);
  } finally {
    await spread.stop();
  }

  const runOnly = new RuntimeWorker({
    metadata: metadata("job", example("job_run_only"), "run-only"),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
  });
  try {
    await assertRejects(
      () => runOnly.ready,
      Error,
      "default-export",
    );
  } finally {
    await runOnly.stop();
  }
});

Deno.test("job Worker preserves structured command failures", async () => {
  const worker = new RuntimeWorker({
    metadata: metadata("job", example("job_error"), "error"),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
  });
  try {
    try {
      await worker.runJob([]);
      throw new Error("structured job unexpectedly succeeded");
    } catch (error) {
      assertEquals(error instanceof WorkerExecutionError, true);
      assertEquals(
        (error as WorkerExecutionError).message,
        "structured job failure",
      );
      assertEquals((error as WorkerExecutionError).code, "invalid_arguments");
      assertEquals((error as WorkerExecutionError).details, {
        field: "example",
      });
    }
  } finally {
    worker.kill();
  }
});

Deno.test("database-free jobs do not open or close database scopes", async () => {
  const jobMetadata = metadata("job", example("job"), "database-free");
  jobMetadata.databaseAccess = "none";
  const calls: KernelCallRequest[] = [];
  const worker = new RuntimeWorker({
    metadata: jobMetadata,
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
    kernelCall: (call) => {
      calls.push(call);
      return Promise.resolve({});
    },
  });
  try {
    assertEquals(await worker.runJob(["input"]), { input: "input" });
    assertEquals(calls, []);
  } finally {
    await worker.stop();
  }
});

Deno.test("jobs use one invocation-scoped database context", async () => {
  const calls: KernelCallRequest[] = [];
  const worker = new RuntimeWorker({
    metadata: metadata("job", example("job_database"), "job-database"),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
    kernelCall: (call) => {
      calls.push(call);
      if (call.operation === "database.scope.close") {
        return Promise.resolve({ closed: true });
      }
      return Promise.resolve({ columns: ["value"], rows: [[11]] });
    },
  });
  try {
    assertEquals(await worker.runJob([11]), {
      columns: ["value"],
      rows: [[11]],
    });
    const statement = calls.find((call) =>
      call.operation === "database.execute"
    );
    const cleanup = calls.find((call) =>
      call.operation === "database.scope.close"
    );
    assertEquals(statement?.requestId, cleanup?.requestId);
    assertEquals(statement?.executionId, worker.metadata.executionId);
    assertEquals(statement?.workerId, worker.metadata.workerId);
    assertEquals(statement?.serviceId, worker.metadata.workloadId);
  } finally {
    await worker.stop();
  }
});

Deno.test("job secure inputs are isolated and cleared after failures", async () => {
  const first = new RuntimeWorker({
    metadata: metadata("job", example("job_secret"), "secret-first"),
    permissions: { read: [new URL("..", import.meta.url).pathname] },
  });
  const second = new RuntimeWorker({
    metadata: metadata("job", example("job_secret"), "secret-second"),
    permissions: { read: [new URL("..", import.meta.url).pathname] },
  });
  try {
    const values = await Promise.all([
      first.runJob(["password"], { password: "first-private-value" }),
      second.runJob(["password"], { password: "second-private-value" }),
    ]);
    assertEquals(values, ["first-private-value", "second-private-value"]);
    await assertRejects(
      () => first.runJob(["password", true], { password: "never-leak-this" }),
      Error,
      "deliberate job failure",
    );
    assertEquals(await first.runJob(["password"]), "missing");
    assertEquals(JSON.stringify(first.logs).includes("never-leak-this"), false);
  } finally {
    await Promise.all([first.stop(), second.stop()]);
  }
});

Deno.test("service Worker transfers request and response streams", async () => {
  const worker = new RuntimeWorker({
    metadata: metadata("service", example("service")),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
  });
  try {
    const response = await worker.dispatchService(
      new Request("http://service.test/large", {
        method: "POST",
        body: new ReadableStream({
          start(controller) {
            controller.enqueue(new TextEncoder().encode("upload-"));
            controller.enqueue(new TextEncoder().encode("stream"));
            controller.close();
          },
        }),
      }),
    );
    assertEquals(response.status, 201);
    assertEquals(await response.text(), "POST:/large:streamed:upload-stream");
  } finally {
    await worker.stop();
  }
});

Deno.test("service Worker exposes the exact request user", async () => {
  const workerMetadata = metadata(
    "service",
    example("service_context"),
    "service-context",
  );
  const worker = new RuntimeWorker({
    metadata: workerMetadata,
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
  });
  try {
    const response = await worker.dispatchService(
      new Request("http://service.test/context"),
      {
        requestId: "request-alice",
        serviceId: "example/context/service",
        serviceGeneration: 2,
        canonicalBasePath: "/example/context/service",
        originalUrl: "https://example.test/example/context/service",
        client: { ipAddress: "203.0.113.4", networkScope: "public" },
        execution: {
          nodeId: workerMetadata.nodeId,
          runtimeGroupId: workerMetadata.runtimeGroupId,
          sandboxId: workerMetadata.sandboxId,
          workerId: workerMetadata.workerId,
          workerExecutionId: workerMetadata.executionId,
        },
        user: { userId: "user:alice", username: "alice" },
        auth: {
          authenticated: true,
          realm: "user",
          userId: "user:alice",
          username: "alice",
        },
      },
    );
    const value = await response.json();
    assertEquals(value.type, "service");
    assertEquals(value.id, "owner-service-context");
    assertEquals(value.userId, "user:alice");
    assertEquals(value.username, "alice");
    assertEquals(value.requestId, "request-alice");
    assertEquals(value.sandboxId, "sbx-test0001");
    assertEquals(value.workerId, "worker-service-context");
  } finally {
    await worker.stop();
  }
});

Deno.test("jobs and services both have unrestricted outbound network access", async () => {
  const external = Deno.serve({
    hostname: "127.0.0.1",
    port: 0,
    onListen() {},
  }, () => new Response("external-api"));
  const address = external.addr as Deno.NetAddr;
  const target = `http://127.0.0.1:${address.port}/value`;
  const permissions = {
    read: [new URL("../examples", import.meta.url).pathname],
    net: true as const,
    import: true as const,
  };
  const job = new RuntimeWorker({
    metadata: metadata("job", example("job_network"), "network-job"),
    permissions,
  });
  const service = new RuntimeWorker({
    metadata: metadata(
      "service",
      example("service_network"),
      "network-service",
    ),
    permissions,
  });
  try {
    assertEquals(await job.runJob([target]), "external-api");
    const response = await service.dispatchService(
      new Request(`http://service.test/?target=${encodeURIComponent(target)}`),
    );
    assertEquals(response.status, 200);
    assertEquals(await response.text(), "external-api");
  } finally {
    await Promise.all([job.stop(), service.stop()]);
    await external.shutdown();
  }
});

Deno.test("stateless service Worker bridges WebSocket routes without buffering messages", async () => {
  const workerMetadata = metadata(
    "service",
    example("service_websocket"),
    "websocket",
  );
  workerMetadata.service = {
    serviceId: "example/websocket/service",
    generation: 4,
    canonicalBasePath: "/example/websocket/service",
    executionMode: "stateless",
  };
  const worker = new RuntimeWorker({
    metadata: workerMetadata,
    permissions: {
      read: [new URL("..", import.meta.url).pathname],
      import: ["jsr.io:443"],
    },
  });
  const sent: Array<string | Uint8Array> = [];
  const closes: Array<{ code: number; reason: string }> = [];
  const requestMetadata: ServiceRequestMetadata = {
    requestId: "request-websocket-1",
    serviceId: "example/websocket/service",
    serviceGeneration: 4,
    canonicalBasePath: "/example/websocket/service",
    originalUrl: "https://example.test/example/websocket/service/echo/main",
    client: { ipAddress: "203.0.113.4", networkScope: "public" },
    execution: {
      nodeId: workerMetadata.nodeId,
      runtimeGroupId: workerMetadata.runtimeGroupId,
      sandboxId: workerMetadata.sandboxId,
      workerId: workerMetadata.workerId,
      workerExecutionId: workerMetadata.executionId,
    },
    user: { userId: "user:admin", username: "admin" },
    auth: {
      authenticated: true,
      realm: "user",
      userId: "user:Admin",
      username: "Admin",
    },
  };
  try {
    const opened = await worker.openServiceWebSocket(
      new Request("http://service/echo/main"),
      requestMetadata,
      "the8020.echo",
      {
        send: (data) => sent.push(data),
        close: (code, reason) => closes.push({ code, reason }),
      },
    );
    assertEquals(opened.accepted, true);
    if (!opened.accepted) throw new Error("WebSocket route was rejected");
    await waitFor(() => sent.length === 1);
    assertEquals(
      sent[0],
      "ready:main:request-websocket-1:the8020.echo",
    );
    assertEquals(worker.inFlight, 1);

    opened.connection.send("hello");
    opened.connection.send(new Uint8Array([4, 5, 6]));
    await waitFor(() => sent.length === 3);
    assertEquals(sent[1], "echo:hello");
    assertEquals(sent[2], new Uint8Array([4, 5, 6]));
    assertEquals(closes, []);

    opened.connection.close(1000, "test complete");
    await waitFor(() => worker.inFlight === 0);

    const missing = await worker.openServiceWebSocket(
      new Request("http://service/missing"),
      { ...requestMetadata, requestId: "request-websocket-missing" },
      "",
      { send() {}, close() {} },
    );
    assertEquals(missing.accepted, false);
    if (!missing.accepted) assertEquals(missing.status, 404);
    assertEquals(worker.inFlight, 0);
  } finally {
    await worker.stop();
  }
});

async function waitFor(predicate: () => boolean): Promise<void> {
  const deadline = Date.now() + 2_000;
  while (!predicate()) {
    if (Date.now() >= deadline) {
      throw new Error("timed out waiting for Worker output");
    }
    await new Promise((resolve) => setTimeout(resolve, 1));
  }
}

Deno.test("service Worker bridges signing, verification, admin, and database calls", async () => {
  const calls: KernelCallRequest[] = [];
  const worker = new RuntimeWorker({
    metadata: metadata("service", example("service_kernel"), "kernel"),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
    kernelCall: (call) => {
      calls.push(call);
      if (call.operation === "runtime.operation") {
        return Promise.resolve({
          success: true,
          result: call.arguments.operation === "crypto.token.sign"
            ? { token: "signed-token" }
            : null,
        });
      }
      if (call.operation === "admin.execute") {
        return Promise.resolve({
          protocol_version: 2,
          success: true,
          result: { ready: true },
        });
      }
      if (call.operation === "database.execute") {
        return call.arguments.return_rows === true
          ? Promise.resolve({ columns: ["value"], rows: [[7]] })
          : Promise.resolve({ columns: [], rows: [], affected_rows: 1 });
      }
      return Promise.resolve({ setCookie: "the8020_auth=; Max-Age=0" });
    },
  });
  const requestMetadata: ServiceRequestMetadata = {
    requestId: "request-auth-1",
    serviceId: "example/auth/login",
    serviceGeneration: 1,
    canonicalBasePath: "/example/auth/login",
    originalUrl: "https://example.test/example/auth/login",
    client: { ipAddress: "203.0.113.4", networkScope: "public" },
    execution: {
      nodeId: worker.metadata.nodeId,
      runtimeGroupId: worker.metadata.runtimeGroupId,
      sandboxId: worker.metadata.sandboxId,
      workerId: worker.metadata.workerId,
      workerExecutionId: worker.metadata.executionId,
    },
    user: { userId: "user:system", username: "system" },
    auth: { authenticated: false },
  };
  try {
    const login = await worker.dispatchService(
      new Request("http://service/sign", {
        method: "POST",
        body: JSON.stringify({ sub: "user:alice" }),
      }),
      requestMetadata,
    );
    assertEquals(await login.json(), "signed-token");
    assertEquals(calls[0], {
      operation: "runtime.operation",
      arguments: {
        operation: "crypto.token.sign",
        input: { claims: { sub: "user:alice" } },
      },
      requestId: "request-auth-1",
      serviceId: "example/auth/login",
      executionId: "execution-kernel",
      workerId: "worker-kernel",
      persistentExecutionId: undefined,
      user: { userId: "user:system", username: "system" },
    });
    await worker.dispatchService(
      new Request("http://service/verify"),
      { ...requestMetadata, requestId: "request-auth-2" },
    );
    const applicationCalls = () =>
      calls.filter((call) => call.operation !== "database.scope.close");
    assertEquals(applicationCalls()[1]?.operation, "runtime.operation");
    assertEquals(applicationCalls()[1]?.requestId, "request-auth-2");
    const admin = await worker.dispatchService(
      new Request("http://service/admin"),
      { ...requestMetadata, requestId: "request-admin-1" },
    );
    assertEquals(await admin.json(), { ready: true });
    assertEquals(applicationCalls()[2]?.operation, "admin.execute");
    assertEquals(applicationCalls()[2]?.arguments, {
      command_id: "kernel.status",
      arguments: {},
    });
    const query = await worker.dispatchService(
      new Request("http://service/database-query"),
      { ...requestMetadata, requestId: "request-database-query" },
    );
    assertEquals(await query.json(), {
      columns: ["value"],
      rows: [[7]],
    });
    assertEquals(applicationCalls()[3]?.operation, "database.execute");
    const execute = await worker.dispatchService(
      new Request("http://service/database-execute"),
      { ...requestMetadata, requestId: "request-database-execute" },
    );
    assertEquals(await execute.json(), {
      columns: [],
      rows: [],
      affected_rows: 1,
    });
    assertEquals(applicationCalls()[4]?.operation, "database.execute");
    const streamed = await worker.dispatchService(
      new Request("http://service/database-stream"),
      { ...requestMetadata, requestId: "request-database-stream" },
    );
    assertEquals(await streamed.json(), {
      columns: ["value"],
      rows: [[7]],
    });
    assertEquals(applicationCalls()[5]?.operation, "database.execute");
    assertEquals(applicationCalls()[5]?.requestId, "request-database-stream");
    assertEquals(
      calls.filter((call) => call.operation === "database.scope.close").map(
        (call) => call.requestId,
      ),
      [
        "request-auth-1",
        "request-auth-2",
        "request-admin-1",
        "request-database-query",
        "request-database-execute",
        "request-database-stream",
      ],
    );
  } finally {
    await worker.stop();
  }
});

Deno.test("persistent control calls use the canonical service identity", async () => {
  const calls: KernelCallRequest[] = [];
  const serviceMetadata = metadata(
    "service",
    example("service_control"),
    "persistent-control",
  );
  serviceMetadata.service = {
    serviceId: "example/control/service",
    generation: 1,
    canonicalBasePath: "/example/control/service",
  };
  const worker = new RuntimeWorker({
    metadata: serviceMetadata,
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
    kernelCall: (call) => {
      calls.push(call);
      return Promise.resolve({ completed: true });
    },
  });
  try {
    await worker.ready;
    assertEquals(
      await worker.invoke(
        "example.complete-persistent",
        {},
        undefined,
        "persistent-control",
        { userId: "user:system", username: "system" },
      ),
      { ok: true, output: { completed: true } },
    );
    const completion = calls.find((call) =>
      call.operation === "execution.completePersistent"
    );
    assertEquals(completion?.serviceId, "example/control/service");
    assertEquals(completion?.persistentExecutionId, "persistent-control");
  } finally {
    await worker.stop();
  }
});

Deno.test("service Worker reads database info during module initialization", async () => {
  const calls: KernelCallRequest[] = [];
  const worker = new RuntimeWorker({
    metadata: metadata("service", example("service_database_info"), "db-info"),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
    kernelCall: (call) => {
      calls.push(call);
      return Promise.resolve({
        backend: "sqlite",
        location: "/database/system.db",
        state: "READY",
        initialized: true,
        catalog_version: 1,
      });
    },
  });
  try {
    await worker.ready;
    assertEquals(calls.length, 1);
    assertEquals(calls[0]?.operation, "database.info");
    assertEquals(calls[0]?.requestId, undefined);
    assertEquals(calls[0]?.serviceId, "workload-db-info");
    const response = await worker.dispatchService(
      new Request("http://service/database-info"),
    );
    assertEquals((await response.json()).backend, "sqlite");
    assertEquals(
      calls.filter((call) => call.operation === "database.info").length,
      1,
    );
  } finally {
    await worker.stop();
  }
});

Deno.test("service Worker preserves SSE streaming", async () => {
  const worker = new RuntimeWorker({
    metadata: metadata("service", example("service"), "sse"),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
  });
  try {
    const response = await worker.dispatchService(
      new Request("http://service.test/sse"),
    );
    assertEquals(response.headers.get("content-type"), "text/event-stream");
    assertEquals(await response.text(), "event: ready\ndata: streamed\n\n");
  } finally {
    worker.kill();
  }
});

Deno.test("graceful Worker stop drains the complete response stream", async () => {
  const worker = new RuntimeWorker({
    metadata: metadata("service", example("service"), "drain"),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
  });
  const response = await worker.dispatchService(
    new Request("http://service.test/drain"),
  );
  assertEquals(worker.inFlight, 1);
  let stopped = false;
  const stopping = worker.stop(250).then(() => {
    stopped = true;
  });
  await new Promise((resolve) => setTimeout(resolve, 10));
  assertEquals(worker.draining, true);
  assertEquals(stopped, false);
  assertEquals(await response.text(), "GET:/drain:streamed:");
  await stopping;
  assertEquals(worker.inFlight, 0);
  assertEquals(worker.closed, true);
});

Deno.test("service request cancellation reaches the program Worker", async () => {
  const worker = new RuntimeWorker({
    metadata: metadata("service", example("service"), "cancel"),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
  });
  try {
    const controller = new AbortController();
    const pending = worker.dispatchService(
      new Request("http://service.test/wait", { signal: controller.signal }),
    );
    await Promise.resolve();
    controller.abort(new DOMException("test cancellation", "AbortError"));
    await assertRejects(() => pending, Error, "test cancellation");
    assertEquals(worker.inFlight, 0);
  } finally {
    worker.kill();
  }
});

Deno.test("service request cancellation reaches an in-flight kernel call", async () => {
  let started!: () => void;
  const entered = new Promise<void>((resolve) => started = resolve);
  let kernelSignal: AbortSignal | undefined;
  const worker = new RuntimeWorker({
    metadata: metadata("service", example("service_kernel"), "db-cancel"),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
    kernelCall: (call, signal) => {
      if (call.operation === "database.scope.close") {
        return Promise.resolve({ closed: true });
      }
      kernelSignal = signal;
      started();
      return new Promise((_resolve, reject) => {
        signal?.addEventListener(
          "abort",
          () => reject(signal.reason),
          { once: true },
        );
      });
    },
  });
  try {
    const controller = new AbortController();
    const pending = worker.dispatchService(
      new Request("http://service/database-query", {
        signal: controller.signal,
      }),
      {
        requestId: "request-database-cancel",
        serviceId: "example/database/service",
        serviceGeneration: 1,
        canonicalBasePath: "/example/database/service",
        originalUrl: "http://service/database-query",
        client: { ipAddress: "127.0.0.1", networkScope: "loopback" },
        execution: {
          nodeId: worker.metadata.nodeId,
          runtimeGroupId: worker.metadata.runtimeGroupId,
          sandboxId: worker.metadata.sandboxId,
          workerId: worker.metadata.workerId,
          workerExecutionId: worker.metadata.executionId,
        },
        user: { userId: "user:system", username: "system" },
        auth: { authenticated: false },
      },
    );
    await entered;
    controller.abort(new DOMException("client left", "AbortError"));
    await assertRejects(() => pending, Error, "client left");
    await waitFor(() => kernelSignal?.aborted === true);
    assertEquals(kernelSignal?.aborted, true);
  } finally {
    worker.kill();
  }
});

Deno.test("Worker permissions deny undeclared host reads", async () => {
  const worker = new RuntimeWorker({
    metadata: metadata("job", example("denied"), "denied"),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
  });
  try {
    await assertRejects(
      () => worker.runJob([]),
      Error,
      "Requires read access",
    );
  } finally {
    worker.kill();
  }
});

Deno.test("application Worker cannot read the internal token or Unix socket", async () => {
  const worker = new RuntimeWorker({
    metadata: metadata("job", example("job_internal_access"), "internal"),
    permissions: {
      read: [new URL("../examples", import.meta.url).pathname],
      net: true,
    },
  });
  try {
    assertEquals(await worker.runJob([]), {
      token: "NotCapable",
      socket: "NotCapable",
    });
  } finally {
    worker.kill();
  }
});

Deno.test("nested Workers remain available within the parent envelope", async () => {
  const worker = new RuntimeWorker({
    metadata: metadata("job", example("nested"), "nested"),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
  });
  try {
    assertEquals(await worker.runJob([]), "nested-ok");
  } finally {
    worker.kill();
  }
});

Deno.test("target Worker authenticates before HTTP and WebSocket handlers", async () => {
  let allowed = false;
  const calls: KernelCallRequest[] = [];
  const worker = new RuntimeWorker({
    metadata: {
      ...metadata(
        "service",
        new URL("./testdata/authenticated_service.ts", import.meta.url).href,
        "auth-policy",
      ),
      service: {
        serviceId: "example/auth/service",
        generation: 1,
        canonicalBasePath: "/example/auth/service",
        executionMode: "stateless",
      },
    },
    permissions: {
      read: [
        new URL("./testdata", import.meta.url).pathname,
        new URL("../http", import.meta.url).pathname,
      ],
      net: true,
      import: true,
    },
    kernelCall: (call) => {
      calls.push(call);
      return Promise.resolve({ columns: ["allowed"], rows: [[allowed]] });
    },
  });
  const meta: ServiceRequestMetadata = {
    requestId: "policy-request",
    serviceId: "example/auth/service",
    serviceGeneration: 1,
    canonicalBasePath: "/example/auth/service",
    originalUrl: "https://example.test/",
    client: { ipAddress: "127.0.0.1", networkScope: "loopback" },
    execution: {
      nodeId: worker.metadata.nodeId,
      runtimeGroupId: worker.metadata.runtimeGroupId,
      sandboxId: worker.metadata.sandboxId,
      workerId: worker.metadata.workerId,
      workerExecutionId: worker.metadata.executionId,
    },
    user: { userId: "user:alice", username: "alice" },
    auth: { authenticated: false },
    authentication: {
      module: new URL("./testdata/authentication.ts", import.meta.url).href,
      claims: { sub: "user:alice" },
      unauthenticated: { action: "reject", status: 401 },
    },
  };
  try {
    for (const cookie of ["", "valid", "invalid", "expired"]) {
      const response = await worker.dispatchService(
        new Request("https://service/", {
          headers: { cookie: `the8020_auth=${cookie}` },
        }),
        { ...meta, authentication: undefined, user: worker.metadata.user },
      );
      const result = await response.json();
      assertEquals(result.username, "system");
      assertEquals(result.authenticated, false);
    }
    assertEquals(
      calls.filter((call) => call.operation === "database.execute").length,
      0,
    );
    const rejected = await worker.dispatchService(
      new Request("https://service/"),
      meta,
    );
    assertEquals(rejected.status, 401);
    assertEquals(await rejected.text(), "Denied");
    const rejectedSocket = await worker.openServiceWebSocket(
      new Request("https://service/"),
      meta,
      "test",
      { send: () => {}, close: () => {} },
    );
    assertEquals(rejectedSocket.accepted, false);
    allowed = true;
    const accepted = await worker.dispatchService(
      new Request("https://service/"),
      meta,
    );
    const identity = await accepted.json();
    assertEquals(identity.username, "alice");
    assertEquals(identity.authenticated, true);
    const sent: Array<string | Uint8Array> = [];
    const opened = await worker.openServiceWebSocket(
      new Request("https://service/"),
      meta,
      "test",
      { send: (data) => sent.push(data), close: () => {} },
    );
    assertEquals(opened.accepted, true);
    if (!opened.accepted) throw new Error("WebSocket rejected");
    await waitFor(() => sent.length === 1);
    assertEquals(JSON.parse(sent[0] as string).authenticated, true);
    opened.connection.send("check context");
    await waitFor(() => sent.length === 2);
    assertEquals(JSON.parse(sent[1] as string).username, "alice");
    assertEquals(JSON.parse(sent[1] as string).authenticated, true);
    for (
      const call of calls.filter((call) =>
        call.operation === "database.execute"
      )
    ) {
      assertEquals(call.user, { userId: "user:alice", username: "alice" });
      assertEquals(call.requestId, "policy-request");
    }
  } finally {
    await worker.stop();
  }
});
