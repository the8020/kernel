import { assertEquals, assertRejects } from "../test/assert.ts";
import type { ServiceRequestMetadata } from "../worker/contracts.ts";
import { createKernelBridge } from "./bridge.ts";
import { AdminCommandError, kernel } from "./mod.ts";

const metadata: ServiceRequestMetadata = {
  requestId: "request-1",
  serviceId: "example/auth/login",
  serviceGeneration: 1,
  canonicalBasePath: "/example/auth/login",
  originalUrl: "https://example.test/example/auth/login",
  execution: {
    nodeId: "node-1",
    runtimeGroupId: "rgp-test0001",
    sandboxId: "sbx-test0001",
    workerId: "wrk-test0001",
    workerExecutionId: "execution-1",
  },
  auth: { authenticated: false },
};
const persistentMetadata: ServiceRequestMetadata = {
  ...metadata,
  persistentExecutionId: "persistent-test",
  persistentKeepAliveMilliseconds: 60_000,
};

Deno.test("typed kernel auth bridge correlates login and logout", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1);
  const calls: Array<Record<string, unknown>> = [];
  channel.port2.onmessage = (event) => calls.push(event.data);
  channel.port2.start();
  try {
    const loginPromise = bridge.withRequest(
      metadata,
      () =>
        kernel.auth.bootstrapLogin({ username: "Admin", password: "private" }),
    );
    await new Promise((resolve) => setTimeout(resolve, 0));
    const loginCall = calls[0]!;
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
    await new Promise((resolve) => setTimeout(resolve, 0));
    const logoutCall = calls[1]!;
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

Deno.test("current user reads trusted persistent context locally", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1);
  const calls: unknown[] = [];
  channel.port2.onmessage = (event) => calls.push(event.data);
  channel.port2.start();
  const authenticated: ServiceRequestMetadata = {
    ...persistentMetadata,
    auth: {
      authenticated: true,
      realm: "bootstrap-admin",
      userId: "user-1",
      username: "Admin",
      authVersion: 3,
    },
  };
  try {
    assertEquals(
      await bridge.withRequest(authenticated, () => kernel.auth.currentUser()),
      { id: "user-1", username: "Admin", realm: "bootstrap-admin" },
    );
    assertEquals(
      await bridge.withRequest(metadata, () => kernel.auth.currentUser()),
      undefined,
    );
    bridge.beginPersistentRequest(authenticated);
    assertEquals(await kernel.auth.currentUser(), {
      id: "user-1",
      username: "Admin",
      realm: "bootstrap-admin",
    });
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
  const calls: unknown[] = [];
  channel.port2.onmessage = (event) => calls.push(event.data);
  channel.port2.start();
  try {
    bridge.beginPersistentRequest(persistentMetadata);
    const completion = kernel.execution.completePersistent();
    const invocation = kernel.worker.invoke({
      nodeId: "node-2",
      sandboxId: "sbx-target01",
      workerId: "wrk-target01",
      function: "package.inspect",
      input: { id: "one" },
    });
    await new Promise((resolve) => setTimeout(resolve, 0));
    assertEquals(
      (calls[0] as { payload: { operation: string } }).payload.operation,
      "execution.completePersistent",
    );
    assertEquals(
      (calls[1] as { payload: { operation: string } }).payload.operation,
      "worker.invoke",
    );
    for (const call of calls as Array<Record<string, unknown>>) {
      bridge.handle({
        type: "kernel_result",
        correlationId: call.correlationId as string,
        payload: call === calls[0]
          ? undefined
          : { ok: true, output: { id: "one" } },
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
  const calls: Array<Record<string, unknown>> = [];
  channel.port2.onmessage = (event) => calls.push(event.data);
  channel.port2.start();
  const authenticated: ServiceRequestMetadata = {
    ...persistentMetadata,
    auth: {
      authenticated: true,
      realm: "bootstrap-admin",
      userId: "user-1",
      username: "Admin",
    },
  };
  try {
    bridge.beginPersistentRequest(authenticated);
    const list = kernel.admin.execute<{ services: unknown[] }>("service.list");
    await new Promise((resolve) => setTimeout(resolve, 0));
    const listCall = calls[0]!;
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
        protocol_version: 1,
        success: true,
        request_id: "command-1",
        result: { services: [{ service_id: "core/example/service" }] },
      },
    });
    assertEquals(await list, {
      services: [{ service_id: "core/example/service" }],
    });

    const missing = kernel.admin.execute("service.inspect", {
      service_id: "missing/service/id",
    });
    await new Promise((resolve) => setTimeout(resolve, 0));
    const missingCall = calls[1]!;
    bridge.handle({
      type: "kernel_result",
      correlationId: missingCall.correlationId as string,
      payload: {
        protocol_version: 1,
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

Deno.test("typed package API delegates to generic administrative commands", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1);
  const calls: Array<Record<string, unknown>> = [];
  channel.port2.onmessage = (event) => calls.push(event.data);
  channel.port2.start();
  try {
    bridge.beginPersistentRequest({
      ...persistentMetadata,
      auth: {
        authenticated: true,
        realm: "bootstrap-admin",
        userId: "user-1",
        username: "Admin",
      },
    });
    const respond = async (
      promise: Promise<unknown>,
      commandId: string,
      arguments_: Record<string, unknown>,
      result: Record<string, unknown>,
    ) => {
      await new Promise((resolve) => setTimeout(resolve, 0));
      const call = calls.at(-1)!;
      assertEquals(
        (call.payload as { arguments: unknown }).arguments,
        { command_id: commandId, arguments: arguments_ },
      );
      bridge.handle({
        type: "kernel_result",
        correlationId: call.correlationId as string,
        payload: { protocol_version: 1, success: true, result },
      });
      return await promise;
    };

    assertEquals(
      await respond(
        kernel.packages.source.inspect("https://github.com/the8020/uui"),
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
      schema: 1,
      author: "the8020",
      repository: "uui",
      source: "https://github.com/the8020/uui.git",
      local: false,
      package_id: "the8020/uui",
      path: "/state/package-index/the8020/uui.toml",
      valid: true,
    };
    assertEquals(
      await respond(
        kernel.packages.index.set({
          author: "the8020",
          repository: "uui",
          source: "https://github.com/the8020/uui.git",
        }),
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
        kernel.packages.versions.list("the8020/uui", 25),
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
        kernel.packages.synchronize(["the8020/uui"]),
        "package.synchronize",
        { packages: "the8020/uui" },
        { packages: [{ package_id: "the8020/uui", success: true }] },
      ),
      [{ package_id: "the8020/uui", success: true }],
    );
    assertEquals(
      await respond(
        kernel.packages.local.create({
          author: "example",
          repository: "tools",
        }),
        "package.local.create",
        { author: "example", repository: "tools" },
        { package: { commit: "abcdef1" } },
      ),
      { commit: "abcdef1" },
    );
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});

Deno.test("kernel auth call outside request context fails safely", async () => {
  const channel = new MessageChannel();
  const bridge = createKernelBridge(channel.port1);
  try {
    await assertRejects(
      () => kernel.auth.logoutCurrent(),
      Error,
      "inside a service request",
    );
  } finally {
    bridge.close();
    channel.port1.close();
    channel.port2.close();
  }
});
