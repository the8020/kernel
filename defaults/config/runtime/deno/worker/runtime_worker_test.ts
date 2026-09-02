import { assertEquals, assertRejects } from "../test/assert.ts";
import { RuntimeWorker } from "./runtime_worker.ts";
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
    releaseId: "test",
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
    assertEquals(await worker.runJob({ value: 1 }), {
      input: { value: 1 },
      executionCount: 1,
    });
    assertEquals(worker.logs.map((event) => event.message), [
      "execution 1",
      'job input {"value":1}',
    ]);
    assertEquals(await worker.runJob("again"), {
      input: "again",
      executionCount: 2,
    });
    assertEquals(worker.logs.map((event) => event.message), [
      "execution 2",
      "job input again",
    ]);
  } finally {
    await worker.stop();
    await new Promise((resolve) => setTimeout(resolve, 0));
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
    auth: {
      authenticated: true,
      realm: "bootstrap-admin",
      userId: "bootstrap-admin:Admin",
      username: "Admin",
      authVersion: 1,
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

Deno.test("service Worker bridges typed kernel authentication calls", async () => {
  const calls: KernelCallRequest[] = [];
  const worker = new RuntimeWorker({
    metadata: metadata("service", example("service_kernel"), "kernel"),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
    kernelCall: (call) => {
      calls.push(call);
      if (call.operation === "auth.bootstrapLogin") {
        return Promise.resolve({
          authenticated: true,
          user: {
            id: "bootstrap-admin:Admin",
            username: "Admin",
            realm: "bootstrap-admin",
          },
          setCookie: "the8020_auth=opaque; HttpOnly; Path=/",
        });
      }
      if (call.operation === "admin.execute") {
        return Promise.resolve({
          protocol_version: 1,
          success: true,
          result: { services: [{ service_id: "core/example/service" }] },
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
    auth: { authenticated: false },
  };
  try {
    const login = await worker.dispatchService(
      new Request("http://service/login", {
        method: "POST",
        body: JSON.stringify({ username: "Admin", password: "private" }),
      }),
      requestMetadata,
    );
    assertEquals((await login.json()).authenticated, true);
    assertEquals(calls[0], {
      operation: "auth.bootstrapLogin",
      arguments: { username: "Admin", password: "private" },
      requestId: "request-auth-1",
      serviceId: "example/auth/login",
      executionId: "execution-kernel",
      workerId: "worker-kernel",
      persistentExecutionId: undefined,
    });
    await worker.dispatchService(
      new Request("http://service/logout"),
      { ...requestMetadata, requestId: "request-auth-2" },
    );
    const applicationCalls = () =>
      calls.filter((call) => call.operation !== "database.scope.close");
    assertEquals(applicationCalls()[1]?.operation, "auth.logoutCurrent");
    assertEquals(applicationCalls()[1]?.requestId, "request-auth-2");
    const admin = await worker.dispatchService(
      new Request("http://service/admin"),
      { ...requestMetadata, requestId: "request-admin-1" },
    );
    assertEquals(await admin.json(), {
      services: [{ service_id: "core/example/service" }],
    });
    assertEquals(applicationCalls()[2]?.operation, "admin.execute");
    assertEquals(applicationCalls()[2]?.arguments, {
      command_id: "service.list",
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

Deno.test("Worker permissions deny undeclared host reads", async () => {
  const worker = new RuntimeWorker({
    metadata: metadata("job", example("denied"), "denied"),
    permissions: { read: [new URL("../examples", import.meta.url).pathname] },
  });
  try {
    await assertRejects(
      () => worker.runJob(null),
      Error,
      "Requires read access",
    );
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
    assertEquals(await worker.runJob(null), "nested-ok");
  } finally {
    worker.kill();
  }
});
