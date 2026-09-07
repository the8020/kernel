import { assertEquals, assertRejects } from "../test/assert.ts";
import { context } from "../context/mod.ts";
import type {
  ExecutionMetadata,
  ServiceRequestMetadata,
} from "../worker/contracts.ts";
import { createKernelBridge } from "./bridge.ts";
import {
  AdminCommandError,
  kernel,
  kernelDatabaseBackend,
  parseCommandArguments,
  requiredCommandArgument,
  TerminalControlBusyError,
} from "./mod.ts";

const metadata: ServiceRequestMetadata = {
  contextId: "request-1",
  serviceId: "example/auth/login",
  serviceGeneration: 1,
  canonicalBasePath: "/example/auth/login",
  originalUrl: "https://example.test/example/auth/login",
  client: { ipAddress: "203.0.113.4", networkScope: "public" },
  execution: {
    nodeId: "node-1",

    sandboxId: "sbx-test0001",
    workerId: "wrk-test0001",
  },
  user: { userId: "user:system", username: "system" },
  auth: { authenticated: false },
};

const workerMetadata: ExecutionMetadata = {
  nodeId: "node-1",

  sandboxId: "sbx-test0001",
  workerId: "wrk-test0001",

  workloadType: "service",
  ownerId: "example/auth/login",
  workloadId: "example/auth/login",
  releaseId: "test",
  entrypoint: "file:///workspace/service.ts",
  debuggerName: "service:example/auth/login:execution-1:wrk-test0001",
  databaseBackend: "postgresql",
  user: { userId: "user:system", username: "system" },
  origin: { type: "service", id: "example/auth/login" },
};

Deno.test("package command argument helpers return structured failures", () => {
  for (
    const action of [
      () => requiredCommandArgument([], 0, "service ID"),
      () => parseCommandArguments(["--unknown"], { values: ["known"] }),
    ]
  ) {
    try {
      action();
      throw new Error("invalid arguments unexpectedly succeeded");
    } catch (error) {
      assertEquals(error instanceof AdminCommandError, true);
      assertEquals((error as AdminCommandError).code, "invalid_arguments");
    }
  }
});

Deno.test("the bridge rejects missing execution users instead of defaulting", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1, workerMetadata);
  let called = false;
  try {
    const invalid = {
      ...metadata,
      user: undefined,
    } as unknown as ServiceRequestMetadata;
    await assertRejects(
      async () => {
        await bridge.withRequest(invalid, () => {
          called = true;
        });
      },
      TypeError,
      "execution user",
    );
    await assertRejects(
      async () => {
        await bridge.withExecution(invalid, () => {
          called = true;
        });
      },
      TypeError,
      "execution user",
    );
    assertEquals(called, false);
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});
const persistentMetadata: ServiceRequestMetadata = {
  ...metadata,
  persistentExecutionId: "persistent-test",
  persistentKeepAliveMilliseconds: 60_000,
};

function createCallQueue(port: MessagePort) {
  const queued: Array<Record<string, unknown>> = [];
  const waiting: Array<(call: Record<string, unknown>) => void> = [];
  port.onmessage = (event) => {
    const call = event.data as Record<string, unknown>;
    const resolve = waiting.shift();
    if (resolve === undefined) queued.push(call);
    else resolve(call);
  };
  port.start();
  return {
    next(): Promise<Record<string, unknown>> {
      const call = queued.shift();
      if (call !== undefined) return Promise.resolve(call);
      return new Promise((resolve) => waiting.push(resolve));
    },
  };
}

Deno.test("cryptographic operations use the existing request bridge", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1, workerMetadata);
  const calls = createCallQueue(channel.port2);
  try {
    const pending = bridge.withRequest(
      metadata,
      () => kernel.crypto.sign(new Uint8Array([1, 2, 3])),
    );
    const call = await calls.next();
    assertEquals(
      (call.payload as { operation: string }).operation,
      "runtime.operation",
    );
    assertEquals((call.payload as { arguments: unknown }).arguments, {
      operation: "crypto.sign",
      input: { data: "AQID" },
    });
    bridge.handle({
      type: "kernel_result",
      correlationId: call.correlationId as string,
      payload: { success: true, result: { signature: "signed" } },
    });
    assertEquals(await pending, "signed");
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("terminal bytes cross the bridge intact and disconnect still permits detach", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1, workerMetadata);
  const calls = createCallQueue(channel.port2);
  const control = new AbortController();
  const bytes = Uint8Array.from({ length: 65_536 }, (_, i) => i % 256);
  const reply = (call: Record<string, unknown>, result: unknown) =>
    bridge.handle({
      type: "kernel_result",
      correlationId: call.correlationId as string,
      payload: { success: true, result },
    });
  try {
    const written = bridge.withRequest(
      metadata,
      () => kernel.terminals.write("att-aaaaaaaaaa", bytes),
      control.signal,
    );
    const write = await calls.next();
    const payload = write.payload as {
      arguments: { operation: string; input: { data: string } };
    };
    assertEquals(payload.arguments.operation, "terminal.write");
    assertEquals(
      Uint8Array.from(
        atob(payload.arguments.input.data),
        (c) => c.charCodeAt(0),
      ),
      bytes,
    );
    reply(write, null);
    await written;
    const reading = bridge.withRequest(
      metadata,
      () => kernel.terminals.read("att-bbbbbbbbbb", 7),
      control.signal,
    );
    const read = await calls.next();
    reply(read, {
      events: [{ sequence: 8, data: payload.arguments.input.data }],
      sequence: 8,
      exited: false,
    });
    assertEquals((await reading).events[0]?.data, bytes);
    const closing = bridge.withRequest(
      metadata,
      () =>
        kernel.terminals.close({
          terminalId: "tty-aaaaaaaaaa",
          nodeId: "nod-bbbbbbbbbb",
        }),
    );
    const close = await calls.next();
    assertEquals((close.payload as { arguments: unknown }).arguments, {
      operation: "terminal.close",
      input: { terminalId: "tty-aaaaaaaaaa", nodeId: "nod-bbbbbbbbbb" },
    });
    reply(close, null);
    await closing;
    control.abort(new Error("terminal view disconnected"));
    await assertRejects(
      () =>
        bridge.withRequest(
          metadata,
          () => kernel.terminals.write("att-aaaaaaaaaa", new Uint8Array([1])),
          control.signal,
        ),
      Error,
      "terminal view disconnected",
    );
    const detached = bridge.withRequest(
      metadata,
      () => kernel.terminals.detach("att-aaaaaaaaaa"),
      control.signal,
    );
    // An already-aborted call can emit kernel_cancel, but must emit no input.
    let detach = await calls.next();
    if (detach.type === "kernel_cancel") detach = await calls.next();
    assertEquals((detach.payload as { arguments: unknown }).arguments, {
      operation: "terminal.detach",
      input: { attachmentId: "att-aaaaaaaaaa" },
    });
    reply(detach, null);
    await detached;
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("terminal control contention is typed and native display waits cancel through the bridge", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1, workerMetadata);
  const calls = createCallQueue(channel.port2);
  try {
    const pending = bridge.withRequest(
      metadata,
      () => kernel.terminals.attach("tty-aaaaaaaaaa", "control"),
    );
    const call = await calls.next();
    bridge.handle({
      type: "kernel_result",
      correlationId: call.correlationId as string,
      payload: { success: true, result: { busy: true } },
    });
    await assertRejects(
      () => pending,
      TerminalControlBusyError,
      "input controller",
    );
    const stop = new AbortController();
    const waiting = bridge.withRequest(
      metadata,
      () => kernel.terminals.nextView("att-aaaaaaaaaa", stop.signal),
    );
    const next = await calls.next();
    assertEquals((next.payload as { arguments: unknown }).arguments, {
      operation: "terminal.view-next",
      input: { attachmentId: "att-aaaaaaaaaa" },
    });
    stop.abort(new Error("native owner ended"));
    await assertRejects(() => waiting, Error, "native owner ended");
    const cancelled = await calls.next();
    assertEquals(cancelled.type, "kernel_cancel");
    assertEquals(cancelled.correlationId, next.correlationId);
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("authentication context is synchronous and never calls the kernel", () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1, workerMetadata);
  try {
    assertEquals(
      bridge.withRequest({
        ...persistentMetadata,
        auth: { authenticated: true },
      }, () => context.authenticated),
      true,
    );
    assertEquals(
      bridge.withRequest(metadata, () => context.authenticated),
      false,
    );
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("persistent handlers retain approved identity while transport cancellation stays local", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1, workerMetadata);
  const calls = createCallQueue(channel.port2);
  const connection = new AbortController();
  const ready = Promise.withResolvers<void>();
  const release = Promise.withResolvers<void>();
  const reply = (call: Record<string, unknown>, payload: unknown) =>
    bridge.handle({
      type: "kernel_result",
      correlationId: call.correlationId as string,
      payload,
    });
  try {
    await assertRejects(
      () =>
        bridge.withRequest(
          persistentMetadata,
          () => kernel.execution.runPersistent(async () => {}),
        ),
      Error,
      "zero-keepalive",
    );
    const running = bridge.withRequest({
      ...persistentMetadata,
      persistentKeepAliveMilliseconds: 0,
    }, () =>
      kernel.execution.runPersistent(async () => {
        ready.resolve();
        await release.promise;
        assertEquals(context.userId, "user:system");
        await kernel.terminals.inspect("tty-aaaaaaaaaa");
      }), connection.signal);
    const claim = await calls.next();
    assertEquals(
      (claim.payload as { operation: string }).operation,
      "execution.retainPersistent",
    );
    reply(claim, { retained: true, contextId: "ctx-aaaaaaaaaa" });
    await ready.promise;
    connection.abort();
    release.resolve();
    const inspect = await calls.next();
    assertEquals((inspect.payload as { arguments: unknown }).arguments, {
      operation: "terminal.inspect",
      input: { terminalId: "tty-aaaaaaaaaa" },
    });
    reply(inspect, { success: true, result: {} });
    const cleanup = await calls.next();
    const cleanupPayload = cleanup.payload as {
      operation: string;
      request: { contextId: string; parentContextId: string };
    };
    assertEquals(cleanupPayload.operation, "database.scope.close");
    assertEquals(cleanupPayload.request.contextId, "ctx-aaaaaaaaaa");
    assertEquals(
      cleanupPayload.request.parentContextId,
      persistentMetadata.contextId,
    );
    reply(cleanup, { closed: true });
    const complete = await calls.next();
    assertEquals(
      (complete.payload as { operation: string }).operation,
      "execution.completePersistent",
    );
    reply(complete, { completed: true });
    await running;
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("persistent completion and exact Worker calls use the generic bridge", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1, workerMetadata);
  const calls = createCallQueue(channel.port2);
  try {
    const [completion, invocation] = bridge.withRequest(
      persistentMetadata,
      () =>
        [
          kernel.execution.completePersistent(),
          kernel.worker.invoke({
            nodeId: "node-2",
            sandboxId: "sbx-target01",
            workerId: "wrk-target01",
            function: "package.inspect",
            input: { id: "one" },
          }),
        ] as const,
    );
    const completionCall = await calls.next();
    const invocationCall = await calls.next();
    assertEquals(
      (completionCall.payload as { operation: string }).operation,
      "execution.completePersistent",
    );
    assertEquals(
      (invocationCall.payload as { operation: string }).operation,
      "worker.invoke",
    );
    for (
      const [call, payload] of [
        [completionCall, undefined],
        [invocationCall, { ok: true, output: { id: "one" } }],
      ] as const
    ) {
      bridge.handle({
        type: "kernel_result",
        correlationId: call.correlationId as string,
        payload,
      });
    }
    await completion;
    assertEquals(await invocation, { id: "one" });
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("typed kernel admin bridge returns results and command errors", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1, workerMetadata);
  const calls = createCallQueue(channel.port2);
  const authenticated: ServiceRequestMetadata = {
    ...persistentMetadata,
    auth: {
      authenticated: true,
      realm: "user",
      userId: "user-1",
      username: "Admin",
    },
  };
  try {
    const list = bridge.withRequest(
      authenticated,
      () => kernel.admin.execute<{ services: unknown[] }>("service.list"),
    );
    const listCall = await calls.next();
    assertEquals(
      (listCall.payload as { operation: string }).operation,
      "admin.execute",
    );
    assertEquals(
      (listCall.payload as { arguments: unknown }).arguments,
      { command_id: "service.list", arguments: {} },
    );
    bridge.handle({
      type: "kernel_result",
      correlationId: listCall.correlationId as string,
      payload: {
        protocol_version: 2,
        success: true,
        request_id: "command-1",
        result: { services: [{ service_id: "core/example/service" }] },
      },
    });
    assertEquals(await list, {
      services: [{ service_id: "core/example/service" }],
    });

    const comparison = bridge.withRequest(
      authenticated,
      () => kernel.database.tables.compare("acme__orders__orders"),
    );
    const comparisonCall = await calls.next();
    assertEquals(
      (comparisonCall.payload as { arguments: unknown }).arguments,
      {
        operation: "database.table.compare",
        input: { table_id: "acme__orders__orders" },
      },
    );
    bridge.handle({
      type: "kernel_result",
      correlationId: comparisonCall.correlationId as string,
      payload: {
        success: true,
        result: { table: { table_id: "acme__orders__orders" } },
      },
    });
    assertEquals(await comparison, { table_id: "acme__orders__orders" });

    const missing = bridge.withRequest(
      authenticated,
      () =>
        kernel.admin.execute("service.inspect", {
          service_id: "missing/service/id",
        }),
    );
    const missingCall = await calls.next();
    const executionReference = {
      program_id: "acme/tools/inspect",
      execution_id: "job-abcdefghij",
      node_id: "nod-abcdefghij",
      sandbox_id: "sbx-abcdefghij",
      context_id: "ctx-abcdefghij",
      log_position: "before-command",
      queued_at: "2026-09-06T10:00:00Z",
      finished_at: "2026-09-06T10:00:01Z",
    };
    bridge.handle({
      type: "kernel_result",
      correlationId: missingCall.correlationId as string,
      payload: {
        protocol_version: 2,
        success: false,
        request_id: "command-2",
        error: { code: "not_found", message: "service not found" },
        execution: executionReference,
      },
    });
    try {
      await missing;
      throw new Error("expected admin command failure");
    } catch (error) {
      if (!(error instanceof AdminCommandError)) throw error;
      assertEquals(error.code, "not_found");
      assertEquals(error.requestId, "command-2");
      assertEquals(error.execution, executionReference);
    }
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("typed database bridge uses one execute operation", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1, workerMetadata);
  const calls = createCallQueue(channel.port2);
  try {
    const query = bridge.withRequest(
      metadata,
      () => kernel.database.execute("SELECT $1", [7], { returnRows: true }),
    );
    const queryCall = await calls.next();
    assertEquals(
      (queryCall.payload as { operation: string }).operation,
      "database.execute",
    );
    assertEquals(
      (queryCall.payload as { arguments: unknown }).arguments,
      { statement: "SELECT $1", parameters: [7], return_rows: true },
    );
    bridge.handle({
      type: "kernel_result",
      correlationId: queryCall.correlationId as string,
      payload: { columns: ["value"], rows: [[7]] },
    });
    assertEquals(await query, {
      columns: ["value"],
      rows: [[7]],
    });

    const execute = bridge.withRequest(
      metadata,
      () => kernel.database.execute("DELETE FROM example"),
    );
    const executeCall = await calls.next();
    assertEquals(
      (executeCall.payload as { operation: string }).operation,
      "database.execute",
    );
    bridge.handle({
      type: "kernel_result",
      correlationId: executeCall.correlationId as string,
      payload: { columns: [], rows: [], affected_rows: 2 },
    });
    assertEquals(await execute, { columns: [], rows: [], affected_rows: 2 });

    const insert = bridge.withRequest(
      metadata,
      () =>
        kernel.database.execute("INSERT INTO example DEFAULT VALUES", [], {
          returnInsertId: true,
        }),
    );
    const insertCall = await calls.next();
    assertEquals(
      (insertCall.payload as { arguments: unknown }).arguments,
      {
        statement: "INSERT INTO example DEFAULT VALUES",
        parameters: [],
        return_insert_id: true,
      },
    );
    bridge.handle({
      type: "kernel_result",
      correlationId: insertCall.correlationId as string,
      payload: {
        columns: [],
        rows: [],
        insert_id: { type: "bigint", value: "1" },
      },
    });
    assertEquals(await insert, {
      columns: [],
      rows: [],
      insert_id: { type: "bigint", value: "1" },
    });
    await assertRejects(
      () => bridge.withRequest(metadata, () => kernel.database.execute("", [])),
      TypeError,
      "SQL statement is required",
    );
    await assertRejects(
      () =>
        bridge.withRequest(
          metadata,
          () => kernel.database.execute("SELECT $1", [{}] as never),
        ),
      TypeError,
      "SQL parameters must be an array",
    );
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("interleaved asynchronous calls retain their exact request", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1, workerMetadata);
  const calls = createCallQueue(channel.port2);
  let releaseFirst!: () => void;
  let releaseSecond!: () => void;
  const firstGate = new Promise<void>((resolve) => releaseFirst = resolve);
  const secondGate = new Promise<void>((resolve) => releaseSecond = resolve);
  const firstMetadata = { ...metadata, contextId: "request-first" };
  const secondMetadata = { ...metadata, contextId: "request-second" };
  try {
    const first = bridge.withRequest(firstMetadata, async () => {
      await firstGate;
      return await kernel.database.execute("SELECT 'first'", [], {
        returnRows: true,
      });
    });
    const second = bridge.withRequest(secondMetadata, async () => {
      await secondGate;
      await Promise.resolve();
      return await kernel.database.execute("SELECT 'second'", [], {
        returnRows: true,
      });
    });
    releaseSecond();
    const secondCall = await calls.next();
    releaseFirst();
    const firstCall = await calls.next();
    assertEquals(
      (secondCall.payload as { request: { contextId: string } }).request
        .contextId,
      "request-second",
    );
    assertEquals(
      (firstCall.payload as { request: { contextId: string } }).request
        .contextId,
      "request-first",
    );
    for (const call of [secondCall, firstCall]) {
      bridge.handle({
        type: "kernel_result",
        correlationId: call.correlationId as string,
        payload: { columns: ["value"], rows: [[1]] },
      });
    }
    await Promise.all([first, second]);
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("overlapping persistent requests retain isolated immutable contexts", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1, workerMetadata);
  const calls = createCallQueue(channel.port2);
  let resume!: () => void;
  const suspended = new Promise<void>((resolve) => resume = resolve);
  const authenticated: ServiceRequestMetadata = {
    ...persistentMetadata,
    contextId: "request-establish",
    auth: {
      authenticated: true,
      realm: "user",
      userId: "user-1",
      username: "Admin",
    },
  };
  try {
    const program = bridge.withRequest(authenticated, async () => {
      await suspended;
      return await kernel.admin.execute("service.list");
    });
    bridge.withRequest(
      { ...authenticated, contextId: "request-websocket" },
      () => undefined,
    );
    const control = bridge.withExecution(
      {
        contextId: "request-control",
        serviceId: "service-version-a",
        persistentExecutionId: "persistent-test",
        user: workerMetadata.user,
      },
      () => kernel.admin.execute("service.inspect"),
    );
    const controlCall = await calls.next();
    assertEquals(
      (controlCall.payload as { request: { contextId: string } }).request
        .contextId,
      "request-control",
    );
    bridge.handle({
      type: "kernel_result",
      correlationId: controlCall.correlationId as string,
      payload: {
        protocol_version: 2,
        success: true,
        result: { service: {} },
      },
    });
    await control;
    resume();
    const call = await calls.next();
    assertEquals(
      (call.payload as { request: { contextId: string } }).request.contextId,
      "request-establish",
    );
    bridge.handle({
      type: "kernel_result",
      correlationId: call.correlationId as string,
      payload: {
        protocol_version: 2,
        success: true,
        result: { services: [] },
      },
    });
    assertEquals(await program, { services: [] });
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("overlapping persistent database calls keep exact request scopes", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1, workerMetadata);
  const calls = createCallQueue(channel.port2);
  const base: ServiceRequestMetadata = {
    ...persistentMetadata,
    persistentExecutionId: "persistent-shared",
  };
  try {
    const first = bridge.withRequest(
      { ...base, contextId: "request-first" },
      () => kernel.database.execute("SELECT 1", [], { returnRows: true }),
    );
    const second = bridge.withRequest(
      { ...base, contextId: "request-second" },
      () => kernel.database.execute("SELECT 2", [], { returnRows: true }),
    );
    const pending = [await calls.next(), await calls.next()];
    assertEquals(
      new Set(
        pending.map((call) =>
          (call.payload as { request: { contextId: string } }).request.contextId
        ),
      ),
      new Set(["request-first", "request-second"]),
    );
    for (const call of pending.reverse()) {
      bridge.handle({
        type: "kernel_result",
        correlationId: call.correlationId as string,
        payload: { columns: ["value"], rows: [[1]] },
      });
    }
    await Promise.all([first, second]);
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("request cancellation cancels its exact pending kernel call", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1, workerMetadata);
  const calls = createCallQueue(channel.port2);
  const controller = new AbortController();
  try {
    const query = bridge.withRequest(
      metadata,
      () => kernel.database.execute("SELECT 1", [], { returnRows: true }),
      controller.signal,
    );
    const call = await calls.next();
    assertEquals(call.type, "kernel_call");
    controller.abort(new DOMException("request ended", "AbortError"));
    const cancellation = await calls.next();
    assertEquals(cancellation, {
      type: "kernel_cancel",
      correlationId: call.correlationId,
    });
    await assertRejects(() => query, Error, "request ended");
    assertEquals(
      bridge.handle({
        type: "kernel_result",
        correlationId: call.correlationId as string,
        payload: { columns: ["value"], rows: [[1]] },
      }),
      true,
    );
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("log query cancellation affects only its own pending call", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1, workerMetadata);
  const calls = createCallQueue(channel.port2);
  const execution = new AbortController(), queryAbort = new AbortController();
  try {
    const input = {
      node_id: "nod-0123456789",
      position: "before-job",
      username: "alice",
      limit: 20,
    };
    const query = bridge.withRequest(
      metadata,
      () => kernel.logs.query(input, queryAbort.signal),
      execution.signal,
    );
    const rejected = assertRejects(() => query, Error, "view closed");
    const call = await calls.next();
    assertEquals((call.payload as { arguments: unknown }).arguments, {
      operation: "logs.query",
      input,
    });
    const sibling = bridge.withRequest(
      metadata,
      () => kernel.logs.query({ tail: true }),
      execution.signal,
    );
    const siblingCall = await calls.next();
    queryAbort.abort(new DOMException("view closed", "AbortError"));
    assertEquals(await calls.next(), {
      type: "kernel_cancel",
      correlationId: call.correlationId,
    });
    await rejected;
    assertEquals(execution.signal.aborted, false);
    const page = {
      state: "ok",
      records: [],
      more: false,
      scanned_bytes: 0,
      cursor: "next",
    };
    bridge.handle({
      type: "kernel_result",
      correlationId: siblingCall.correlationId as string,
      payload: { success: true, result: page },
    });
    assertEquals(await sibling, page);
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("typed secret and package APIs use private runtime operations", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1, workerMetadata);
  const calls = createCallQueue(channel.port2);
  try {
    const authenticated: ServiceRequestMetadata = {
      ...persistentMetadata,
      auth: {
        authenticated: true,
        realm: "user",
        userId: "user-1",
        username: "Admin",
      },
    };
    const inContext = <Result>(callback: () => Promise<Result>) =>
      bridge.withRequest(authenticated, callback);
    const respond = async (
      promise: Promise<unknown>,
      commandId: string,
      arguments_: Record<string, unknown>,
      result: Record<string, unknown>,
    ) => {
      const call = await calls.next();
      assertEquals(
        (call.payload as { arguments: unknown }).arguments,
        { operation: commandId, input: arguments_ },
      );
      bridge.handle({
        type: "kernel_result",
        correlationId: call.correlationId as string,
        payload: { success: true, result },
      });
      return await promise;
    };

    assertEquals(
      await respond(
        inContext(() => kernel.events.emit("minute", { test: true })),
        "event.emit",
        { name: "minute", data: { test: true } },
        { id: "event-1", listeners: 2 },
      ),
      { id: "event-1", listeners: 2 },
    );
    const programInput = {
      programId: "acme/tools/report",
      arguments: [{ day: 1 }],
      username: "robot",
      sandboxGroup: "batch",
      timeoutMs: 300000,
    };
    const programResult = {
      state: "failed",
      failure: "example",
      executionId: "job-0123456789",
      nodeId: "nod-0123456789",
      sandboxId: "sbx-0123456789",
      workerId: "wrk-0123456789",
      contextId: "ctx-0123456789",
      parentContextId: "ctx-abcdefghij",
      logPosition: "saved-position",
      queuedAt: "2026-09-06T01:02:03.000Z",
      startedAt: "2026-09-06T01:02:03.100Z",
      finishedAt: "2026-09-06T01:02:04.000Z",
      packageCommit: "abc",
      result: null,
    };
    assertEquals(
      await respond(
        inContext(() => kernel.programs.run(programInput)),
        "program.run",
        programInput,
        programResult,
      ),
      programResult,
    );

    assertEquals(
      await respond(
        inContext(() => kernel.secrets.list()),
        "secret.list",
        {},
        { secrets: [{ name: "github", updated_at: "2026-09-01T00:00:00Z" }] },
      ),
      [{ name: "github", updated_at: "2026-09-01T00:00:00Z" }],
    );
    assertEquals(
      await respond(
        inContext(() =>
          kernel.secrets.set({ name: "github", value: "replacement" })
        ),
        "secret.set",
        { name: "github", value: "replacement" },
        { secret: { name: "github", updated_at: "2026-09-01T00:01:00Z" } },
      ),
      { name: "github", updated_at: "2026-09-01T00:01:00Z" },
    );
    assertEquals(
      await respond(
        inContext(() => kernel.secrets.get("github")),
        "secret.get",
        { name: "github" },
        {
          secret: {
            name: "github",
            value: "replacement",
            updated_at: "2026-09-01T00:01:00Z",
          },
        },
      ),
      {
        name: "github",
        value: "replacement",
        updated_at: "2026-09-01T00:01:00Z",
      },
    );

    assertEquals(
      await respond(
        inContext(() =>
          kernel.packages.source.inspect("https://github.com/the8020/uui")
        ),
        "package.source.inspect",
        { source: "https://github.com/the8020/uui" },
        {
          source: {
            source: "https://github.com/the8020/uui.git",
            author: "the8020",
            repository: "uui",
            package_id: "the8020/uui",
            references: [],
          },
        },
      ),
      {
        source: "https://github.com/the8020/uui.git",
        author: "the8020",
        repository: "uui",
        package_id: "the8020/uui",
        references: [],
      },
    );

    const index = {
      author: "the8020",
      repository: "uui",
      source: "https://github.com/the8020/uui.git",
      local: false,
      package_id: "the8020/uui",
      valid: true,
    };
    assertEquals(
      await respond(
        inContext(() =>
          kernel.packages.index.set({
            author: "the8020",
            repository: "uui",
            source: "https://github.com/the8020/uui.git",
          })
        ),
        "package.index.set",
        {
          author: "the8020",
          repository: "uui",
          source: "https://github.com/the8020/uui.git",
        },
        { package: index },
      ),
      index,
    );
    assertEquals(
      await respond(
        inContext(() => kernel.packages.versions.list("the8020/uui", 25)),
        "package.version.list",
        { package_id: "the8020/uui", limit: 25 },
        {
          package: {
            package_id: "the8020/uui",
            source: index.source,
            versions: [],
          },
        },
      ),
      { package_id: "the8020/uui", source: index.source, versions: [] },
    );
    assertEquals(
      await respond(
        inContext(() => kernel.packages.synchronize(["the8020/uui"])),
        "package.synchronize",
        { packages: "the8020/uui" },
        { packages: [{ package_id: "the8020/uui", success: true }] },
      ),
      [{ package_id: "the8020/uui", success: true }],
    );
    assertEquals(
      await respond(
        inContext(() =>
          kernel.packages.local.create({
            author: "example",
            repository: "tools",
          })
        ),
        "package.local.create",
        { author: "example", repository: "tools" },
        { package: { commit: "abcdef1" } },
      ),
      { commit: "abcdef1" },
    );

    const repository = {
      package_id: "the8020/uui",
      path: "/packages/the8020/uui",
      activation_ready: true,
      branch: "main",
      head: "abcdef1234567",
      remote_name: "origin",
      remote_url: "https://github.com/the8020/uui.git",
      clean: true,
      status: "ready",
      branches: [{
        name: "main",
        commit: "abcdef1234567",
        current: true,
        remote: false,
      }],
      commits: [],
    };
    assertEquals(
      await respond(
        inContext(() => kernel.packages.repository.inspect("the8020/uui")),
        "package.repository.inspect",
        { package_id: "the8020/uui" },
        { repository },
      ),
      repository,
    );
    assertEquals(
      await respond(
        inContext(() => kernel.packages.repository.pull("the8020/uui")),
        "package.repository.pull",
        { package_id: "the8020/uui" },
        { repository },
      ),
      repository,
    );
    assertEquals(
      await respond(
        inContext(() => kernel.packages.repository.push("the8020/uui")),
        "package.repository.push",
        { package_id: "the8020/uui" },
        { repository },
      ),
      repository,
    );
    assertEquals(
      await respond(
        inContext(() =>
          kernel.packages.repository.checkout({
            packageId: "the8020/uui",
            branch: "main",
          })
        ),
        "package.repository.checkout",
        { package_id: "the8020/uui", branch: "main" },
        { repository },
      ),
      repository,
    );
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("Worker metadata and database info are available before execution", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1, workerMetadata);
  const calls = createCallQueue(channel.port2);
  try {
    assertEquals(kernelDatabaseBackend(), "postgresql");
    const info = kernel.database.info();
    const call = await calls.next();
    assertEquals(
      (call.payload as { operation: string; request?: unknown }).operation,
      "database.info",
    );
    assertEquals(
      (call.payload as { operation: string; request?: unknown }).request,
      undefined,
    );
    bridge.handle({
      type: "kernel_result",
      correlationId: call.correlationId as string,
      payload: {
        backend: "postgresql",
        location: "postgresql://database/system",
        state: "READY",
        initialized: true,
        catalog_version: 1,
      },
    });
    assertEquals((await info).backend, "postgresql");
    await assertRejects(
      () => kernel.crypto.token.verify("invalid"),
      Error,
      "inside an execution",
    );
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("execution context is immutable and isolated across requests", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1, workerMetadata);
  const calls = createCallQueue(channel.port2);
  try {
    const values = await Promise.all([
      bridge.withRequest(
        {
          ...metadata,
          contextId: "request-alice",
          user: { userId: "user:alice", username: "alice" },
        },
        async () => {
          await Promise.resolve();
          return context.current;
        },
      ),
      bridge.withRequest(
        {
          ...metadata,
          contextId: "request-bob",
          user: { userId: "user:bob", username: "bob" },
        },
        async () => {
          await Promise.resolve();
          return context.current;
        },
      ),
    ]);
    assertEquals(values.map((value) => [value.username, value.contextId]), [
      ["alice", "request-alice"],
      ["bob", "request-bob"],
    ]);
    assertEquals(Object.isFrozen(values[0]), true);
    assertEquals(Reflect.set(values[0], "username", "changed"), false);
    assertEquals(values[0].username, "alice");
    const mutableUser = { userId: "user:alice", username: "alice" };
    const pending = bridge.withRequest(
      { ...metadata, user: mutableUser },
      () => {
        mutableUser.username = "tampered";
        assertEquals(context.username, "alice");
        return kernel.admin.execute("kernel.status");
      },
    );
    const call = await calls.next();
    assertEquals(
      (call.payload as { request: { user: unknown } }).request.user,
      { userId: "user:alice", username: "alice" },
    );
    bridge.handle({
      type: "kernel_result",
      correlationId: call.correlationId as string,
      payload: { protocol_version: 2, success: true, result: {} },
    });
    await pending;
    await assertRejects(
      () => Promise.resolve().then(() => context.current),
      Error,
      "inside an invocation",
    );
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});
