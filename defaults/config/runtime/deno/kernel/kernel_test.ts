import { assertEquals, assertRejects } from "../test/assert.ts";
import type { ServiceRequestMetadata } from "../worker/contracts.ts";
import { createKernelBridge } from "./bridge.ts";
import {
  AdminCommandError,
  kernel,
  kernelDatabaseBackend,
  parseCommandArguments,
  requiredCommandArgument,
} from "./mod.ts";

const metadata: ServiceRequestMetadata = {
  requestId: "request-1",
  serviceId: "example/auth/login",
  serviceGeneration: 1,
  canonicalBasePath: "/example/auth/login",
  originalUrl: "https://example.test/example/auth/login",
  client: { ipAddress: "203.0.113.4", networkScope: "public" },
  execution: {
    nodeId: "node-1",
    runtimeGroupId: "rgp-test0001",
    sandboxId: "sbx-test0001",
    workerId: "wrk-test0001",
    workerExecutionId: "execution-1",
  },
  auth: { authenticated: false },
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

Deno.test("typed kernel auth bridge correlates login and logout", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1);
  const calls = createCallQueue(channel.port2);
  try {
    const loginPromise = bridge.withRequest(
      metadata,
      () => kernel.auth.login({ username: "Admin", password: "private" }),
    );
    const loginCall = await calls.next();
    assertEquals(loginCall.type, "kernel_call");
    assertEquals(
      (loginCall.payload as { request: unknown }).request,
      {
        requestId: "request-1",
        serviceId: "example/auth/login",
        persistentExecutionId: undefined,
      },
    );
    bridge.handle({
      type: "kernel_result",
      correlationId: loginCall.correlationId as string,
      payload: { authenticated: true, setCookie: "opaque-header" },
    });
    assertEquals(await loginPromise, {
      authenticated: true,
      setCookie: "opaque-header",
    });

    const logoutPromise = bridge.withRequest(
      metadata,
      () => kernel.auth.logoutCurrent(),
    );
    const logoutCall = await calls.next();
    bridge.handle({
      type: "kernel_result",
      correlationId: logoutCall.correlationId as string,
      payload: { setCookie: "cleared" },
    });
    assertEquals(await logoutPromise, { setCookie: "cleared" });
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("current user uses only the exact asynchronous request context", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1);
  const calls: unknown[] = [];
  channel.port2.onmessage = (event) => calls.push(event.data);
  channel.port2.start();
  const authenticated: ServiceRequestMetadata = {
    ...persistentMetadata,
    auth: {
      authenticated: true,
      realm: "user",
      userId: "user-1",
      username: "Admin",
      authVersion: 3,
    },
  };
  try {
    assertEquals(
      await bridge.withRequest(authenticated, () => kernel.auth.currentUser()),
      { id: "user-1", username: "Admin", realm: "user" },
    );
    assertEquals(
      await bridge.withRequest(metadata, () => kernel.auth.currentUser()),
      undefined,
    );
    await assertRejects(
      () => kernel.auth.currentUser(),
      Error,
      "inside an execution",
    );
    await new Promise((resolve) => setTimeout(resolve, 0));
    assertEquals(calls, []);
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("persistent completion and exact Worker calls use the generic bridge", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1);
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
  const bridge = createKernelBridge(channel.port1);
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
    bridge.handle({
      type: "kernel_result",
      correlationId: missingCall.correlationId as string,
      payload: {
        protocol_version: 2,
        success: false,
        request_id: "command-2",
        error: { code: "not_found", message: "service not found" },
      },
    });
    try {
      await missing;
      throw new Error("expected admin command failure");
    } catch (error) {
      if (!(error instanceof AdminCommandError)) throw error;
      assertEquals(error.code, "not_found");
      assertEquals(error.requestId, "command-2");
    }
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("typed database bridge uses one execute operation", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1);
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
  const bridge = createKernelBridge(channel.port1);
  const calls = createCallQueue(channel.port2);
  let releaseFirst!: () => void;
  let releaseSecond!: () => void;
  const firstGate = new Promise<void>((resolve) => releaseFirst = resolve);
  const secondGate = new Promise<void>((resolve) => releaseSecond = resolve);
  const firstMetadata = { ...metadata, requestId: "request-first" };
  const secondMetadata = { ...metadata, requestId: "request-second" };
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
      (secondCall.payload as { request: { requestId: string } }).request
        .requestId,
      "request-second",
    );
    assertEquals(
      (firstCall.payload as { request: { requestId: string } }).request
        .requestId,
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
  const bridge = createKernelBridge(channel.port1);
  const calls = createCallQueue(channel.port2);
  let resume!: () => void;
  const suspended = new Promise<void>((resolve) => resume = resolve);
  const authenticated: ServiceRequestMetadata = {
    ...persistentMetadata,
    requestId: "request-establish",
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
      { ...authenticated, requestId: "request-websocket" },
      () => undefined,
    );
    const control = bridge.withExecution(
      {
        requestId: "request-control",
        serviceId: "service-version-a",
        persistentExecutionId: "persistent-test",
      },
      () => kernel.admin.execute("service.inspect"),
    );
    const controlCall = await calls.next();
    assertEquals(
      (controlCall.payload as { request: { requestId: string } }).request
        .requestId,
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
      (call.payload as { request: { requestId: string } }).request.requestId,
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
  const bridge = createKernelBridge(channel.port1);
  const calls = createCallQueue(channel.port2);
  const base: ServiceRequestMetadata = {
    ...persistentMetadata,
    persistentExecutionId: "persistent-shared",
  };
  try {
    const first = bridge.withRequest(
      { ...base, requestId: "request-first" },
      () => kernel.database.execute("SELECT 1", [], { returnRows: true }),
    );
    const second = bridge.withRequest(
      { ...base, requestId: "request-second" },
      () => kernel.database.execute("SELECT 2", [], { returnRows: true }),
    );
    const pending = [await calls.next(), await calls.next()];
    assertEquals(
      new Set(
        pending.map((call) =>
          (call.payload as { request: { requestId: string } }).request.requestId
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
  const bridge = createKernelBridge(channel.port1);
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

Deno.test("typed secret and package APIs use private runtime operations", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1);
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
  const bridge = createKernelBridge(channel.port1, "postgresql");
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
      () => kernel.auth.logoutCurrent(),
      Error,
      "inside an execution",
    );
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});
