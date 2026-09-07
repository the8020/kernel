import { newId } from "../identity/mod.ts";
import { assertEquals, assertRejects } from "../test/assert.ts";
import { type MessageType, PROTOCOL_VERSION } from "@the8020/protocol";
import { serviceCheckArguments, Supervisor } from "./supervisor.ts";
import type { ExecutionMetadata } from "../worker/contracts.ts";
import { kernelCallbackRequest } from "./callback_request.ts";
import { TestLogSink } from "../test/logs.ts";
import { readHTTPResponse } from "./unix_http.ts";

const token = "0123456789abcdef0123456789abcdef";
const examples = new URL("../examples", import.meta.url).pathname;

Deno.test("kernel callback payloads contain only operation-owned fields", () => {
  const base = {
    arguments: {},
    contextId: "ctx-0000000001",
    serviceId: "example/persistent",

    workerId: "wrk-0000000025",
    persistentExecutionId: "pex-0000000010",
  };
  const invocation = kernelCallbackRequest({
    ...base,
    operation: "worker.invoke",
    arguments: {
      nodeId: "nod-0000000002",
      sandboxId: "sbx-0000000015",
      workerId: "wrk-0000000026",
      persistentExecutionId: "pex-0000000011",
      function: "example.inspect",
      input: { value: 1 },
    },
  });
  assertEquals(invocation, {
    path: "/v1/runtime/worker/invoke",
    messageType: "worker_invoke",
    responseMessageType: "worker_result",
    payload: {
      target_node_id: "nod-0000000002",
      target_sandbox_id: "sbx-0000000015",
      target_worker_id: "wrk-0000000026",
      target_persistent_execution_id: "pex-0000000011",
      function: "example.inspect",
      input: { value: 1 },
      worker_id: "wrk-0000000025",
      context_id: "ctx-0000000001",
    },
  });
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
        worker_id: "wrk-0000000025",
        context_id: "ctx-0000000001",
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

Deno.test("closing a Worker releases its kernel resources", async () => {
  const calls: Array<Record<string, unknown>> = [];
  const supervisor = new Supervisor({
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000013",
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
    metadata: metadata("wrk-0000000023"),
    permissions: { read: [examples] },
  });
  await supervisor.stopWorker(worker.metadata.workerId, true);
  assertEquals(
    calls.some((call) =>
      call.operation === "execution.releaseWorker" &&
      call.contextId === undefined &&
      call.workerId === "wrk-0000000023" && call.serviceId === undefined
    ),
    true,
  );
});

Deno.test("database-disabled Workers cannot execute SQL", async () => {
  const supervisor = new Supervisor({
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000013",
    workloadType: "service",
    token,
    supervisorVersion: "test",
    startedAt: Date.now(),
    workerStopGraceMilliseconds: 25,
    kernelCall: () => Promise.resolve({ columns: [], rows: [] }),
  });
  const restricted = metadata("wrk-0000000024");
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
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000013",
    workerId: id,

    workloadType: "service",
    ownerId: "owner",
    workloadId: "service-a",
    user: { userId: "user:system", username: "system" },
    origin: { type: "service", id: "service-a" },
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
    sandbox_id: "sbx-0000000004",
    correlation_id: "cor-0000000001",
    payload,
  });
}

Deno.test("supervisor authenticates health/status and rejects cross-type Workers", async () => {
  const supervisor = new Supervisor({
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000001",
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
  assertEquals((await health.json()).protocol_version, PROTOCOL_VERSION);
  const wrong = metadata("wrk-0000000027");
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
  const invalidUser = metadata("wrk-0000000006");
  invalidUser.user = { userId: "user:alice", username: "bob" };
  await assertRejects(
    () =>
      supervisor.startWorker({
        metadata: invalidUser,
        permissions: { read: [examples] },
      }),
    TypeError,
    "execution user is invalid",
  );
  const invalidOrigin = metadata("wrk-0000000005");
  invalidOrigin.origin = { type: "job", id: "job" };
  await assertRejects(
    () =>
      supervisor.startWorker({
        metadata: invalidOrigin,
        permissions: { read: [examples] },
      }),
    TypeError,
    "execution origin is invalid",
  );
  const protocolMismatch = await supervisor.handler(
    new Request("http://runtime/v1/workers/start", {
      method: "POST",
      headers: {
        authorization: `Bearer ${token}`,
        "content-type": "application/json",
      },
      body: controlEnvelope("start_worker", {}, PROTOCOL_VERSION + 1),
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
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000001",
    workloadType: "service",
    token,
    supervisorVersion: "test",
  });
  const first = await supervisor.startWorker({
    metadata: metadata("wrk-0000000016"),
    permissions: { read: [examples] },
  });
  const second = await supervisor.startWorker({
    metadata: metadata("wrk-0000000015"),
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
    "wrk-0000000015",
    "wrk-0000000016",
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
    metadata: metadata("wrk-0000000015"),
    permissions: { read: [examples] },
  });
  assertEquals(retried, second);
  await assertRejects(
    () =>
      supervisor.startWorker({
        metadata: metadata("wrk-0000000015"),
        permissions: { read: [] },
      }),
    Error,
    "different configuration",
  );
  assertEquals(
    second.metadata.debuggerName,
    `service:owner:execution-${second.metadata.workerId}:${second.metadata.workerId}`,
  );
  supervisor.configureService("service-a", [
    first.metadata.workerId,
    second.metadata.workerId,
  ], 32);
  assertEquals(
    supervisor.selectServiceWorker("service-a").metadata.workerId,
    "wrk-0000000015",
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
    "wrk-0000000015",
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
        "the8020-internal-method": "POST",
        "the8020-internal-url": "http://service/routed",
        "the8020-internal-context-id": newId("ctx"),
      },
      body: "body",
    }),
  );
  assertEquals(
    routed.headers.get("the8020-internal-selected-worker-id"),
    "wrk-0000000015",
  );
  assertEquals(await routed.text(), "POST:/routed:streamed:body");
  assertEquals(supervisor.status().worker_count, 1);
  await supervisor.drain();
  await supervisor.stopWorker("wrk-0000000001");
  assertEquals(supervisor.status().worker_count, 0);
  assertEquals(supervisor.status().draining, true);
});

Deno.test("higher concurrency has one bounded temporary slot per Worker", async () => {
  const supervisor = new Supervisor({
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000011",
    workloadType: "service",
    token,
    supervisorVersion: "test",
  });
  const workerMetadata = metadata("wrk-0000000021");
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
          "the8020-internal-method": "GET",
          "the8020-internal-url": "http://service/stream",
          "the8020-internal-context-id": newId("ctx"),
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
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000009",
    workloadType: "service",
    token,
    supervisorVersion: "test",
    entrypointValidator: async () => {
      validationCalls++;
      await validationGate;
    },
  });
  const validated = metadata("wrk-0000000020");
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
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000006",
    workloadType: "service",
    token,
    supervisorVersion: "test",
    now: () => now,
  });
  const workerMetadata = [
    metadata("wrk-0000000009"),
    metadata("wrk-0000000010"),
  ];
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
    existing = false,
  ) =>
    supervisor.handler(
      new Request("http://runtime/v1/services/service-version-a/dispatch", {
        method: "POST",
        headers: {
          authorization: `Bearer ${token}`,
          "the8020-internal-method": "GET",
          "the8020-internal-url": "http://service/persistent",
          "the8020-internal-context-id": newId("ctx"),
          "the8020-internal-persistent-execution-id": executionId,
          "the8020-internal-persistent-keep-alive-ms": String(keepAlive),
          ...(targetWorkerId === undefined ? {} : {
            "the8020-internal-target-worker-id": targetWorkerId,
          }),
          ...(existing
            ? { "the8020-internal-persistent-existing": "true" }
            : {}),
        },
      }),
    );
  try {
    const first = await dispatch("pex-0000000001");
    const firstWorker = first.headers.get(
      "the8020-internal-selected-worker-id",
    );
    await first.body?.cancel();
    const resumed = await dispatch("pex-0000000001", 30, firstWorker!, true);
    assertEquals(
      resumed.headers.get("the8020-internal-selected-worker-id"),
      firstWorker,
    );
    await resumed.body?.cancel();
    const second = await dispatch("pex-0000000002");
    const secondWorker = second.headers.get(
      "the8020-internal-selected-worker-id",
    );
    assertEquals(secondWorker === firstWorker, false);
    await second.body?.cancel();
    assertEquals(supervisor.workers().map((worker) => worker.in_flight), [
      1,
      1,
    ]);
    supervisor.completePersistentExecution(
      "service-version-a",
      "pex-0000000001",
      firstWorker!,
    );
    supervisor.completePersistentExecution(
      "service-version-a",
      "pex-0000000001",
      firstWorker!,
    );
    const targeted = await dispatch("pex-0000000003", 30, firstWorker!);
    assertEquals(targeted.status, 201);
    assertEquals(
      targeted.headers.get("the8020-internal-selected-worker-id"),
      firstWorker,
    );
    await targeted.body?.cancel();
    let mismatched = "";
    try {
      supervisor.completePersistentExecution(
        "service-version-a",
        "pex-0000000003",
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
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000008",
    workloadType: "service",
    token,
    supervisorVersion: "test",
  });
  const workerMetadata = metadata("wrk-0000000012");
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
  const dispatch = (existing = false) =>
    supervisor.handler(
      new Request("http://runtime/v1/services/service-version-a/dispatch", {
        method: "POST",
        headers: {
          authorization: `Bearer ${token}`,
          "the8020-internal-method": "GET",
          "the8020-internal-url": "http://service/stream",
          "the8020-internal-context-id": newId("ctx"),
          "the8020-internal-persistent-execution-id": "pex-0000000004",
          "the8020-internal-persistent-keep-alive-ms": "100",
          "the8020-internal-target-worker-id": worker.metadata.workerId,
          ...(existing
            ? { "the8020-internal-persistent-existing": "true" }
            : {}),
        },
      }),
    );
  let second: Response | undefined;
  try {
    const first = await dispatch();
    const pending = dispatch(true).then((response) => second = response);
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
  const contextIds: string[] = [];
  const supervisor = new Supervisor({
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000007",
    workloadType: "service",
    token,
    supervisorVersion: "test",
    kernelCall: async (call) => {
      if (call.operation === "database.scope.close") return { closed: true };
      if (call.operation !== "database.execute") {
        throw new Error(`unexpected kernel operation ${call.operation}`);
      }
      contextIds.push(call.contextId ?? "");
      enteredCount++;
      if (enteredCount === requestCount) allEnteredResolve();
      await queryGate;
      return { columns: ["value"], rows: [[7]] };
    },
  });
  const workerMetadata = metadata("wrk-0000000011");
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
    () => newId("ctx"),
  );
  // The first dispatch synchronously reserves the binding before yielding;
  // subsequent requests reuse it while all database operations overlap.
  const requests = expectedIds.map((contextId, index) =>
    supervisor.handler(
      new Request("http://runtime/v1/services/service-version-a/dispatch", {
        method: "POST",
        headers: {
          authorization: `Bearer ${token}`,
          "the8020-internal-method": "GET",
          "the8020-internal-url": "http://service/database-query",
          "the8020-internal-context-id": contextId,
          "the8020-internal-persistent-execution-id": "pex-0000000004",
          "the8020-internal-persistent-keep-alive-ms": "100",
          ...(index === 0 ? {} : {
            "the8020-internal-persistent-existing": "true",
            "the8020-internal-target-worker-id": worker.metadata.workerId,
          }),
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
    assertEquals([...new Set(contextIds)].sort(), expectedIds.sort());
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
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000005",
    workloadType: "service",
    token,
    supervisorVersion: "test",
    now: () => now,
  });
  const workerMetadata = metadata("wrk-0000000007");
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
          "the8020-internal-method": "GET",
          "the8020-internal-url": "http://service/persistent",
          "the8020-internal-context-id": newId("ctx"),
          "the8020-internal-persistent-execution-id": "pex-0000000004",
          "the8020-internal-persistent-keep-alive-ms": "100",
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
      "pex-0000000005",
      { userId: "user:system", username: "system" },
      testInvocation(),
    );
    assertEquals(mismatch.error?.code, "target_mismatch");
    const exact = await supervisor.invokeWorker(
      worker.metadata.workerId,
      "example.missing",
      null,
      new AbortController().signal,
      "pex-0000000004",
      { userId: "user:system", username: "system" },
      testInvocation(),
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

Deno.test("HTTP serving compresses Worker streams and honors service responses", async () => {
  const supervisor = new Supervisor({
    nodeId: "nod-0000000001",
    sandboxId: "sbx-0000000014",
    workloadType: "service",
    token,
    supervisorVersion: "test",
  });
  const workerMetadata = metadata("wrk-0000000022");
  workerMetadata.entrypoint = new URL(
    "../examples/service_compression.ts",
    import.meta.url,
  ).href;
  workerMetadata.service = {
    serviceId: "example/compression/service",
    generation: 1,
    canonicalBasePath: "/example/compression/service",
    executionMode: "stateless",
  };
  const worker = await supervisor.startWorker({
    metadata: workerMetadata,
    permissions: { read: [new URL("..", import.meta.url).pathname] },
  });
  supervisor.configureService("service-a", [worker.metadata.workerId], 2);
  const server = supervisor.serve({
    hostname: "127.0.0.1",
    port: 0,
    onListen() {},
  });
  const port = server.addr.port;
  const text = "compressible service response ".repeat(1_024);
  const headers = (path: string, accept?: string) => {
    const result = new Headers({
      authorization: `Bearer ${token}`,
      "the8020-internal-url": `http://service${path}`,
      "the8020-internal-context-id": newId("ctx"),
      "the8020-internal-method": "GET",
    });
    if (accept !== undefined) result.set("accept-encoding", accept);
    return result;
  };
  // Read the wire bytes directly: fetch() would silently decode gzip/Brotli.
  const request = async (path: string, accept?: string) => {
    const connection = await Deno.connect({ hostname: "127.0.0.1", port });
    const timeout = setTimeout(() => connection.close(), 5_000);
    try {
      const lines = [...headers(path, accept)].map(([key, value]) =>
        `${key}: ${value}`
      );
      const data = new TextEncoder().encode([
        "POST /v1/services/service-a/dispatch HTTP/1.0",
        `Host: 127.0.0.1:${port}`,
        "Content-Length: 0",
        "Connection: close",
        ...lines,
        "\r\n",
      ].join("\r\n"));
      let written = 0;
      while (written < data.length) {
        written += await connection.write(data.subarray(written));
      }
      const raw = await readHTTPResponse(connection);
      const boundary = indexOfBytes(raw, new TextEncoder().encode("\r\n\r\n"));
      assertEquals(boundary >= 0, true);
      const head = new TextDecoder().decode(raw.subarray(0, boundary)).split(
        "\r\n",
      );
      const status = Number(head.shift()!.split(" ")[1]);
      const responseHeaders = new Headers(head.map((line): [string, string] => {
        const split = line.indexOf(":");
        return [line.slice(0, split), line.slice(split + 1).trim()];
      }));
      assertEquals(responseHeaders.has("transfer-encoding"), false);
      const body = raw.slice(boundary + 4);
      return { status, headers: responseHeaders, body };
    } finally {
      clearTimeout(timeout);
      try {
        connection.close();
      } catch { /* Already closed by timeout. */ }
    }
  };
  const abort = new AbortController();
  try {
    for (
      const [path, accept, encoding] of [
        ["/text", undefined, null],
        ["/text", "", null],
        ["/text", "identity", null],
        ["/text", "gzip;q=0, br;q=0", null],
        ["/text", "gzip", "gzip"],
        ["/text", "gzip;q=0.5, br;q=1", "br"],
        ["/text", "br;q=1, gzip;q=0, identity;q=0", "br"],
        ["/no-transform", "gzip, br", null],
        ["/range", "gzip, br", null],
        ["/encoded", "gzip", "gzip"],
        ["/encoded", "gzip;q=0.5, br;q=1", "gzip"],
      ] as const
    ) {
      const response = await request(path, accept);
      assertEquals(response.status, path === "/range" ? 206 : 200);
      assertEquals(response.headers.get("content-encoding"), encoding);
      let decoded = new Response(response.body);
      if (encoding !== null) {
        assertEquals(response.body.length < text.length, true);
        decoded = new Response(decoded.body!.pipeThrough(
          new DecompressionStream(encoding === "br" ? "brotli" : "gzip"),
        ));
      }
      assertEquals(await decoded.text(), text);
      const automatic = encoding !== null && path !== "/encoded";
      assertEquals(
        response.headers.get("etag"),
        automatic ? 'W/"example"' : '"example"',
      );
      assertEquals(response.headers.get("vary")?.includes("Origin"), true);
      if (automatic) {
        assertEquals(
          response.headers.get("vary")?.includes("Accept-Encoding"),
          true,
        );
        assertEquals(response.headers.has("content-length"), false);
      }
      if (path === "/no-transform") {
        assertEquals(
          response.headers.get("cache-control"),
          "private, no-transform",
        );
      }
    }

    // The client must receive data before the Worker closes the response.
    const timeout = setTimeout(() => abort.abort(), 5_000);
    try {
      const response = await fetch(
        `http://127.0.0.1:${port}/v1/services/service-a/dispatch`,
        {
          method: "POST",
          headers: headers("/stream", "gzip"),
          signal: abort.signal,
        },
      );
      assertEquals(response.status, 200);
      assertEquals(response.headers.get("content-encoding"), "gzip");
      const reader = response.body!.getReader();
      const first = await reader.read();
      assertEquals(first.done, false);
      assertEquals(first.value!.length > 0, true);
      assertEquals(
        text.startsWith(new TextDecoder().decode(first.value)),
        true,
      );
      const finish = await request("/finish", "gzip");
      assertEquals(finish.status, 204);
      assertEquals(finish.body.length, 0);
      let size = first.value!.length;
      while (true) {
        const next = await reader.read();
        if (next.done) break;
        size += next.value.length;
      }
      assertEquals(size, text.length);
    } finally {
      clearTimeout(timeout);
    }
  } finally {
    abort.abort();
    await supervisor.drain();
    await server.shutdown();
  }
});

Deno.test({
  name: "supervisor owns request-service WebSocket upgrades and message relay",
  sanitizeOps: false,
  sanitizeResources: false,
  fn: async () => {
    const supervisor = new Supervisor({
      nodeId: "nod-0000000001",

      sandboxId: "sbx-0000000014",
      workloadType: "service",
      token,
      supervisorVersion: "test",
    });
    const workerMetadata = metadata("wrk-0000000022");
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
    const server = supervisor.serve({
      hostname: "127.0.0.1",
      port: 0,
      onListen: ({ port }) => resolvePort(port),
    });
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
the8020-internal-url: http://service/echo/main\r
the8020-internal-context-id: ctx-0000000002\r
the8020-internal-service-id: example/websocket/service\r
the8020-internal-service-generation: 2\r
the8020-internal-canonical-base-path: /example/websocket/service\r
the8020-internal-original-url: https://example.test/example/websocket/service/echo/main\r
the8020-internal-auth-authenticated: false\r
\r
`,
      ));
      const response = new TextDecoder().decode(
        await stream.until(new TextEncoder().encode("\r\n\r\n")),
      );
      assertEquals(response.startsWith("HTTP/1.1 101 "), true);
      assertEquals(
        await readWebSocketText(stream),
        "ready:main:ctx-0000000002:the8020.echo",
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
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000001",
    workloadType: "service",
    token,
    supervisorVersion: "test",
  });
  const healthy = await supervisor.startWorker({
    metadata: metadata("wrk-0000000004"),
    permissions: { read: [examples] },
  });
  const crashing = metadata("wrk-0000000003");
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
  assertEquals(status.recent_failures[0]?.worker_id, "wrk-0000000003");
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
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000002",
    workloadType: "service",
    token,
    supervisorVersion: "test",
  });
  const authMetadata = metadata("wrk-0000000002");
  authMetadata.entrypoint = new URL(
    "../examples/service_auth.ts",
    import.meta.url,
  ).href;
  const worker = await supervisor.startWorker({
    metadata: authMetadata,
    permissions: {
      read: [examples, new URL("../worker/testdata", import.meta.url).pathname],
    },
  });
  supervisor.configureService("service-a", [worker.metadata.workerId], 32);
  const response = await supervisor.handler(
    new Request("http://runtime/v1/services/service-a/dispatch", {
      method: "POST",
      headers: {
        authorization: `Bearer ${token}`,
        "the8020-internal-url": "http://service/auth",
        "the8020-internal-context-id": newId("ctx"),
        "the8020-internal-authentication": btoa(
          JSON.stringify({
            module:
              "data:application/typescript,export function authenticate() {}",
            claims: { sub: "user:admin" },
            unauthenticated: { action: "reject", status: 401 },
          }),
        ),
        "the8020-internal-auth-realm": "user",
        "the8020-internal-auth-user-id": "user:admin",
        "the8020-internal-auth-username": "admin",
        "the8020-internal-auth-version": "7",
        "the8020-internal-user-id": "user:admin",
        "the8020-internal-username": "admin",
      },
    }),
  );
  assertEquals(await response.json(), {
    auth: {
      authenticated: true,
      realm: "user",
      userId: "user:admin",
      username: "admin",
    },
    user: { userId: "user:admin", username: "admin" },
    execution: {
      nodeId: "nod-0000000001",

      sandboxId: "sbx-0000000002",
      workerId: worker.metadata.workerId,
    },
    internalHeaderVisible: false,
  });
  await supervisor.drain();
});

Deno.test("exact Worker control invokes only explicitly registered functions", async () => {
  const supervisor = new Supervisor({
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000003",
    workloadType: "service",
    token,
    supervisorVersion: "test",
  });
  const controlMetadata = metadata("wrk-0000000017");
  controlMetadata.entrypoint = new URL(
    "../examples/service_control.ts",
    import.meta.url,
  ).href;
  await supervisor.startWorker({
    metadata: controlMetadata,
    permissions: { read: [examples] },
  });
  const invoked = await supervisor.handler(
    new Request(
      `http://runtime/v1/workers/${controlMetadata.workerId}/invoke`,
      {
        method: "POST",
        headers: { authorization: `Bearer ${token}` },
        body: controlEnvelope("worker_invoke", {
          invocation: testInvocation(),
          function: "example.echo",
          input: { value: "ok" },
          user: { userId: "user:system", username: "system" },
        }).replace(
          '"sandbox_id":"sbx-0000000004"',
          '"sandbox_id":"sbx-0000000003"',
        ),
      },
    ),
  );
  const envelope = await invoked.json();
  assertEquals(envelope.payload, { ok: true, output: { value: "ok" } });

  const wrongWorker = await supervisor.handler(
    new Request("http://runtime/v1/workers/wrk-9999999999/invoke", {
      method: "POST",
      headers: { authorization: `Bearer ${token}` },
      body: controlEnvelope("worker_invoke", {
        invocation: testInvocation(),
        function: "example.echo",
        input: { value: "wrong target" },
        user: { userId: "user:system", username: "system" },
      }).replace(
        '"sandbox_id":"sbx-0000000004"',
        '"sandbox_id":"sbx-0000000003"',
      ),
    }),
  );
  assertEquals((await wrongWorker.json()).payload, {
    ok: false,
    error: {
      code: "target_not_found",
      message: "Worker wrk-9999999999 is unavailable",
    },
  });

  const unregistered = await supervisor.handler(
    new Request(
      `http://runtime/v1/workers/${controlMetadata.workerId}/invoke`,
      {
        method: "POST",
        headers: { authorization: `Bearer ${token}` },
        body: controlEnvelope("worker_invoke", {
          invocation: testInvocation(),
          function: "default",
          input: null,
          user: { userId: "user:system", username: "system" },
        }).replace(
          '"sandbox_id":"sbx-0000000004"',
          '"sandbox_id":"sbx-0000000003"',
        ),
      },
    ),
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

Deno.test("job dispatch forwards console logs and returns execution metadata without log copies", async () => {
  const logs = new TestLogSink();
  const checked: string[][] = [];
  const analyzed: string[][] = [];
  const supervisor = new Supervisor({
    logSink: logs,
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000004",
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
  const job = metadata("wrk-0000000018");
  job.workloadType = "job";
  job.workloadId = "job-a";
  job.origin = { type: "job", id: "job-a" };
  job.entrypoint = new URL("../examples/job.ts", import.meta.url).href;
  await supervisor.startWorker({
    metadata: job,
    permissions: { read: [examples] },
  });
  const response = await supervisor.handler(
    new Request(`http://runtime/v1/jobs/${job.workerId}/run`, {
      method: "POST",
      headers: {
        authorization: `Bearer ${token}`,
        "content-type": "application/json",
      },
      body: controlEnvelope("job_start", {
        invocation: testInvocation(),
        arguments: [{ value: 1 }],
        secrets: {},
        check_modules: [job.entrypoint],
      }),
    }),
  );
  const body = await response.json();
  assertEquals(response.status, 200);
  assertEquals(body.message_type, "job_result");
  assertEquals(body.correlation_id, "cor-0000000001");
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
    logs.records.filter((event) => event.component === "worker").map((event) =>
      event.message
    ),
    ['job input {"value":1}'],
  );
  const status = supervisor.workers().find((worker) =>
    worker.worker_id === job.workerId
  );
  assertEquals(Object.hasOwn(body.payload, "logs"), false);
  assertEquals(Object.hasOwn(status!, "logs"), false);
  await supervisor.drain();
});

Deno.test("job dispatch preserves structured command failures", async () => {
  const supervisor = new Supervisor({
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000004",
    workloadType: "job",
    token,
    supervisorVersion: "test",
  });
  const job = metadata("wrk-0000000019");
  job.workloadType = "job";
  job.workloadId = "job-error";
  job.origin = { type: "job", id: "job-error" };
  job.entrypoint = new URL("../examples/job_error.ts", import.meta.url).href;
  await supervisor.startWorker({
    metadata: job,
    permissions: { read: [examples] },
  });
  const response = await supervisor.handler(
    new Request(`http://runtime/v1/jobs/${job.workerId}/run`, {
      method: "POST",
      headers: {
        authorization: `Bearer ${token}`,
        "content-type": "application/json",
      },
      body: controlEnvelope("job_start", {
        invocation: testInvocation(),
        arguments: [],
        secrets: {},
      }),
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

Deno.test("persistent follow-ups reject wrong owners and cannot revive completed or expired bindings", async () => {
  let now = 1_000;
  const supervisor = new Supervisor({
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000010",
    workloadType: "service",
    token,
    supervisorVersion: "test",
    now: () => now,
    kernelCall: (call) => {
      if (call.operation === "execution.releaseWorker") {
        return Promise.resolve({ released: true });
      }
      throw new Error("completion must stay in its owning supervisor");
    },
  });
  const target = metadata("wrk-0000000013");
  target.workloadId = "route-service";
  target.entrypoint =
    new URL("../examples/service_control.ts", import.meta.url).href;
  target.service = {
    serviceId: "service-a",
    generation: 1,
    canonicalBasePath: "/service-a",
    executionMode: "persistent",
  };
  target.databaseAccess = "none";
  const worker = await supervisor.startWorker({
    metadata: target,
    permissions: { read: [examples] },
  });
  supervisor.configureService(target.workloadId, [target.workerId], 1);
  const dispatch = (
    executionId: string,
    existing: boolean,
    user = "alice",
    workerId: string | null = target.workerId,
    service = target.workloadId,
  ) =>
    supervisor.handler(
      new Request(`http://runtime/v1/services/${service}/dispatch`, {
        method: "POST",
        headers: {
          authorization: `Bearer ${token}`,
          "the8020-internal-url": "http://service/",
          "the8020-internal-context-id": newId("ctx"),
          "the8020-internal-persistent-execution-id": executionId,
          "the8020-internal-persistent-keep-alive-ms": "100",
          ...(workerId === null ? {} : {
            "the8020-internal-target-worker-id": workerId,
          }),
          "the8020-internal-user-id": `user:${user}`,
          "the8020-internal-username": user,
          ...(existing
            ? { "the8020-internal-persistent-existing": "true" }
            : {}),
        },
      }),
    );
  const expect = async (response: Promise<Response>, status: number) => {
    const value = await response;
    assertEquals(value.status, status);
    await value.body?.cancel();
  };
  try {
    await expect(dispatch("pex-0000000020", false), 204);
    await expect(dispatch("pex-0000000020", false), 409);
    await expect(dispatch("pex-0000000020", false, "bob"), 409);
    assertEquals(supervisor.workers()[0]?.persistent_executions, 1);
    await expect(dispatch("pex-0000000020", true), 204);
    await expect(dispatch("pex-0000000020", true, "alice", null), 409);
    await expect(dispatch("pex-0000000020", true, "bob"), 409);
    await expect(
      dispatch("pex-0000000020", true, "alice", "wrk-0000000008"),
      409,
    );
    await expect(
      dispatch(
        "pex-0000000020",
        true,
        "alice",
        target.workerId,
        "other-service",
      ),
      409,
    );
    await expect(dispatch("pex-0000000023", true), 409);
    assertEquals(supervisor.workers()[0]?.persistent_executions, 1);
    const completed = await supervisor.invokeWorker(
      worker.metadata.workerId,
      "example.complete-persistent",
      null,
      new AbortController().signal,
      "pex-0000000020",
      { userId: "user:alice", username: "alice" },
      testInvocation(),
    );
    assertEquals(completed.ok, true);
    await expect(dispatch("pex-0000000020", true), 409);
    assertEquals(supervisor.workers()[0]?.persistent_executions, 0);
    await expect(dispatch("pex-0000000021", false), 204);
    now += 100;
    await expect(dispatch("pex-0000000021", true), 409);
    assertEquals(supervisor.workers()[0]?.persistent_executions, 0);
    await expect(dispatch("pex-0000000022", false), 204);
    await expect(dispatch("pex-0000000020", true), 409);
    await expect(dispatch("pex-0000000021", true), 409);
    await expect(dispatch("pex-0000000022", true), 204);
    assertEquals(supervisor.workers()[0]?.persistent_executions, 1);
  } finally {
    await supervisor.stopWorker(target.workerId, true);
  }
});

Deno.test("zero keepalive retains detached executions without timer activity until completion", async () => {
  let now = 1_000;
  const supervisor = new Supervisor({
    nodeId: "nod-0000000001",
    sandboxId: "sbx-0000000010",
    workloadType: "service",
    token,
    supervisorVersion: "test",
    now: () => now,
  });
  const target = metadata("wrk-0000000013");
  target.workloadId = "retained-service";
  target.entrypoint =
    new URL("../examples/service_control.ts", import.meta.url).href;
  target.service = {
    serviceId: "service-a",
    generation: 1,
    canonicalBasePath: "/service-a",
    executionMode: "persistent",
  };
  const worker = await supervisor.startWorker({
    metadata: target,
    permissions: { read: [examples] },
  });
  supervisor.configureService(target.workloadId, [target.workerId], 1);
  const dispatch = (existing: boolean, keepAlive: string | null = "0") => {
    const headers = new Headers({
      authorization: `Bearer ${token}`,
      "the8020-internal-url": "http://service/",
      "the8020-internal-context-id": newId("ctx"),
      "the8020-internal-persistent-execution-id": "pex-0000000020",
      "the8020-internal-target-worker-id": target.workerId,
      "the8020-internal-user-id": "user:alice",
      "the8020-internal-username": "alice",
    });
    if (keepAlive !== null) {
      headers.set("the8020-internal-persistent-keep-alive-ms", keepAlive);
    }
    if (existing) headers.set("the8020-internal-persistent-existing", "true");
    return supervisor.handler(
      new Request(`http://runtime/v1/services/${target.workloadId}/dispatch`, {
        method: "POST",
        headers,
      }),
    );
  };
  try {
    for (const invalid of [null, "", "-1"]) {
      const response = await dispatch(false, invalid);
      assertEquals(response.status >= 400, true);
      await response.body?.cancel();
      assertEquals(supervisor.workers()[0]?.persistent_executions, 0);
    }
    const initial = await dispatch(false);
    assertEquals(initial.status, 204);
    await initial.body?.cancel();
    now += 365 * 24 * 60 * 60 * 1_000;
    assertEquals(supervisor.workers()[0]?.persistent_executions, 1);
    assertEquals(supervisor.workers()[0]?.idle_since_ms, undefined);
    // A follow-up cannot replace the lifetime chosen by its initial request.
    const resumed = await dispatch(true, "1");
    assertEquals(resumed.status, 204);
    await resumed.body?.cancel();
    now += 365 * 24 * 60 * 60 * 1_000;
    assertEquals(supervisor.workers()[0]?.persistent_executions, 1);
    const result = await supervisor.invokeWorker(
      worker.metadata.workerId,
      "example.complete-persistent",
      null,
      new AbortController().signal,
      "pex-0000000020",
      { userId: "user:alice", username: "alice" },
      testInvocation(),
    );
    assertEquals(result.ok, true);
    assertEquals(supervisor.workers()[0]?.persistent_executions, 0);
    assertEquals(supervisor.workers()[0]?.idle_since_ms, now);
    const stale = await dispatch(true);
    assertEquals(stale.status, 409);
    await stale.body?.cancel();
  } finally {
    await supervisor.stopWorker(target.workerId, true);
  }
});

Deno.test("accepted persistent work survives loss of its establishment response", async () => {
  const supervisor = new Supervisor({
    nodeId: "nod-0000000001",
    sandboxId: "sbx-0000000010",
    workloadType: "service",
    token,
    supervisorVersion: "test",
  });
  const target = metadata("wrk-0000000013");
  target.workloadId = "retained-creation";
  target.entrypoint =
    new URL("../examples/service_retained.ts", import.meta.url).href;
  target.service = {
    serviceId: "service-a",
    generation: 1,
    canonicalBasePath: "/service-a",
    executionMode: "persistent",
  };
  target.databaseAccess = "none";
  await supervisor.startWorker({
    metadata: target,
    permissions: { read: [examples] },
  });
  supervisor.configureService(target.workloadId, [target.workerId], 1);
  const connection = new AbortController();
  const signal = AbortSignal.timeout(5_000);
  const first = supervisor.handler(
    new Request(`http://runtime/v1/services/${target.workloadId}/dispatch`, {
      method: "POST",
      signal: connection.signal,
      headers: {
        authorization: `Bearer ${token}`,
        "the8020-internal-url": "http://service/",
        "the8020-internal-context-id": newId("ctx"),
        "the8020-internal-persistent-execution-id": "pex-0000000020",
        "the8020-internal-persistent-keep-alive-ms": "0",
      },
    }),
  );
  const invoke = (name: string) =>
    supervisor.invokeWorker(target.workerId, name, null, signal, undefined, {
      userId: "user:system",
      username: "system",
    }, testInvocation());
  try {
    assertEquals((await invoke("example.when-retained")).ok, true);
    connection.abort();
    const lost = await first;
    await lost.body?.cancel();
    assertEquals(supervisor.workers()[0]?.persistent_executions, 1);
    assertEquals(supervisor.workers()[0]?.idle_since_ms, undefined);
    assertEquals((await invoke("example.release")).ok, true);
    assertEquals(supervisor.workers()[0]?.persistent_executions, 0);
  } finally {
    connection.abort();
    await supervisor.stopWorker(target.workerId, true);
  }
});

Deno.test("persistent HTTP response streams retain their binding through consumption", async () => {
  let now = 1_000;
  const supervisor = new Supervisor({
    nodeId: "nod-0000000001",

    sandboxId: "sbx-0000000012",
    workloadType: "service",
    token,
    supervisorVersion: "test",
    now: () => now,
  });
  const target = metadata("wrk-0000000014");
  target.service = {
    serviceId: target.workloadId,
    generation: 1,
    canonicalBasePath: "/service-a",
    executionMode: "persistent",
  };
  await supervisor.startWorker({
    metadata: target,
    permissions: { read: [examples] },
  });
  supervisor.configureService(target.workloadId, [target.workerId], 1);
  const dispatch = (existing: boolean) =>
    supervisor.handler(
      new Request(`http://runtime/v1/services/${target.workloadId}/dispatch`, {
        method: "POST",
        headers: {
          authorization: `Bearer ${token}`,
          "the8020-internal-url": "http://service/sse",
          "the8020-internal-context-id": newId("ctx"),
          "the8020-internal-persistent-execution-id": "pex-0000000024",
          "the8020-internal-persistent-keep-alive-ms": "100",
          ...(existing
            ? {
              "the8020-internal-persistent-existing": "true",
              "the8020-internal-target-worker-id": target.workerId,
            }
            : {}),
        },
      }),
    );
  try {
    const initial = await dispatch(false);
    await new Promise((resolve) => setTimeout(resolve, 10));
    now += 10_000;
    assertEquals(supervisor.workers()[0]?.persistent_executions, 1);
    assertEquals(await initial.text(), "event: ready\ndata: streamed\n\n");
    now += 99;
    const followup = await dispatch(true);
    assertEquals(followup.status, 201);
    await followup.body?.cancel();
    now += 100;
    const expired = await dispatch(true);
    assertEquals(expired.status, 409);
    await expired.body?.cancel();
    assertEquals(supervisor.workers()[0]?.persistent_executions, 0);
  } finally {
    await supervisor.stopWorker(target.workerId, true);
  }
});

function testInvocation() {
  return { contextId: newId("ctx"), jobRunId: newId("job") };
}

Deno.test("supervisor validates operational identities before registration", async () => {
  const options = {
    nodeId: newId("nod"),
    sandboxId: newId("sbx"),
    workloadType: "service" as const,
    token,
    supervisorVersion: "test",
  };
  for (
    const invalid of [
      { nodeId: "node-test" },
      { sandboxId: "sbx-short" },
      { sandboxId: newId("wrk") },
    ]
  ) {
    await assertRejects(
      () => Promise.resolve(new Supervisor({ ...options, ...invalid })),
      TypeError,
      "canonical node/sandbox IDs",
    );
  }
  const supervisor = new Supervisor(options);
  await assertRejects(
    () =>
      supervisor.startWorker({
        metadata: metadata("wrk-short"),
        permissions: { read: [examples] },
      }),
    TypeError,
    "canonical Worker ID",
  );
  assertEquals(supervisor.status().worker_count, 0);
  await supervisor.drain();
});

Deno.test("service HTTP and WebSocket ingress validate contexts and preserve log parents", async () => {
  const logs = new TestLogSink();
  const supervisor = new Supervisor({
    logSink: logs,
    nodeId: newId("nod"),
    sandboxId: newId("sbx"),
    workloadType: "service",
    token,
    supervisorVersion: "test",
  });
  const target = metadata(newId("wrk"));
  target.entrypoint =
    new URL("../worker/testdata/logging_service.ts", import.meta.url).href;
  await supervisor.startWorker({
    metadata: target,
    permissions: { read: [new URL("../worker", import.meta.url).pathname] },
  });
  supervisor.configureService(target.workloadId, [target.workerId], 8);
  const dispatch = (
    identifiers: Record<string, string>,
    username = "alice",
    websocket = false,
  ) =>
    supervisor.handler(
      new Request(
        `http://runtime/v1/services/${target.workloadId}/${
          websocket ? "websocket" : "dispatch"
        }`,
        {
          method: websocket ? "GET" : "POST",
          headers: {
            authorization: `Bearer ${token}`,
            "the8020-internal-url": "http://service/?delay=20",
            "the8020-internal-user-id": `user:${username}`,
            "the8020-internal-username": username,
            ...identifiers,
          },
        },
      ),
    );
  try {
    const invalid: Record<string, string>[] = [
      {},
      { "the8020-internal-context-id": "" },
      { "the8020-internal-context-id": "ctx-short" },
      { "the8020-internal-context-id": newId("job") },
      {
        "the8020-internal-context-id": newId("ctx"),
        "the8020-internal-parent-context-id": "ctx-short",
      },
      {
        "the8020-internal-context-id": newId("ctx"),
        "the8020-internal-persistent-execution-id": "pex-short",
      },
    ];
    for (const headers of invalid) {
      for (const websocket of [false, true]) {
        const response = await dispatch(headers, "alice", websocket);
        assertEquals(response.status, 400);
        await response.body?.cancel();
      }
    }
    assertEquals(
      logs.records.filter((record) => record.component === "worker").length,
      0,
    );
    assertEquals(supervisor.snapshot().active_requests, 0);

    const invocations = ["alice", "bobby"].map((username) => ({
      username,
      contextId: newId("ctx"),
      parentContextId: newId("ctx"),
    }));
    const responses = await Promise.all(
      invocations.map((invocation) =>
        dispatch({
          "the8020-internal-context-id": invocation.contextId,
          "the8020-internal-parent-context-id": invocation.parentContextId,
        }, invocation.username)
      ),
    );
    for (const [index, response] of responses.entries()) {
      const invocation = invocations[index]!;
      assertEquals(response.status, 200);
      assertEquals(await response.json(), {
        username: invocation.username,
        contextId: invocation.contextId,
      });
      const records = logs.records.filter((record) =>
        record.context_id === invocation.contextId
      );
      assertEquals(records.map((record) => record.message), [
        `begin ${invocation.username}`,
        `end ${invocation.username}`,
      ]);
      assertEquals(
        records.every((record) =>
          record.parent_context_id === invocation.parentContextId &&
          record.username === invocation.username &&
          record.worker_id === target.workerId &&
          record.sandbox_id === supervisor.options.sandboxId &&
          record.node_id === supervisor.options.nodeId
        ),
        true,
      );
    }
  } finally {
    await supervisor.drain();
  }
});
