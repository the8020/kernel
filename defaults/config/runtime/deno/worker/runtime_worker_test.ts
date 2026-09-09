import { isId, newId } from "../identity/mod.ts";
import { assertEquals, assertRejects } from "../test/assert.ts";
import { RuntimeWorker, WorkerExecutionError } from "./runtime_worker.ts";
import { TestLogSink } from "../test/logs.ts";
import { canonicalExecutionOrigin } from "./contracts.ts";
import type {
  ExecutionMetadata,
  KernelCallRequest,
  ServiceRequestMetadata,
  WorkloadType,
} from "./contracts.ts";

const example = (name: string): string =>
  new URL(`../examples/${name}.ts`, import.meta.url).href;

Deno.test("Worker records only loaded direct, transitive and late dynamic imports", async () => {
  const root = new URL("./testdata/", import.meta.url);
  const worker = new RuntimeWorker({
    metadata: metadata("job", new URL("imports_entry.ts", root).href),
    permissions: { read: [root.pathname] },
  });
  const observed = (name: string): boolean =>
    worker.importsAny(
      new Set([
        Deno.realPathSync(new URL(`imports_${name}.ts`, root)),
      ]),
    );
  try {
    assertEquals(await worker.runJob(testInvocation(), []), "static");
    for (const name of ["entry", "direct", "transitive"]) {
      assertEquals(observed(name), true);
    }
    assertEquals(observed("dynamic"), false);
    const directory = Deno.realPathSync(root);
    assertEquals(worker.importsAny(new Set([directory + "/"])), true);
    assertEquals(worker.importsAny(new Set([directory])), false);
    assertEquals(worker.importsAny(new Set([directory + "-other/"])), false);
    assertEquals(await worker.runJob(testInvocation(), [true]), "dynamic");
    assertEquals(observed("dynamic"), true);
    assertEquals(await worker.runJob(testInvocation(), [true]), "dynamic");
  } finally {
    worker.kill();
  }
  assertEquals(observed("entry"), false);
});

function metadata(
  workloadType: WorkloadType,
  entrypoint: string,
  suffix: string = workloadType,
): ExecutionMetadata {
  const workerId = newId("wrk");
  return {
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000001",
    workerId,

    workloadType,
    ownerId: `owner-${suffix}`,
    workloadId: `workload-${suffix}`,
    user: { userId: "user:system", username: "system" },
    origin: {
      type: workloadType === "service" ? "service" : "module",
      id: `owner-${suffix}`,
    },
    releaseId: "test",
    databaseBackend: "sqlite",
    entrypoint,
    debuggerName: `${workloadType}:owner-${suffix}:${workerId}`,
  };
}

Deno.test("job Worker loads ES module and supports compatible reuse", async () => {
  const logs = new TestLogSink();
  const worker = new RuntimeWorker({
    logSink: logs,
    metadata: metadata("job", example("job")),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
  });
  try {
    assertEquals(await worker.runJob(testInvocation(), [{ value: 1 }]), {
      input: { value: 1 },
    });
    assertEquals(
      logs.records.filter((event) => event.component === "worker").map((
        event,
      ) => event.message),
      [
        'job input {"value":1}',
      ],
    );
    assertEquals(await worker.runJob(testInvocation(), ["again"]), {
      input: "again",
    });
    assertEquals(
      logs.records.filter((event) => event.component === "worker").map((
        event,
      ) => event.message),
      [
        'job input {"value":1}',
        "job input again",
      ],
    );
  } finally {
    await worker.stop();
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
  worker.kill();
  const lifecycle = logs.records.filter((record) =>
    record.component === "worker-lifecycle"
  );
  assertEquals(lifecycle.map((record) => record.message), [
    "Worker started",
    "Worker ready",
    "Worker exited",
  ]);
  assertEquals(lifecycle.at(-1)!.attributes?.reason, "graceful");
  assertEquals(
    lifecycle.every((record) =>
      record.worker_id === worker.metadata.workerId &&
      record.context_id === undefined && record.username === undefined
    ),
    true,
  );
});

Deno.test("Worker startup failure and forced cancellation emit one terminal lifecycle record", async () => {
  for (const mode of ["failure", "forced", "drain_timeout"] as const) {
    const logs = new TestLogSink();
    const worker = new RuntimeWorker({
      logSink: logs,
      metadata: metadata(
        "service",
        example(mode === "failure" ? "missing-lifecycle-fixture" : "service"),
      ),
      permissions: { read: [new URL("../examples", import.meta.url).pathname] },
    });
    try {
      if (mode === "failure") {
        await assertRejects(
          () => worker.ready,
          Error,
          "missing-lifecycle-fixture",
        );
      } else {
        const response = await worker.dispatchService(
          new Request("http://service.test/drain"),
        );
        assertEquals(worker.inFlight, 1);
        if (mode === "forced") worker.kill();
        else await worker.stop(1);
        await response.body?.cancel().catch(() => {});
      }
    } finally {
      worker.kill();
    }
    const terminal = logs.records.filter((record) =>
      record.message === "Worker exited"
    );
    assertEquals(terminal.length, 1);
    assertEquals(terminal[0]!.attributes?.reason, mode);
    assertEquals(
      terminal[0]!.attributes?.phase,
      mode === "failure" ? "starting" : "running",
    );
    assertEquals(
      terminal[0]!.attributes?.in_flight,
      mode === "failure" ? "0" : "1",
    );
    assertEquals(terminal[0]!.level, mode === "failure" ? "ERROR" : "WARN");
    assertEquals(worker.closed, true);
    assertEquals(worker.inFlight, 0);
  }
});

Deno.test("direct module exposes its immutable system execution context", async () => {
  await assertRejects(
    () =>
      Promise.resolve(
        canonicalExecutionOrigin({ type: "job", id: "owner-context" }, "job"),
      ),
    TypeError,
    "execution origin is invalid",
  );
  const worker = new RuntimeWorker({
    metadata: metadata("job", example("job_context"), "context"),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
  });
  try {
    const firstInvocation = {
      ...testInvocation(),
      parentContextId: newId("ctx"),
    };
    const first = worker.runJob(firstInvocation, []);
    await assertRejects(
      () => worker.runJob(firstInvocation, []),
      Error,
      "already active",
    );
    const value = await first as Record<string, unknown>;
    const nextInvocation = testInvocation();
    const next = await worker.runJob(nextInvocation, []) as Record<
      string,
      unknown
    >;
    assertEquals(value.contextId, firstInvocation.contextId);
    assertEquals(value.parentContextId, firstInvocation.parentContextId);
    assertEquals(value.jobRunId, firstInvocation.jobRunId);
    assertEquals(next.contextId, nextInvocation.contextId);
    assertEquals(next.jobRunId, nextInvocation.jobRunId);
    assertEquals(next.workerId, value.workerId);
    assertEquals(next.contextId === value.contextId, false);
    assertEquals(next.parentContextId, undefined);
    assertEquals(value.type, "module");
    assertEquals(next.type, "module");
    assertEquals(value.id, "owner-context");
    assertEquals(value.userId, "user:system");
    assertEquals(value.username, "system");
    assertEquals(value.nodeId, worker.metadata.nodeId);
    assertEquals(value.sandboxId, worker.metadata.sandboxId);
    assertEquals(value.workerId, worker.metadata.workerId);
    assertEquals(isId(value.contextId, "ctx"), true);
    assertEquals(isId(value.jobRunId, "job"), true);
    assertEquals(typeof value.contextId, "string");
  } finally {
    await worker.stop();
  }
});

Deno.test("hook dispatcher runs an ordered shared-state chain in one ordinary reusable job Worker", async () => {
  const logs = new TestLogSink();
  const worker = new RuntimeWorker({
    logSink: logs,
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
    worker.runJob(testInvocation(), [
      handlers,
      {
        packages: [{ package_id: "acme/service" }, {
          package_id: "acme/other",
        }],
      },
      { trace: [], workers: [], value, fail },
    ]) as Promise<Record<string, unknown>>;
  try {
    for (const initial of [2, 5]) {
      const result = await run(initial);
      assertEquals(result.trace, ["build", "enhance", "filter"]);
      assertEquals(result.workers, [
        worker.metadata.workerId,
        worker.metadata.workerId,
        worker.metadata.workerId,
      ]);
      assertEquals(result.value, (initial + 1) * 3 - 1);
      assertEquals(result.packageId, "acme/service");
      assertEquals(result.scopeFrozen, true);
      assertEquals(result.user, "user:system");
    }
    logs.records.length = 0;
    await assertRejects(
      () => run(0, true),
      Error,
      "hook acme/enhance/hooks/index.toml failed: enhancement failed",
    );
    assertEquals(
      logs.records.some((log) => log.message === "filter ran"),
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
    const value = await worker.runJob(testInvocation(), []) as Record<
      string,
      unknown
    >;
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
    assertEquals(
      await spread.runJob(testInvocation(), ["Alice Smith", "--admin"]),
      [
        "Alice Smith",
        "--admin",
      ],
    );
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
      await worker.runJob(testInvocation(), []);
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
    assertEquals(await worker.runJob(testInvocation(), ["input"]), {
      input: "input",
    });
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
    assertEquals(await worker.runJob(testInvocation(), [11]), {
      columns: ["value"],
      rows: [[11]],
    });
    const statement = calls.find((call) =>
      call.operation === "database.execute"
    );
    const cleanup = calls.find((call) =>
      call.operation === "database.scope.close"
    );
    assertEquals(statement?.contextId, cleanup?.contextId);
    assertEquals(isId(statement?.jobRunId, "job"), true);
    assertEquals(statement?.workerId, worker.metadata.workerId);
    assertEquals(statement?.serviceId, worker.metadata.workloadId);
  } finally {
    await worker.stop();
  }
});

Deno.test("job secure inputs are isolated and cleared after failures", async () => {
  const logs = new TestLogSink();
  const first = new RuntimeWorker({
    logSink: logs,
    metadata: metadata("job", example("job_secret"), "secret-first"),
    permissions: { read: [new URL("..", import.meta.url).pathname] },
  });
  const second = new RuntimeWorker({
    metadata: metadata("job", example("job_secret"), "secret-second"),
    permissions: { read: [new URL("..", import.meta.url).pathname] },
  });
  try {
    const values = await Promise.all([
      first.runJob(testInvocation(), ["password"], {
        password: "first-private-value",
      }),
      second.runJob(testInvocation(), ["password"], {
        password: "second-private-value",
      }),
    ]);
    assertEquals(values, ["first-private-value", "second-private-value"]);
    await assertRejects(
      () =>
        first.runJob(testInvocation(), ["password", true], {
          password: "never-leak-this",
        }),
      Error,
      "deliberate job failure",
    );
    assertEquals(await first.runJob(testInvocation(), ["password"]), "missing");
    assertEquals(
      JSON.stringify(logs.records).includes("never-leak-this"),
      false,
    );
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
        contextId: "ctx-0000000003",
        serviceId: "example/context/service",
        serviceGeneration: 2,
        canonicalBasePath: "/example/context/service",
        originalUrl: "https://example.test/example/context/service",
        client: { ipAddress: "203.0.113.4", networkScope: "public" },
        execution: {
          nodeId: workerMetadata.nodeId,

          sandboxId: workerMetadata.sandboxId,
          workerId: workerMetadata.workerId,
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
    assertEquals(value.contextId, "ctx-0000000003");
    assertEquals(value.sandboxId, workerMetadata.sandboxId);
    assertEquals(value.workerId, workerMetadata.workerId);
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
    assertEquals(await job.runJob(testInvocation(), [target]), "external-api");
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
    contextId: "ctx-0000000010",
    serviceId: "example/websocket/service",
    serviceGeneration: 4,
    canonicalBasePath: "/example/websocket/service",
    originalUrl: "https://example.test/example/websocket/service/echo/main",
    client: { ipAddress: "203.0.113.4", networkScope: "public" },
    execution: {
      nodeId: workerMetadata.nodeId,

      sandboxId: workerMetadata.sandboxId,
      workerId: workerMetadata.workerId,
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
    for (
      const invalid of [
        { contextId: "ctx-short" },
        { parentContextId: newId("job") },
      ]
    ) {
      const invalidMetadata = { ...requestMetadata, ...invalid };
      await assertRejects(
        () =>
          worker.dispatchService(
            new Request("http://service/echo/main"),
            invalidMetadata,
          ),
        TypeError,
        "invalid invocation identity",
      );
      await assertRejects(
        () =>
          worker.openServiceWebSocket(
            new Request("http://service/echo/main"),
            invalidMetadata,
            "the8020.echo",
            { send() {}, close() {} },
          ),
        TypeError,
        "invalid invocation identity",
      );
    }
    assertEquals(worker.inFlight, 0);
    assertEquals(sent, []);
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
      "ready:main:ctx-0000000010:the8020.echo",
    );
    assertEquals(worker.inFlight, 1);

    opened.connection.send("hello");
    opened.connection.send(new Uint8Array([4, 5, 6]));
    await waitFor(() => sent.length === 3);
    assertEquals(sent[1], "echo:hello");
    assertEquals(sent[2], new Uint8Array([4, 5, 6]));
    assertEquals(closes, []);

    opened.connection.send("close");
    await waitFor(() => closes.length === 1);
    await waitFor(() => worker.inFlight === 0);

    const replacement = await worker.openServiceWebSocket(
      new Request("http://service/echo/replacement"),
      { ...requestMetadata, contextId: "ctx-0000000012" },
      "the8020.echo",
      { send() {}, close() {} },
    );
    if (!replacement.accepted) throw new Error("Replacement was rejected");
    assertEquals(worker.inFlight, 1);
    // A delayed transport close must not release the replacement's slot.
    opened.connection.close(1000, "server closed");
    assertEquals(worker.inFlight, 1);
    replacement.connection.close();
    assertEquals(worker.inFlight, 0);

    const missing = await worker.openServiceWebSocket(
      new Request("http://service/missing"),
      { ...requestMetadata, contextId: "ctx-0000000011" },
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
    contextId: "ctx-0000000004",
    serviceId: "example/auth/login",
    serviceGeneration: 1,
    canonicalBasePath: "/example/auth/login",
    originalUrl: "https://example.test/example/auth/login",
    client: { ipAddress: "203.0.113.4", networkScope: "public" },
    execution: {
      nodeId: worker.metadata.nodeId,

      sandboxId: worker.metadata.sandboxId,
      workerId: worker.metadata.workerId,
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
      contextId: "ctx-0000000004",
      serviceId: "example/auth/login",

      workerId: worker.metadata.workerId,
      persistentExecutionId: undefined,
      user: { userId: "user:system", username: "system" },
    });
    await worker.dispatchService(
      new Request("http://service/verify"),
      { ...requestMetadata, contextId: "ctx-0000000005" },
    );
    const applicationCalls = () =>
      calls.filter((call) => call.operation !== "database.scope.close");
    assertEquals(applicationCalls()[1]?.operation, "runtime.operation");
    assertEquals(applicationCalls()[1]?.contextId, "ctx-0000000005");
    const admin = await worker.dispatchService(
      new Request("http://service/admin"),
      { ...requestMetadata, contextId: "ctx-0000000002" },
    );
    assertEquals(await admin.json(), { ready: true });
    assertEquals(applicationCalls()[2]?.operation, "admin.execute");
    assertEquals(applicationCalls()[2]?.arguments, {
      command_id: "kernel.status",
      arguments: {},
    });
    const query = await worker.dispatchService(
      new Request("http://service/database-query"),
      { ...requestMetadata, contextId: "ctx-0000000008" },
    );
    assertEquals(await query.json(), {
      columns: ["value"],
      rows: [[7]],
    });
    assertEquals(applicationCalls()[3]?.operation, "database.execute");
    const execute = await worker.dispatchService(
      new Request("http://service/database-execute"),
      { ...requestMetadata, contextId: "ctx-0000000007" },
    );
    assertEquals(await execute.json(), {
      columns: [],
      rows: [],
      affected_rows: 1,
    });
    assertEquals(applicationCalls()[4]?.operation, "database.execute");
    const streamed = await worker.dispatchService(
      new Request("http://service/database-stream"),
      { ...requestMetadata, contextId: "ctx-0000000009" },
    );
    assertEquals(await streamed.json(), {
      columns: ["value"],
      rows: [[7]],
    });
    assertEquals(applicationCalls()[5]?.operation, "database.execute");
    assertEquals(applicationCalls()[5]?.contextId, "ctx-0000000009");
    assertEquals(
      calls.filter((call) => call.operation === "database.scope.close").map(
        (call) => call.contextId,
      ),
      [
        "ctx-0000000004",
        "ctx-0000000005",
        "ctx-0000000002",
        "ctx-0000000008",
        "ctx-0000000007",
        "ctx-0000000009",
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
        testInvocation(),
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
    assertEquals(calls[0]?.contextId, undefined);
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
        contextId: "ctx-0000000006",
        serviceId: "example/database/service",
        serviceGeneration: 1,
        canonicalBasePath: "/example/database/service",
        originalUrl: "http://service/database-query",
        client: { ipAddress: "127.0.0.1", networkScope: "loopback" },
        execution: {
          nodeId: worker.metadata.nodeId,

          sandboxId: worker.metadata.sandboxId,
          workerId: worker.metadata.workerId,
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
      () => worker.runJob(testInvocation(), []),
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
    assertEquals(await worker.runJob(testInvocation(), []), {
      token: "NotCapable",
      socket: "NotCapable",
      logs: "NotCapable",
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
    assertEquals(await worker.runJob(testInvocation(), []), "nested-ok");
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
    contextId: "ctx-0000000001",
    serviceId: "example/auth/service",
    serviceGeneration: 1,
    canonicalBasePath: "/example/auth/service",
    originalUrl: "https://example.test/",
    client: { ipAddress: "127.0.0.1", networkScope: "loopback" },
    execution: {
      nodeId: worker.metadata.nodeId,

      sandboxId: worker.metadata.sandboxId,
      workerId: worker.metadata.workerId,
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
    const nativeApproval = {
      ...meta,
      authentication: {
        ...meta.authentication!,
        approved: true,
        module: "must-not-be-imported",
      },
    };
    const native = await worker.dispatchService(
      new Request("https://service/"),
      nativeApproval,
    );
    assertEquals((await native.json()).authenticated, true);
    await assertRejects(
      () =>
        worker.dispatchService(new Request("https://service/"), {
          ...nativeApproval,
          user: { userId: "user:other", username: "other" },
        }),
      Error,
      "does not match execution principal",
    );
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
      assertEquals(call.contextId, "ctx-0000000001");
    }
  } finally {
    await worker.stop();
  }
});

function testInvocation() {
  return { contextId: newId("ctx"), jobRunId: newId("job") };
}

Deno.test("console records preserve different users across overlapping service continuations", async () => {
  const logs = new TestLogSink();
  const worker = new RuntimeWorker({
    metadata: metadata(
      "service",
      new URL("./testdata/logging_service.ts", import.meta.url).href,
      "logging",
    ),
    permissions: { read: [new URL("./testdata", import.meta.url).pathname] },
    logSink: logs,
  });
  const contextIds = [newId("ctx"), newId("ctx")];
  const request = (username: string, index: number, delay: number) =>
    worker.dispatchService(
      new Request(`http://service/?delay=${delay}`),
      {
        contextId: contextIds[index]!,
        serviceId: "acme/example/logs",
        serviceGeneration: 1,
        canonicalBasePath: "/acme/example/logs/",
        originalUrl: "http://service/",
        client: { ipAddress: "127.0.0.1", networkScope: "loopback" },
        execution: {
          nodeId: worker.metadata.nodeId,
          sandboxId: worker.metadata.sandboxId,
          workerId: worker.metadata.workerId,
        },
        user: { userId: `user:${username}`, username },
        auth: { authenticated: false },
      },
    ).then((response) => response.json());
  try {
    await Promise.all([request("alice", 0, 30), request("bob", 1, 0)]);
    assertEquals(
      logs.records.filter((record) => record.component === "worker").map((
        record,
      ) => record.message),
      [
        "begin alice",
        "begin bob",
        "end bob",
        "end alice",
      ],
    );
    for (
      const [username, id] of [["alice", contextIds[0]], ["bob", contextIds[1]]]
    ) {
      const records = logs.records.filter((record) =>
        record.username === username
      );
      assertEquals(records.length, 2);
      assertEquals(
        records.every((record) =>
          record.context_id === id &&
          record.worker_id === worker.metadata.workerId
        ),
        true,
      );
    }
  } finally {
    await worker.stop();
  }
});

Deno.test("failed reused jobs forward redacted diagnostics under their allocated invocation", async () => {
  const logs = new TestLogSink();
  const worker = new RuntimeWorker({
    metadata: metadata(
      "job",
      new URL("./testdata/logging_job.ts", import.meta.url).href,
      "logging-job",
    ),
    permissions: { read: [new URL("./testdata", import.meta.url).pathname] },
    logSink: logs,
  });
  const first = testInvocation(), next = testInvocation();
  try {
    await assertRejects(
      () => worker.runJob(first, [true], { password: "never-persist-this" }),
      Error,
      "outer",
    );
    await worker.runJob(next, []);
    const failure = logs.records.find((record) => record.level === "ERROR")!;
    assertEquals(failure.context_id, first.contextId);
    assertEquals(failure.job_id, first.jobRunId);
    assertEquals(failure.username, "system");
    assertEquals(failure.message.includes("Caused by:"), true);
    assertEquals(failure.message.includes("logging_job.ts"), true);
    assertEquals(
      JSON.stringify(logs.records).includes("never-persist-this"),
      false,
    );
    assertEquals(logs.records.at(-1)!.context_id, next.contextId);
    assertEquals(logs.records.at(-1)!.job_id, next.jobRunId);
  } finally {
    await worker.stop();
  }
});
