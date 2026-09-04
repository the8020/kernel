import { assertEquals, assertRejects } from "../test/assert.ts";
import { type MessageType, PROTOCOL_VERSION } from "@the8020/protocol";
import { serviceCheckArguments, Supervisor } from "./supervisor.ts";
import type { ExecutionMetadata } from "../worker/contracts.ts";
import { kernelCallbackRequest } from "./callback_request.ts";

const token = "0123456789abcdef0123456789abcdef";
const examples = new URL("../examples", import.meta.url).pathname;

Deno.test("kernel callback payloads contain only operation-owned fields", () => {
  const base = {
    arguments: {},
    requestId: "request-test",
    serviceId: "example/persistent",
    executionId: "execution-test",
    workerId: "wrk-source01",
    persistentExecutionId: "persistent-test",
  };
  const invocation = kernelCallbackRequest({
    ...base,
    operation: "worker.invoke",
    arguments: {
      nodeId: "node-target",
      sandboxId: "sbx-target01",
      workerId: "wrk-target01",
      persistentExecutionId: "persistent-target",
      function: "example.inspect",
      input: { value: 1 },
    },
  });
  assertEquals(invocation, {
    path: "/v1/runtime/worker/invoke",
    messageType: "worker_invoke",
    responseMessageType: "worker_result",
    payload: {
      target_node_id: "node-target",
      target_sandbox_id: "sbx-target01",
      target_worker_id: "wrk-target01",
      target_persistent_execution_id: "persistent-target",
      function: "example.inspect",
      input: { value: 1 },
      execution_id: "execution-test",
      worker_id: "wrk-source01",
      request_id: "request-test",
    },
  });
  assertEquals(
    kernelCallbackRequest({
      ...base,
      operation: "execution.completePersistent",
    }).payload,
    {
      execution_id: "execution-test",
      worker_id: "wrk-source01",
      request_id: "request-test",
      service_id: "example/persistent",
      persistent_execution_id: "persistent-test",
    },
  );
  assertEquals(
    kernelCallbackRequest({
      ...base,
      operation: "database.execute",
      arguments: { statement: "SELECT $1", parameters: [1], return_rows: true },
    }),
    {
      path: "/v1/runtime/database/execute",
      messageType: "database_execute",
      responseMessageType: "database_result",
      payload: {
        statement: "SELECT $1",
        parameters: [1],
        return_rows: true,
        execution_id: "execution-test",
        worker_id: "wrk-source01",
        request_id: "request-test",
      },
    },
  );
});

Deno.test("service type checking uses supported dependency-mode arguments", () => {
  assertEquals(
    serviceCheckArguments(
      "file:///workspace/packages/service.ts",
      "cached_only",
    ),
    [
      "check",
      "--config=/opt/runtime/deno.json",
      "file:///workspace/packages/service.ts",
    ],
  );
  assertEquals(
    serviceCheckArguments(
      "file:///workspace/packages/service.ts",
      "online",
    ),
    [
      "check",
      "--config=/opt/runtime/deno.json",
      "file:///workspace/packages/service.ts",
    ],
  );
});

Deno.test("closing a Worker requests transaction cleanup for its scope", async () => {
  const calls: Array<Record<string, unknown>> = [];
  const supervisor = new Supervisor({
    runtimeGroupId: "group-test",
    sandboxId: "sandbox-test",
    workloadType: "service",
    token,
    supervisorVersion: "test",
    startedAt: Date.now(),
    workerStopGraceMilliseconds: 25,
    kernelCall: (call) => {
      calls.push(call as unknown as Record<string, unknown>);
      return Promise.resolve(undefined);
    },
  });
  const worker = await supervisor.startWorker({
    metadata: metadata("wrk-cleanup"),
    permissions: { read: [examples] },
  });
  await supervisor.stopWorker(worker.metadata.workerId, true);
  assertEquals(
    calls.some((call) =>
      call.operation === "database.scope.close" &&
      call.executionId === "execution-wrk-cleanup" &&
      call.workerId === "wrk-cleanup" && call.serviceId === undefined
    ),
    true,
  );
});

Deno.test("database-disabled Workers cannot execute SQL", async () => {
  const supervisor = new Supervisor({
    runtimeGroupId: "group-test",
    sandboxId: "sandbox-test",
    workloadType: "service",
    token,
    supervisorVersion: "test",
    startedAt: Date.now(),
    workerStopGraceMilliseconds: 25,
    kernelCall: () => Promise.resolve({ columns: [], rows: [] }),
  });
  const restricted = metadata("wrk-no-database");
  restricted.entrypoint = new URL(
    "../examples/service_kernel.ts",
    import.meta.url,
  ).href;
  restricted.databaseAccess = "none";
  const worker = await supervisor.startWorker({
    metadata: restricted,
    permissions: { read: [examples] },
  });
  await assertRejects(
    () =>
      worker.dispatchService(
        new Request("http://service/database-query"),
      ),
    Error,
    "database SQL is not available",
  );
  await supervisor.drain();
});

function metadata(id: string): ExecutionMetadata {
  return {
    nodeId: "node-test",
    runtimeGroupId: "group-test",
    sandboxId: "sandbox-test",
    workerId: id,
    executionId: `execution-${id}`,
    workloadType: "service",
    ownerId: "owner",
    workloadId: "service-a",
    releaseId: "test",
    databaseBackend: "sqlite",
    entrypoint: new URL("../examples/service.ts", import.meta.url).href,
    debuggerName: `service:owner:execution-${id}:${id}`,
  };
}

function controlEnvelope(
  messageType: MessageType,
  payload: Record<string, unknown>,
  protocolVersion: number = PROTOCOL_VERSION,
): string {
  return JSON.stringify({
    protocol_version: protocolVersion,
    message_type: messageType,
    runtime_group_id: "group-job",
    correlation_id: "correlation-test",
    payload,
  });
}

Deno.test("supervisor authenticates health/status and rejects cross-type Workers", async () => {
  const supervisor = new Supervisor({
    runtimeGroupId: "group",
    sandboxId: "sandbox",
    workloadType: "service",
    token,
    supervisorVersion: "test",
    startedAt: Date.now(),
    workerStopGraceMilliseconds: 25,
  });
  assertEquals(supervisor.options.workerStopGraceMilliseconds, 25);
  const unauthorized = await supervisor.handler(
    new Request("http://runtime/v1/status"),
  );
  assertEquals(unauthorized.status, 401);
  const health = await supervisor.handler(
    new Request("http://runtime/v1/health", {
      headers: { authorization: `Bearer ${token}` },
    }),
  );
  assertEquals(health.status, 200);
  assertEquals((await health.json()).protocol_version, 1);
  const wrong = metadata("wrong");
  wrong.workloadType = "job";
  await assertRejects(
    () =>
      supervisor.startWorker({
        metadata: wrong,
        permissions: { read: [examples] },
      }),
    Error,
    "does not match",
  );
  const protocolMismatch = await supervisor.handler(
    new Request("http://runtime/v1/workers/start", {
      method: "POST",
      headers: {
        authorization: `Bearer ${token}`,
        "content-type": "application/json",
      },
      body: controlEnvelope("start_worker", {}, 2),
    }),
  );
  const protocolError = await protocolMismatch.json();
  assertEquals(protocolMismatch.status, 400);
  assertEquals(protocolError.message_type, "error_response");
  assertEquals(
    (protocolError.payload.error as string).includes(
      "unsupported runtime protocol version",
    ),
    true,
  );
});

Deno.test("supervisor tracks Workers, service pools, and drain", async () => {
  const supervisor = new Supervisor({
    runtimeGroupId: "group",
    sandboxId: "sandbox",
    workloadType: "service",
    token,
    supervisorVersion: "test",
  });
  const first = await supervisor.startWorker({
    metadata: metadata("worker-b"),
    permissions: { read: [examples] },
  });
  const second = await supervisor.startWorker({
    metadata: metadata("worker-a"),
    permissions: { read: [examples] },
  });
  const readySnapshot = supervisor.snapshot() as {
    revision: number;
    supervisor_started_at_ms: number;
    worker_count: number;
    ready_worker_count: number;
    workers: Array<{ worker_id: string; state: string }>;
  };
  assertEquals(readySnapshot.worker_count, 2);
  assertEquals(readySnapshot.ready_worker_count, 2);
  assertEquals(readySnapshot.workers.map((worker) => worker.worker_id), [
    "worker-a",
    "worker-b",
  ]);
  assertEquals(
    readySnapshot.workers.some((worker) => "logs" in worker),
    false,
  );
  assertEquals(
    Number.isSafeInteger(readySnapshot.supervisor_started_at_ms),
    true,
  );
  const retried = await supervisor.startWorker({
    metadata: metadata("worker-a"),
    permissions: { read: [examples] },
  });
  assertEquals(retried, second);
  await assertRejects(
    () =>
      supervisor.startWorker({
        metadata: metadata("worker-a"),
        permissions: { read: [] },
      }),
    Error,
    "different configuration",
  );
  assertEquals(
    second.metadata.debuggerName,
    "service:owner:execution-worker-a:worker-a",
  );
  supervisor.configureService("service-a", [
    first.metadata.workerId,
    second.metadata.workerId,
  ], 32);
  assertEquals(
    supervisor.selectServiceWorker("service-a").metadata.workerId,
    "worker-a",
  );
  const request = first.dispatchService(new Request("http://service/slow"));
  await Promise.resolve();
  const activeSnapshot = supervisor.snapshot() as {
    revision: number;
    active_requests: number;
  };
  assertEquals(activeSnapshot.active_requests, 1);
  assertEquals(activeSnapshot.revision > readySnapshot.revision, true);
  assertEquals(
    supervisor.selectServiceWorker("service-a").metadata.workerId,
    "worker-a",
  );
  assertEquals(await (await request).text(), "GET:/slow:streamed:");
  const idleSnapshot = supervisor.snapshot() as {
    revision: number;
    active_requests: number;
  };
  assertEquals(idleSnapshot.active_requests, 0);
  assertEquals(idleSnapshot.revision > activeSnapshot.revision, true);
  const drainingResponse = await first.dispatchService(
    new Request("http://service/draining"),
  );
  supervisor.configureService("service-a", [second.metadata.workerId], 32);
  const stopping = supervisor.stopWorker(first.metadata.workerId);
  await new Promise((resolve) => setTimeout(resolve, 10));
  assertEquals(
    supervisor.workers().find((item) =>
      item.worker_id === first.metadata.workerId
    )?.state,
    "stopping",
  );
  assertEquals(
    supervisor.selectServiceWorker("service-a").metadata.workerId,
    second.metadata.workerId,
  );
  assertEquals(
    await drainingResponse.text(),
    "GET:/draining:streamed:",
  );
  await stopping;
  const routed = await supervisor.handler(
    new Request("http://runtime/v1/services/service-a/dispatch", {
      method: "POST",
      headers: {
        authorization: `Bearer ${token}`,
        "x-80-20-method": "POST",
        "x-80-20-url": "http://service/routed",
      },
      body: "body",
    }),
  );
  assertEquals(
    routed.headers.get("x-80-20-runtime-worker-id"),
    "worker-a",
  );
  assertEquals(await routed.text(), "POST:/routed:streamed:body");
  assertEquals(supervisor.status().worker_count, 1);
  await supervisor.drain();
  await supervisor.stopWorker("already-absent");
  assertEquals(supervisor.status().worker_count, 0);
  assertEquals(supervisor.status().draining, true);
});

Deno.test("higher concurrency has one bounded temporary slot per Worker", async () => {
  const supervisor = new Supervisor({
    runtimeGroupId: "group-soft-limit",
    sandboxId: "sandbox-soft-limit",
    workloadType: "service",
    token,
    supervisorVersion: "test",
  });
  const workerMetadata = metadata("worker-soft-limit");
  const worker = await supervisor.startWorker({
    metadata: workerMetadata,
    permissions: { read: [examples] },
  });
  supervisor.configureService("service-a", [worker.metadata.workerId], 2, 1);
  const dispatch = () =>
    supervisor.handler(
      new Request("http://runtime/v1/services/service-a/dispatch", {
        method: "POST",
        headers: {
          authorization: `Bearer ${token}`,
          "x-80-20-method": "GET",
          "x-80-20-url": "http://service/stream",
        },
      }),
    );
  const responses: Response[] = [];
  try {
    for (let index = 0; index < 3; index++) responses.push(await dispatch());
    assertEquals(supervisor.workers()[0]?.in_flight, 3);
    let fourthSettled = false;
    const fourth = dispatch().then((response) => {
      fourthSettled = true;
      return response;
    });
    await new Promise((resolve) => setTimeout(resolve, 10));
    assertEquals(fourthSettled, false);
    await responses.shift()!.body?.cancel();
    responses.push(await fourth);
    assertEquals(supervisor.workers()[0]?.in_flight, 3);
  } finally {
    await Promise.all(responses.map((response) => response.body?.cancel()));
    await supervisor.stopWorker(worker.metadata.workerId, true);
  }
});

Deno.test("concurrent Worker lifecycle retries remain idempotent", async () => {
  let releaseValidation!: () => void;
  const validationGate = new Promise<void>((resolve) => {
    releaseValidation = resolve;
  });
  let validationCalls = 0;
  const supervisor = new Supervisor({
    runtimeGroupId: "group-retry",
    sandboxId: "sandbox-retry",
    workloadType: "service",
    token,
    supervisorVersion: "test",
    entrypointValidator: async () => {
      validationCalls++;
      await validationGate;
    },
  });
  const validated = metadata("worker-retry");
  validated.validateEntrypoint = true;
  const options = {
    metadata: validated,
    permissions: { read: [examples] },
  };
  const firstStart = supervisor.startWorker(options);
  await Promise.resolve();
  const repeatedStart = supervisor.startWorker(options);
  await assertRejects(
    () =>
      supervisor.startWorker({
        metadata: validated,
        permissions: { read: [] },
      }),
    Error,
    "different configuration",
  );
  assertEquals(validationCalls, 1);
  releaseValidation();
  const [first, repeated] = await Promise.all([firstStart, repeatedStart]);
  assertEquals(first, repeated);
  assertEquals(supervisor.status().worker_count, 1);
  await Promise.all([
    supervisor.stopWorker(validated.workerId),
    supervisor.stopWorker(validated.workerId),
  ]);
  assertEquals(supervisor.status().worker_count, 0);
});

Deno.test("persistent executions reserve hard slots and return to the same Worker", async () => {
  const now = 1_000;
  const supervisor = new Supervisor({
    runtimeGroupId: "group-persistent",
    sandboxId: "sandbox-persistent",
    workloadType: "service",
    token,
    supervisorVersion: "test",
    now: () => now,
  });
  const workerMetadata = [metadata("persistent-a"), metadata("persistent-b")];
  for (const item of workerMetadata) {
    item.workloadId = "service-version-a";
    item.service = {
      serviceId: "service-a",
      generation: 1,
      canonicalBasePath: "/service-a",
      executionMode: "persistent",
    };
  }
  const workers = [];
  for (const item of workerMetadata) {
    workers.push(
      await supervisor.startWorker({
        metadata: item,
        permissions: { read: [examples] },
      }),
    );
  }
  supervisor.configureService(
    "service-version-a",
    workers.map((worker) => worker.metadata.workerId),
    1,
  );
  const dispatch = (
    executionId: string,
    keepAlive = 30,
    targetWorkerId?: string,
  ) =>
    supervisor.handler(
      new Request("http://runtime/v1/services/service-version-a/dispatch", {
        method: "POST",
        headers: {
          authorization: `Bearer ${token}`,
          "x-80-20-method": "GET",
          "x-80-20-url": "http://service/persistent",
          "x-80-20-internal-persistent-execution-id": executionId,
          "x-80-20-internal-persistent-keep-alive-ms": String(keepAlive),
          ...(targetWorkerId === undefined ? {} : {
            "x-80-20-internal-target-worker-id": targetWorkerId,
          }),
        },
      }),
    );
  try {
    const first = await dispatch("execution-one");
    const firstWorker = first.headers.get("x-80-20-runtime-worker-id");
    await first.body?.cancel();
    const resumed = await dispatch("execution-one");
    assertEquals(
      resumed.headers.get("x-80-20-runtime-worker-id"),
      firstWorker,
    );
    await resumed.body?.cancel();
    const second = await dispatch("execution-two");
    const secondWorker = second.headers.get("x-80-20-runtime-worker-id");
    assertEquals(secondWorker === firstWorker, false);
    await second.body?.cancel();
    assertEquals(supervisor.workers().map((worker) => worker.in_flight), [
      1,
      1,
    ]);
    supervisor.completePersistentExecution(
      "service-version-a",
      "execution-one",
      firstWorker!,
    );
    supervisor.completePersistentExecution(
      "service-version-a",
      "execution-one",
      firstWorker!,
    );
    const targeted = await dispatch("execution-three", 30, firstWorker!);
    assertEquals(targeted.status, 201);
    assertEquals(
      targeted.headers.get("x-80-20-runtime-worker-id"),
      firstWorker,
    );
    await targeted.body?.cancel();
    let mismatched = "";
    try {
      supervisor.completePersistentExecution(
        "service-version-a",
        "execution-three",
        secondWorker!,
      );
    } catch (error) {
      mismatched = error instanceof Error ? error.message : String(error);
    }
    assertEquals(
      mismatched,
      "persistent execution binding does not match Worker",
    );
  } finally {
    for (const worker of workers) {
      await supervisor.stopWorker(worker.metadata.workerId, true);
    }
  }
});

Deno.test("persistent follow-up requests obey strict single-request concurrency", async () => {
  const supervisor = new Supervisor({
    runtimeGroupId: "group-persistent-strict",
    sandboxId: "sandbox-persistent-strict",
    workloadType: "service",
    token,
    supervisorVersion: "test",
  });
  const workerMetadata = metadata("persistent-strict");
  workerMetadata.workloadId = "service-version-a";
  workerMetadata.service = {
    serviceId: "service-a",
    generation: 1,
    canonicalBasePath: "/service-a",
    executionMode: "persistent",
  };
  const worker = await supervisor.startWorker({
    metadata: workerMetadata,
    permissions: { read: [examples] },
  });
  supervisor.configureService(
    "service-version-a",
    [worker.metadata.workerId],
    1,
  );
  const dispatch = () =>
    supervisor.handler(
      new Request("http://runtime/v1/services/service-version-a/dispatch", {
        method: "POST",
        headers: {
          authorization: `Bearer ${token}`,
          "x-80-20-method": "GET",
          "x-80-20-url": "http://service/stream",
          "x-80-20-internal-persistent-execution-id": "session-one",
          "x-80-20-internal-persistent-keep-alive-ms": "100",
          "x-80-20-internal-target-worker-id": worker.metadata.workerId,
        },
      }),
    );
  let second: Response | undefined;
  try {
    const first = await dispatch();
    const pending = dispatch().then((response) => second = response);
    await new Promise((resolve) => setTimeout(resolve, 10));
    assertEquals(second, undefined);
    await first.body?.cancel();
    second = await pending;
    assertEquals(second.status, 201);
  } finally {
    await second?.body?.cancel();
    await supervisor.stopWorker(worker.metadata.workerId, true);
  }
});

Deno.test("concurrent persistent database requests retain isolated request IDs", async () => {
  const requestCount = 16;
  let enteredCount = 0;
  let allEnteredResolve!: () => void;
  const allEntered = new Promise<void>((resolve) =>
    allEnteredResolve = resolve
  );
  let releaseQueries!: () => void;
  const queryGate = new Promise<void>((resolve) => releaseQueries = resolve);
  const requestIds: string[] = [];
  const supervisor = new Supervisor({
    runtimeGroupId: "group-persistent-database",
    sandboxId: "sandbox-persistent-database",
    workloadType: "service",
    token,
    supervisorVersion: "test",
    kernelCall: async (call) => {
      if (call.operation === "database.scope.close") return { closed: true };
      if (call.operation !== "database.execute") {
        throw new Error(`unexpected kernel operation ${call.operation}`);
      }
      requestIds.push(call.requestId ?? "");
      enteredCount++;
      if (enteredCount === requestCount) allEnteredResolve();
      await queryGate;
      return { columns: ["value"], rows: [[7]] };
    },
  });
  const workerMetadata = metadata("persistent-database");
  workerMetadata.workloadId = "service-version-a";
  workerMetadata.entrypoint = new URL(
    "../examples/service_kernel.ts",
    import.meta.url,
  ).href;
  workerMetadata.service = {
    serviceId: "service-a",
    generation: 1,
    canonicalBasePath: "/service-a",
    executionMode: "persistent",
  };
  const worker = await supervisor.startWorker({
    metadata: workerMetadata,
    permissions: { read: [examples] },
  });
  supervisor.configureService(
    "service-version-a",
    [worker.metadata.workerId],
    requestCount,
    requestCount,
  );
  const expectedIds = Array.from(
    { length: requestCount },
    (_, index) => `request-${index}`,
  );
  const requests = expectedIds.map((requestId) =>
    supervisor.handler(
      new Request("http://runtime/v1/services/service-version-a/dispatch", {
        method: "POST",
        headers: {
          authorization: `Bearer ${token}`,
          "x-80-20-method": "GET",
          "x-80-20-url": "http://service/database-query",
          "x-80-20-internal-request-id": requestId,
          "x-80-20-internal-persistent-execution-id": "session-one",
          "x-80-20-internal-persistent-keep-alive-ms": "100",
        },
      }),
    )
  );
  try {
    await Promise.race([
      allEntered,
      new Promise<never>((_, reject) =>
        setTimeout(
          () => reject(new Error("concurrent database requests did not enter")),
          1_000,
        )
      ),
    ]);
    assertEquals([...new Set(requestIds)].sort(), expectedIds.sort());
    releaseQueries();
    const responses = await Promise.all(requests);
    assertEquals(responses.every((response) => response.status === 200), true);
    await Promise.all(responses.map((response) => response.body?.cancel()));
  } finally {
    releaseQueries();
    await Promise.allSettled(requests);
    await supervisor.stopWorker(worker.metadata.workerId, true);
  }
});

Deno.test("session reservation expiry starts an independent Worker idle clock", async () => {
  let now = 1_000;
  const supervisor = new Supervisor({
    runtimeGroupId: "group-keepalive",
    sandboxId: "sandbox-keepalive",
    workloadType: "service",
    token,
    supervisorVersion: "test",
    now: () => now,
  });
  const workerMetadata = metadata("keepalive-worker");
  workerMetadata.workloadId = "service-version-a";
  workerMetadata.service = {
    serviceId: "service-a",
    generation: 1,
    canonicalBasePath: "/service-a",
    executionMode: "persistent",
  };
  const worker = await supervisor.startWorker({
    metadata: workerMetadata,
    permissions: { read: [examples] },
  });
  supervisor.configureService(
    "service-version-a",
    [worker.metadata.workerId],
    1,
  );
  try {
    assertEquals(supervisor.workers()[0]?.idle_since_ms, 1_000);
    const response = await supervisor.handler(
      new Request("http://runtime/v1/services/service-version-a/dispatch", {
        method: "POST",
        headers: {
          authorization: `Bearer ${token}`,
          "x-80-20-method": "GET",
          "x-80-20-url": "http://service/persistent",
          "x-80-20-internal-persistent-execution-id": "session-one",
          "x-80-20-internal-persistent-keep-alive-ms": "100",
        },
      }),
    );
    await response.body?.cancel();
    assertEquals(supervisor.workers()[0]?.in_flight, 1);
    assertEquals(supervisor.workers()[0]?.idle_since_ms, undefined);

    const mismatch = await supervisor.invokeWorker(
      worker.metadata.workerId,
      "example.missing",
      null,
      new AbortController().signal,
      "session-other",
    );
    assertEquals(mismatch.error?.code, "target_mismatch");
    const exact = await supervisor.invokeWorker(
      worker.metadata.workerId,
      "example.missing",
      null,
      new AbortController().signal,
      "session-one",
    );
    assertEquals(exact.error?.code, "function_not_found");

    now = 1_100;
    const expired = supervisor.workers()[0];
    assertEquals(expired?.in_flight, 0);
    assertEquals(expired?.idle_since_ms, 1_100);
  } finally {
    await supervisor.stopWorker(worker.metadata.workerId, true);
  }
});

Deno.test({
  name: "supervisor owns request-service WebSocket upgrades and message relay",
  sanitizeOps: false,
  sanitizeResources: false,
  fn: async () => {
    const supervisor = new Supervisor({
      runtimeGroupId: "group-websocket",
      sandboxId: "sandbox-websocket",
      workloadType: "service",
      token,
      supervisorVersion: "test",
    });
    const workerMetadata = metadata("worker-websocket");
    workerMetadata.entrypoint = new URL(
      "../examples/service_websocket.ts",
      import.meta.url,
    ).href;
    workerMetadata.service = {
      serviceId: "example/websocket/service",
      generation: 2,
      canonicalBasePath: "/example/websocket/service",
      executionMode: "stateless",
    };
    const worker = await supervisor.startWorker({
      metadata: workerMetadata,
      permissions: {
        read: [new URL("..", import.meta.url).pathname],
        import: ["jsr.io:443"],
      },
    });
    supervisor.configureService("service-a", [worker.metadata.workerId], 32);
    let resolvePort!: (port: number) => void;
    const listening = new Promise<number>((resolve) => resolvePort = resolve);
    const server = Deno.serve({
      hostname: "127.0.0.1",
      port: 0,
      onListen: ({ port }) => resolvePort(port),
    }, supervisor.handler);
    const port = await listening;
    const connection = await Deno.connect({
      hostname: "127.0.0.1",
      port,
    });
    const stream = new ByteStream(connection);
    try {
      const key = encodeBase64(crypto.getRandomValues(new Uint8Array(16)));
      await connection.write(new TextEncoder().encode(
        `GET /v1/services/service-a/websocket HTTP/1.1\r
Host: 127.0.0.1:${port}\r
Connection: Upgrade\r
Upgrade: websocket\r
Sec-WebSocket-Key: ${key}\r
Sec-WebSocket-Version: 13\r
Sec-WebSocket-Protocol: the8020.echo\r
Authorization: Bearer ${token}\r
X-80-20-URL: http://service/echo/main\r
X-80-20-Internal-Request-ID: request-websocket-supervisor\r
X-80-20-Internal-Service-ID: example/websocket/service\r
X-80-20-Internal-Service-Generation: 2\r
X-80-20-Internal-Canonical-Base-Path: /example/websocket/service\r
X-80-20-Internal-Original-URL: https://example.test/example/websocket/service/echo/main\r
X-80-20-Internal-Auth-Authenticated: false\r
\r
`,
      ));
      const response = new TextDecoder().decode(
        await stream.until(new TextEncoder().encode("\r\n\r\n")),
      );
      assertEquals(response.startsWith("HTTP/1.1 101 "), true);
      assertEquals(
        await readWebSocketText(stream),
        "ready:main:request-websocket-supervisor:the8020.echo",
      );
      await connection.write(clientWebSocketFrame(0x1, "hello"));
      assertEquals(await readWebSocketText(stream), "echo:hello");
      await connection.write(
        clientWebSocketFrame(0x8, new Uint8Array([3, 232])),
      );
    } finally {
      connection.close();
      await server.shutdown();
      await supervisor.drain();
      await server.finished;
    }
  },
});

Deno.test("one Worker startup crash does not terminate healthy siblings", async () => {
  const supervisor = new Supervisor({
    runtimeGroupId: "group",
    sandboxId: "sandbox",
    workloadType: "service",
    token,
    supervisorVersion: "test",
  });
  const healthy = await supervisor.startWorker({
    metadata: metadata("healthy"),
    permissions: { read: [examples] },
  });
  const crashing = metadata("crashing");
  crashing.entrypoint = new URL("../examples/crash.ts", import.meta.url).href;
  await assertRejects(
    () =>
      supervisor.startWorker({
        metadata: crashing,
        permissions: { read: [examples] },
      }),
    Error,
    "startup crash",
  );
  const status = supervisor.status() as {
    worker_count: number;
    recent_failures: Array<{ worker_id: string; reason: string }>;
  };
  assertEquals(status.worker_count, 1);
  assertEquals(status.recent_failures.length, 1);
  assertEquals(status.recent_failures[0]?.worker_id, "crashing");
  assertEquals(
    status.recent_failures[0]?.reason.includes("startup crash"),
    true,
  );
  assertEquals(
    await (await healthy.dispatchService(new Request("http://service/ok")))
      .text(),
    "GET:/ok:streamed:",
  );
  await supervisor.drain();
});

Deno.test("supervisor converts only trusted internal authentication metadata", async () => {
  const supervisor = new Supervisor({
    runtimeGroupId: "group-auth",
    sandboxId: "sandbox-auth",
    workloadType: "service",
    token,
    supervisorVersion: "test",
  });
  const authMetadata = metadata("auth");
  authMetadata.entrypoint = new URL(
    "../examples/service_auth.ts",
    import.meta.url,
  ).href;
  const worker = await supervisor.startWorker({
    metadata: authMetadata,
    permissions: { read: [examples] },
  });
  supervisor.configureService("service-a", [worker.metadata.workerId], 32);
  const response = await supervisor.handler(
    new Request("http://runtime/v1/services/service-a/dispatch", {
      method: "POST",
      headers: {
        authorization: `Bearer ${token}`,
        "x-80-20-url": "http://service/auth",
        "x-80-20-internal-auth-authenticated": "true",
        "x-80-20-internal-auth-realm": "user",
        "x-80-20-internal-auth-user-id": "user:Admin",
        "x-80-20-internal-auth-username": "Admin",
        "x-80-20-internal-auth-version": "7",
      },
    }),
  );
  assertEquals(await response.json(), {
    auth: {
      authenticated: true,
      realm: "user",
      userId: "user:Admin",
      username: "Admin",
      authVersion: 7,
    },
    execution: {
      nodeId: "group-auth",
      runtimeGroupId: "group-auth",
      sandboxId: "sandbox-auth",
      workerId: worker.metadata.workerId,
      workerExecutionId: worker.metadata.executionId,
    },
    internalHeaderVisible: false,
  });
  await supervisor.drain();
});

Deno.test("exact Worker control invokes only explicitly registered functions", async () => {
  const supervisor = new Supervisor({
    runtimeGroupId: "group-control",
    sandboxId: "sandbox-control",
    workloadType: "service",
    token,
    supervisorVersion: "test",
  });
  const controlMetadata = metadata("worker-control");
  controlMetadata.entrypoint = new URL(
    "../examples/service_control.ts",
    import.meta.url,
  ).href;
  await supervisor.startWorker({
    metadata: controlMetadata,
    permissions: { read: [examples] },
  });
  const invoked = await supervisor.handler(
    new Request("http://runtime/v1/workers/worker-control/invoke", {
      method: "POST",
      headers: { authorization: `Bearer ${token}` },
      body: controlEnvelope("worker_invoke", {
        function: "example.echo",
        input: { value: "ok" },
      }).replace(
        '"runtime_group_id":"group-job"',
        '"runtime_group_id":"group-control"',
      ),
    }),
  );
  const envelope = await invoked.json();
  assertEquals(envelope.payload, { ok: true, output: { value: "ok" } });

  const wrongWorker = await supervisor.handler(
    new Request("http://runtime/v1/workers/worker-other/invoke", {
      method: "POST",
      headers: { authorization: `Bearer ${token}` },
      body: controlEnvelope("worker_invoke", {
        function: "example.echo",
        input: { value: "wrong target" },
      }).replace(
        '"runtime_group_id":"group-job"',
        '"runtime_group_id":"group-control"',
      ),
    }),
  );
  assertEquals((await wrongWorker.json()).payload, {
    ok: false,
    error: {
      code: "target_not_found",
      message: "Worker worker-other is unavailable",
    },
  });

  const unregistered = await supervisor.handler(
    new Request("http://runtime/v1/workers/worker-control/invoke", {
      method: "POST",
      headers: { authorization: `Bearer ${token}` },
      body: controlEnvelope("worker_invoke", {
        function: "default",
        input: null,
      }).replace(
        '"runtime_group_id":"group-job"',
        '"runtime_group_id":"group-control"',
      ),
    }),
  );
  assertEquals((await unregistered.json()).payload, {
    ok: false,
    error: {
      code: "function_not_found",
      message: "Worker function default is not registered",
    },
  });
  await supervisor.drain();
});

Deno.test("job dispatch returns bounded structured and console logs", async () => {
  const checked: string[][] = [];
  const analyzed: string[][] = [];
  const supervisor = new Supervisor({
    runtimeGroupId: "group-job",
    sandboxId: "sandbox-job",
    workloadType: "job",
    token,
    supervisorVersion: "test",
    entrypointValidator: (modules) => {
      checked.push(modules);
      return Promise.resolve();
    },
    moduleAnalyzer: (modules) => {
      analyzed.push(modules);
      return Promise.resolve(Object.fromEntries(modules.map((module) => [
        module,
        [module, "/workspace/packages/the8020/demo/src/shared.ts"],
      ])));
    },
  });
  const job = metadata("worker-job");
  job.workloadType = "job";
  job.workloadId = "job-a";
  job.entrypoint = new URL("../examples/job.ts", import.meta.url).href;
  await supervisor.startWorker({
    metadata: job,
    permissions: { read: [examples] },
  });
  const response = await supervisor.handler(
    new Request("http://runtime/v1/jobs/worker-job/run", {
      method: "POST",
      headers: {
        authorization: `Bearer ${token}`,
        "content-type": "application/json",
      },
      body: controlEnvelope("job_start", {
        arguments: [{ value: 1 }],
        secrets: {},
        check_modules: [job.entrypoint],
      }),
    }),
  );
  const body = await response.json();
  assertEquals(response.status, 200);
  assertEquals(body.message_type, "job_result");
  assertEquals(body.correlation_id, "correlation-test");
  assertEquals(body.payload.result, {
    input: { value: 1 },
  });
  assertEquals(checked, [[job.entrypoint]]);
  assertEquals(analyzed, [[job.entrypoint]]);
  assertEquals(body.payload.module_dependencies, {
    [job.entrypoint]: [
      job.entrypoint,
      "/workspace/packages/the8020/demo/src/shared.ts",
    ],
  });
  assertEquals(
    body.payload.logs.map((event: { message: string }) => event.message),
    ['job input {"value":1}'],
  );
  const status = supervisor.workers().find((worker) =>
    worker.worker_id === job.workerId
  );
  assertEquals(status?.logs?.map((event) => event.message), [
    'job input {"value":1}',
  ]);
  await supervisor.drain();
});

Deno.test("job dispatch preserves structured command failures", async () => {
  const supervisor = new Supervisor({
    runtimeGroupId: "group-job",
    sandboxId: "sandbox-job-error",
    workloadType: "job",
    token,
    supervisorVersion: "test",
  });
  const job = metadata("worker-job-error");
  job.workloadType = "job";
  job.workloadId = "job-error";
  job.entrypoint = new URL("../examples/job_error.ts", import.meta.url).href;
  await supervisor.startWorker({
    metadata: job,
    permissions: { read: [examples] },
  });
  const response = await supervisor.handler(
    new Request("http://runtime/v1/jobs/worker-job-error/run", {
      method: "POST",
      headers: {
        authorization: `Bearer ${token}`,
        "content-type": "application/json",
      },
      body: controlEnvelope("job_start", { arguments: [], secrets: {} }),
    }),
  );
  const body = await response.json();
  assertEquals(response.status, 400);
  assertEquals(body.message_type, "error_response");
  assertEquals(body.payload, {
    error: "structured job failure",
    code: "invalid_arguments",
    details: { field: "example" },
  });
  await supervisor.drain();
});

class ByteStream {
  readonly #connection: Deno.Conn;
  #buffer = new Uint8Array();

  constructor(connection: Deno.Conn) {
    this.#connection = connection;
  }

  async bytes(length: number): Promise<Uint8Array> {
    while (this.#buffer.byteLength < length) await this.#read();
    const value = this.#buffer.slice(0, length);
    this.#buffer = this.#buffer.slice(length);
    return value;
  }

  async until(delimiter: Uint8Array): Promise<Uint8Array> {
    while (true) {
      const index = indexOfBytes(this.#buffer, delimiter);
      if (index >= 0) {
        const end = index + delimiter.byteLength;
        const value = this.#buffer.slice(0, end);
        this.#buffer = this.#buffer.slice(end);
        return value;
      }
      await this.#read();
    }
  }

  async #read(): Promise<void> {
    const chunk = new Uint8Array(4_096);
    const length = await this.#connection.read(chunk);
    if (length === null) throw new Error("unexpected WebSocket EOF");
    const combined = new Uint8Array(this.#buffer.byteLength + length);
    combined.set(this.#buffer);
    combined.set(chunk.subarray(0, length), this.#buffer.byteLength);
    this.#buffer = combined;
  }
}

async function readWebSocketText(stream: ByteStream): Promise<string> {
  const header = await stream.bytes(2);
  const opcode = header[0]! & 0x0f;
  let length = header[1]! & 0x7f;
  if (length === 126) {
    const extended = await stream.bytes(2);
    length = (extended[0]! << 8) | extended[1]!;
  } else if (length === 127) {
    throw new Error("test WebSocket frame is unexpectedly large");
  }
  if ((header[1]! & 0x80) !== 0) {
    throw new Error("server WebSocket frame must not be masked");
  }
  const payload = await stream.bytes(length);
  if (opcode !== 0x1) {
    throw new Error(`expected text WebSocket frame, received opcode ${opcode}`);
  }
  return new TextDecoder().decode(payload);
}

function clientWebSocketFrame(
  opcode: number,
  value: string | Uint8Array,
): Uint8Array {
  const payload = typeof value === "string"
    ? new TextEncoder().encode(value)
    : value;
  if (payload.byteLength >= 126) throw new Error("test frame is too large");
  const mask = new Uint8Array([0x11, 0x22, 0x33, 0x44]);
  const frame = new Uint8Array(2 + mask.byteLength + payload.byteLength);
  frame[0] = 0x80 | opcode;
  frame[1] = 0x80 | payload.byteLength;
  frame.set(mask, 2);
  for (let index = 0; index < payload.byteLength; index++) {
    frame[6 + index] = payload[index]! ^ mask[index % mask.byteLength]!;
  }
  return frame;
}

function indexOfBytes(value: Uint8Array, target: Uint8Array): number {
  outer:
  for (let index = 0; index <= value.byteLength - target.byteLength; index++) {
    for (let offset = 0; offset < target.byteLength; offset++) {
      if (value[index + offset] !== target[offset]) continue outer;
    }
    return index;
  }
  return -1;
}

function encodeBase64(value: Uint8Array): string {
  let binary = "";
  for (const byte of value) binary += String.fromCharCode(byte);
  return btoa(binary);
}
