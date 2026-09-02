import {
  defineService,
  HTTPError,
  type RequestMetadata,
  type RuntimeServiceContext,
  type WebSocketSession,
  z,
} from "./mod.ts";
import type {
  CurrentExecutionMetadata as BundledCurrentExecutionMetadata,
  RequestMetadata as BundledRequestMetadata,
} from "./the8020_http.d.ts";
import { assert, assertEquals, assertMatch } from "@std/assert";

type SameKeys<Left, Right> =
  [Exclude<keyof Left, keyof Right>, Exclude<keyof Right, keyof Left>] extends
    [never, never] ? true : false;
type BundledString = ReturnType<
  typeof import("./the8020_http.d.ts")["z"]["string"]
>;

const requestMetadataKeysMatch: SameKeys<
  RequestMetadata,
  BundledRequestMetadata
> = true;
const executionMetadataKeysMatch: SameKeys<
  RequestMetadata["execution"],
  BundledCurrentExecutionMetadata
> = true;
const bundledStringSupportsRegex: BundledString extends {
  regex(pattern: RegExp): BundledString;
} ? true
  : false = true;
void requestMetadataKeysMatch;
void executionMetadataKeysMatch;
void bundledStringSupportsRegex;

Deno.test("portable self-types match source request metadata", () => {
  const source = context().meta;
  const bundled: BundledRequestMetadata = source;
  const roundTrip: RequestMetadata = bundled;
  assertEquals(roundTrip.execution.workerId, "wrk-test0001");
  assertEquals(roundTrip.client.networkScope, "loopback");
});

function context(requestId = "request-1"): RuntimeServiceContext {
  return {
    signal: new AbortController().signal,
    meta: {
      requestId,
      serviceId: "core/example/service",
      serviceGeneration: 3,
      canonicalBasePath: "/core/example/service",
      originalUrl: "http://localhost/core/example/service/path",
      client: { ipAddress: "127.0.0.1", networkScope: "loopback" },
      execution: {
        nodeId: "node-test",
        runtimeGroupId: "rgp-test0001",
        sandboxId: "sbx-test0001",
        workerId: "wrk-test0001",
        workerExecutionId: "execution-test",
      },
      auth: { authenticated: false },
    },
  };
}

Deno.test("framework supports every required method and standard Response values", async () => {
  const service = defineService();
  for (
    const method of [
      "get",
      "post",
      "put",
      "patch",
      "delete",
      "options",
      "head",
      "all",
    ] as const
  ) {
    service[method](
      `/${method}`,
      {},
      () => new Response(method, { status: 202 }),
    );
  }
  for (
    const method of [
      "GET",
      "POST",
      "PUT",
      "PATCH",
      "DELETE",
      "OPTIONS",
      "HEAD",
    ] as const
  ) {
    const response = await service.fetch(
      new Request(`http://service/${method.toLowerCase()}`, { method }),
      context(),
    );
    assertEquals(response.status, 202);
    assertEquals(
      method === "HEAD" ? "" : await response.text(),
      method === "HEAD" ? "" : method.toLowerCase(),
    );
  }
  const all = await service.fetch(
    new Request("http://service/all", { method: "PURGE" }),
    context(),
  );
  assertEquals(await all.text(), "all");
});

Deno.test("framework validates params query headers and JSON bodies before handlers", async () => {
  const service = defineService();
  service.post(
    "/items/:itemId",
    {
      params: z.object({ itemId: z.string().min(2) }),
      query: z.object({ count: z.coerce.number().int().positive() }),
      headers: z.object({ "x-example": z.string() }),
      body: z.object({ enabled: z.boolean() }),
    },
    ({ params, query, headers, body, meta }) =>
      Response.json({
        params,
        query,
        headers,
        body,
        requestId: meta.requestId,
      }),
  );

  const valid = await service.fetch(
    new Request("http://service/items/ab?count=4", {
      method: "POST",
      headers: { "content-type": "application/json", "x-example": "yes" },
      body: JSON.stringify({ enabled: true }),
    }),
    context("request-valid"),
  );
  assertEquals(valid.status, 200);
  assertEquals(await valid.json(), {
    params: { itemId: "ab" },
    query: { count: 4 },
    headers: { "x-example": "yes" },
    body: { enabled: true },
    requestId: "request-valid",
  });

  const invalid = await service.fetch(
    new Request("http://service/items/x?count=no", {
      method: "POST",
      headers: { "x-example": "yes", "content-type": "application/json" },
      body: "{}",
    }),
    context("request-invalid"),
  );
  assertEquals(invalid.status, 400);
  const problem = await invalid.json();
  assertEquals(problem.request_id, "request-invalid");
  assertEquals(problem.error.code, "validation_error");
  assertEquals(problem.error.location, "params");
});

Deno.test("framework middleware is ordinary ordered TypeScript", async () => {
  const calls: string[] = [];
  const service = defineService();
  service.use(async ({ meta }, next) => {
    calls.push(`before:${meta.requestId}`);
    const response = await next();
    calls.push("after");
    response.headers.set("x-middleware", "yes");
    return response;
  });
  service.get("/", {}, () => {
    calls.push("handler");
    return new Response("ok");
  });
  const response = await service.fetch(
    new Request("http://service/"),
    context(),
  );
  assertEquals(response.headers.get("x-middleware"), "yes");
  assertEquals(calls, ["before:request-1", "handler", "after"]);
});

Deno.test("framework preserves Hono patterns, catch-all, and registration order", async () => {
  const service = defineService();
  service.get(
    "/ordered/:value",
    { params: z.object({ value: z.string() }) },
    ({ params }) => new Response(`first:${params.value}`),
  );
  service.get("/ordered/static", {}, () => new Response("second"));
  service.get(
    "/files/*",
    {},
    ({ request }) => new Response(new URL(request.url).pathname),
  );
  const ordered = await service.fetch(
    new Request("http://service/ordered/static"),
    context(),
  );
  assertEquals(await ordered.text(), "first:static");
  const wildcard = await service.fetch(
    new Request("http://service/files/a/b/c"),
    context(),
  );
  assertEquals(await wildcard.text(), "/files/a/b/c");
});

Deno.test("framework returns deliberate HTTP errors and hides uncaught failures", async () => {
  const service = defineService();
  service.get("/expected", {}, () => {
    throw new HTTPError(409, { error: "conflict" });
  });
  service.get("/unexpected", {}, () => {
    throw new Error("private stack detail");
  });
  const expected = await service.fetch(
    new Request("http://service/expected"),
    context(),
  );
  assertEquals(expected.status, 409);
  assertEquals(await expected.json(), { error: "conflict" });
  const unexpected = await service.fetch(
    new Request("http://service/unexpected"),
    context(),
  );
  assertEquals(unexpected.status, 500);
  const text = await unexpected.text();
  assertMatch(text, /internal_error/);
  assert(!text.includes("private stack detail"));
});

Deno.test("framework leaves undeclared bodies streaming and propagates cancellation", async () => {
  let cancelled = false;
  const service = defineService();
  service.post("/stream", {}, async ({ request, signal }) => {
    const body = await request.text();
    const output = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode(body.slice(0, 3)));
        controller.enqueue(new TextEncoder().encode(body.slice(3)));
        controller.close();
      },
      cancel() {
        cancelled = signal.aborted;
      },
    });
    return new Response(output);
  });
  const controller = new AbortController();
  const response = await service.fetch(
    new Request("http://service/stream", {
      method: "POST",
      body: "streamed",
      signal: controller.signal,
    }),
    context(),
  );
  controller.abort();
  await response.body?.cancel();
  assert(cancelled);
});

Deno.test("OpenAPI output is deterministic, relative, and schema-derived", () => {
  const service = defineService();
  service.get("/orders/:orderId", {
    summary: "Fetch order",
    params: z.object({ orderId: z.string() }),
    query: z.object({ include: z.coerce.boolean().optional() }),
    responses: { 200: z.object({ id: z.string() }) },
  }, () => Response.json({ id: "1" }));
  service.post(
    "/orders",
    { body: z.object({ id: z.string() }) },
    () => new Response(null, { status: 201 }),
  );
  service.get("/undocumented", {}, () => new Response("ok"));
  const metadata = {
    title: "Orders",
    version: "1.0.0",
    canonicalBasePath: "/core/orders/api",
  };
  const first = JSON.stringify(service.openapi(metadata));
  const second = JSON.stringify(service.openapi(metadata));
  assertEquals(first, second);
  const document = JSON.parse(first);
  assertEquals(document.servers, [{ url: "/core/orders/api" }]);
  assert(document.paths["/orders/{orderId}"].get);
  assert(
    !Object.keys(document.paths).some((path) =>
      path.startsWith("/core/orders/api")
    ),
  );
  assertEquals(
    document.paths["/orders/{orderId}"].get.parameters[0].in,
    "path",
  );
  assert(
    document.paths["/orders"].post.requestBody.content["application/json"]
      .schema,
  );
  assertEquals(
    document.paths["/undocumented"].get.responses.default.description,
    "Service response",
  );
});

Deno.test("WebSocket routes preserve middleware, metadata, and bidirectional messages", async () => {
  const service = defineService();
  const controller = new AbortController();
  const sent: Array<string | Uint8Array> = [];
  let closeCode = 0;
  let closeReason = "";
  let resolveClosed!: () => void;
  const closed = new Promise<void>((resolve) => resolveClosed = resolve);
  const socket: WebSocketSession = {
    protocol: "the8020.echo",
    signal: controller.signal,
    send(data) {
      sent.push(data);
    },
    receive() {
      return Promise.resolve({ type: "message", data: "hello" });
    },
    close(code = 1000, reason = "") {
      closeCode = code;
      closeReason = reason;
      controller.abort();
      resolveClosed();
    },
  };
  let middlewareRequest = "";
  let handlerContext: Record<string, unknown> = {};
  service.use(({ meta }, next) => {
    middlewareRequest = meta.requestId;
    return next();
  });
  service.websocket("/events/:topic", async (context) => {
    const event = await context.socket.receive();
    handlerContext = {
      params: context.params,
      query: context.query,
      protocol: context.socket.protocol,
      requestId: context.meta.requestId,
      auth: context.meta.auth,
      event,
    };
    context.socket.send("echo:hello");
    context.socket.send(new Uint8Array([1, 2, 3]));
  });

  const response = await service.connectWebSocket(
    new Request("http://service/events/status?watch=yes"),
    context("request-websocket"),
    socket,
  );
  assertEquals(response.status, 204);
  assertEquals(response.headers.get("x-80-20-websocket-accepted"), "true");
  await closed;
  assertEquals(middlewareRequest, "request-websocket");
  assertEquals(handlerContext, {
    params: { topic: "status" },
    query: { watch: "yes" },
    protocol: "the8020.echo",
    requestId: "request-websocket",
    auth: { authenticated: false },
    event: { type: "message", data: "hello" },
  });
  assertEquals(sent[0], "echo:hello");
  assertEquals(sent[1], new Uint8Array([1, 2, 3]));
  assertEquals(closeCode, 1000);
  assertEquals(closeReason, "handler completed");

  const missing = await service.connectWebSocket(
    new Request("http://service/missing"),
    context("request-missing"),
    socket,
  );
  assertEquals(missing.status, 404);
  const document = service.openapi({
    title: "WebSocket service",
    version: "1",
    canonicalBasePath: "/core/websocket/service",
  }) as { paths: Record<string, Record<string, unknown>> };
  assert(document.paths["/events/{topic}"]?.["x-80-20-websocket"]);
});
