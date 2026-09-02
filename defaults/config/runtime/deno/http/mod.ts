import { type Context as HonoContext, Hono } from "hono";
import { z } from "zod";

export { z };

export interface RequestMetadata {
  requestId: string;
  serviceId: string;
  serviceGeneration: number;
  canonicalBasePath: string;
  originalUrl: string;
  client: ClientConnectionMetadata;
  persistentExecutionId?: string;
  persistentKeepAliveMilliseconds?: number;
  execution: CurrentExecutionMetadata;
  auth: AuthContext;
  authenticatedUser?: string;
}

export interface ClientConnectionMetadata {
  ipAddress: string;
  networkScope: "loopback" | "private" | "link_local" | "public" | "special";
}

export interface CurrentExecutionMetadata {
  nodeId: string;
  runtimeGroupId: string;
  sandboxId: string;
  workerId: string;
  workerExecutionId: string;
  persistentExecutionId?: string;
}

export interface AuthContext {
  authenticated: boolean;
  realm?: "bootstrap-admin";
  userId?: string;
  username?: string;
  authVersion?: number;
}

export interface RuntimeServiceContext {
  readonly signal: AbortSignal;
  readonly meta: RequestMetadata;
  log?(event: {
    level: "debug" | "info" | "warn" | "error";
    message: string;
    fields?: Record<string, unknown>;
  }): void;
}

export interface OpenAPIServiceMetadata {
  title?: string;
  version?: string;
  description?: string;
  canonicalBasePath: string;
}

export type Schema<Output = unknown> = z.ZodType<Output>;
type SchemaOutput<Value> = Value extends z.ZodType<infer Output> ? Output
  : Record<string, never>;

export interface RouteSchemas {
  summary?: string;
  description?: string;
  params?: z.ZodType;
  query?: z.ZodType;
  headers?: z.ZodType;
  body?: z.ZodType;
  responses?: Record<number, z.ZodType>;
}

export interface HandlerContext<Definition extends RouteSchemas> {
  request: Request;
  params: SchemaOutput<Definition["params"]>;
  query: SchemaOutput<Definition["query"]>;
  headers: SchemaOutput<Definition["headers"]>;
  body: SchemaOutput<Definition["body"]>;
  signal: AbortSignal;
  meta: RequestMetadata;
}

export type Handler<Definition extends RouteSchemas = RouteSchemas> = (
  context: HandlerContext<Definition>,
) => Response | Promise<Response>;

export interface MiddlewareContext {
  request: Request;
  signal: AbortSignal;
  meta: RequestMetadata;
}

export type Middleware = (
  context: MiddlewareContext,
  next: () => Promise<Response>,
) => Response | Promise<Response>;

export type WebSocketData = string | Uint8Array;

export type WebSocketInboundEvent =
  | { type: "message"; data: WebSocketData }
  | { type: "close"; code: number; reason: string };

export interface WebSocketSession {
  readonly protocol: string;
  readonly signal: AbortSignal;
  send(data: WebSocketData): void;
  receive(): Promise<WebSocketInboundEvent>;
  close(code?: number, reason?: string): void;
}

export interface WebSocketHandlerContext {
  request: Request;
  params: Record<string, string>;
  query: Record<string, string | string[]>;
  headers: Record<string, string>;
  signal: AbortSignal;
  meta: RequestMetadata;
  socket: WebSocketSession;
}

export type WebSocketHandler = (
  context: WebSocketHandlerContext,
) => void | Promise<void>;

export interface PlatformService {
  readonly __the8020Service: true;
  fetch(request: Request, context: RuntimeServiceContext): Promise<Response>;
  openapi(metadata: OpenAPIServiceMetadata): Record<string, unknown>;
  connectWebSocket(
    request: Request,
    context: RuntimeServiceContext,
    socket: WebSocketSession,
  ): Promise<Response>;
}

type Method =
  | "GET"
  | "POST"
  | "PUT"
  | "PATCH"
  | "DELETE"
  | "OPTIONS"
  | "HEAD"
  | "ALL"
  | "WEBSOCKET";

interface RouteDefinition {
  method: Method;
  path: string;
  schemas: RouteSchemas;
}

interface PlatformEnvironment {
  Bindings: { runtimeContext: RuntimeServiceContext };
}

export interface ServiceBuilder extends PlatformService {
  get<Definition extends RouteSchemas>(
    path: string,
    definition: Definition,
    handler: Handler<Definition>,
  ): ServiceBuilder;
  post<Definition extends RouteSchemas>(
    path: string,
    definition: Definition,
    handler: Handler<Definition>,
  ): ServiceBuilder;
  put<Definition extends RouteSchemas>(
    path: string,
    definition: Definition,
    handler: Handler<Definition>,
  ): ServiceBuilder;
  patch<Definition extends RouteSchemas>(
    path: string,
    definition: Definition,
    handler: Handler<Definition>,
  ): ServiceBuilder;
  delete<Definition extends RouteSchemas>(
    path: string,
    definition: Definition,
    handler: Handler<Definition>,
  ): ServiceBuilder;
  options<Definition extends RouteSchemas>(
    path: string,
    definition: Definition,
    handler: Handler<Definition>,
  ): ServiceBuilder;
  head<Definition extends RouteSchemas>(
    path: string,
    definition: Definition,
    handler: Handler<Definition>,
  ): ServiceBuilder;
  all<Definition extends RouteSchemas>(
    path: string,
    definition: Definition,
    handler: Handler<Definition>,
  ): ServiceBuilder;
  websocket(path: string, handler: WebSocketHandler): ServiceBuilder;
  use(middleware: Middleware): ServiceBuilder;
}

export class HTTPError extends Error {
  readonly status: number;
  readonly body: unknown;
  readonly headers: Headers;

  constructor(
    status: number,
    body: unknown = { error: "request_failed" },
    headers?: HeadersInit,
  ) {
    super(`HTTP ${status}`);
    if (!Number.isInteger(status) || status < 400 || status > 599) {
      throw new TypeError("HTTPError status must be between 400 and 599");
    }
    this.name = "HTTPError";
    this.status = status;
    this.body = body;
    this.headers = new Headers(headers);
  }
}

class Service implements ServiceBuilder {
  readonly __the8020Service = true as const;
  readonly #app = new Hono<PlatformEnvironment>();
  readonly #websocketApp = new Hono<PlatformEnvironment>();
  readonly #routes: RouteDefinition[] = [];
  readonly #middleware: Middleware[] = [];

  get<Definition extends RouteSchemas>(
    path: string,
    definition: Definition,
    handler: Handler<Definition>,
  ): ServiceBuilder {
    return this.#register("GET", path, definition, handler);
  }
  post<Definition extends RouteSchemas>(
    path: string,
    definition: Definition,
    handler: Handler<Definition>,
  ): ServiceBuilder {
    return this.#register("POST", path, definition, handler);
  }
  put<Definition extends RouteSchemas>(
    path: string,
    definition: Definition,
    handler: Handler<Definition>,
  ): ServiceBuilder {
    return this.#register("PUT", path, definition, handler);
  }
  patch<Definition extends RouteSchemas>(
    path: string,
    definition: Definition,
    handler: Handler<Definition>,
  ): ServiceBuilder {
    return this.#register("PATCH", path, definition, handler);
  }
  delete<Definition extends RouteSchemas>(
    path: string,
    definition: Definition,
    handler: Handler<Definition>,
  ): ServiceBuilder {
    return this.#register("DELETE", path, definition, handler);
  }
  options<Definition extends RouteSchemas>(
    path: string,
    definition: Definition,
    handler: Handler<Definition>,
  ): ServiceBuilder {
    return this.#register("OPTIONS", path, definition, handler);
  }
  head<Definition extends RouteSchemas>(
    path: string,
    definition: Definition,
    handler: Handler<Definition>,
  ): ServiceBuilder {
    return this.#register("HEAD", path, definition, handler);
  }
  all<Definition extends RouteSchemas>(
    path: string,
    definition: Definition,
    handler: Handler<Definition>,
  ): ServiceBuilder {
    return this.#register("ALL", path, definition, handler);
  }

  websocket(path: string, handler: WebSocketHandler): ServiceBuilder {
    validateWebSocketRoute(path, handler);
    this.#routes.push({ method: "WEBSOCKET", path, schemas: {} });
    this.#websocketApp.get(path, async (honoContext) => {
      const request = honoContext.req.raw;
      const runtimeContext = honoContext.env.runtimeContext;
      const socket = (runtimeContext as RuntimeServiceContext & {
        websocket?: WebSocketSession;
      }).websocket;
      if (socket === undefined) {
        return new Response(null, { status: 500 });
      }
      let index = -1;
      const launch = (): Promise<Response> => {
        void Promise.resolve().then(() =>
          handler({
            request,
            params: honoContext.req.param(),
            query: queryValues(request.url),
            headers: Object.fromEntries(request.headers),
            signal: runtimeContext.signal,
            meta: runtimeContext.meta,
            socket,
          })
        ).then(
          () => socket.close(1000, "handler completed"),
          (error: unknown) => {
            runtimeContext.log?.({
              level: "error",
              message: "WebSocket handler failed",
              fields: { error: errorMessage(error) },
            });
            socket.close(1011, "handler failed");
          },
        );
        return Promise.resolve(
          new Response(null, {
            status: 204,
            headers: { "x-80-20-websocket-accepted": "true" },
          }),
        );
      };
      const next = async (): Promise<Response> => {
        index++;
        const middleware = this.#middleware[index];
        if (middleware === undefined) return await launch();
        return await middleware({
          request,
          signal: runtimeContext.signal,
          meta: runtimeContext.meta,
        }, next);
      };
      try {
        return await next();
      } catch (error) {
        return handleError(error, runtimeContext);
      }
    });
    return this;
  }

  use(middleware: Middleware): ServiceBuilder {
    if (typeof middleware !== "function") {
      throw new TypeError("service middleware must be a function");
    }
    this.#middleware.push(middleware);
    return this;
  }

  async fetch(
    request: Request,
    context: RuntimeServiceContext,
  ): Promise<Response> {
    const runtimeContext = normalizedRuntimeContext(request, context);
    const headRequest = request.method === "HEAD";
    try {
      const response = await this.#app.fetch(request, { runtimeContext });
      if (headRequest) {
        return new Response(null, {
          status: response.status,
          statusText: response.statusText,
          headers: response.headers,
        });
      }
      return response;
    } catch (error) {
      return handleError(error, runtimeContext);
    }
  }

  async connectWebSocket(
    request: Request,
    context: RuntimeServiceContext,
    socket: WebSocketSession,
  ): Promise<Response> {
    const runtimeContext = {
      ...normalizedRuntimeContext(request, context),
      websocket: socket,
    };
    try {
      return await this.#websocketApp.fetch(request, { runtimeContext });
    } catch (error) {
      return handleError(error, runtimeContext);
    }
  }

  openapi(metadata: OpenAPIServiceMetadata): Record<string, unknown> {
    if (!metadata.canonicalBasePath.startsWith("/")) {
      throw new TypeError("OpenAPI canonical base path must begin with /");
    }
    const paths: Record<string, Record<string, unknown>> = {};
    for (const route of this.#routes) {
      const path = openAPIPath(route.path);
      const method = route.method === "ALL"
        ? "x-80-20-all"
        : route.method === "WEBSOCKET"
        ? "x-80-20-websocket"
        : route.method.toLowerCase();
      const operation: Record<string, unknown> = {};
      if (route.schemas.summary !== undefined) {
        operation.summary = route.schemas.summary;
      }
      if (route.schemas.description !== undefined) {
        operation.description = route.schemas.description;
      }
      const parameters = [
        ...schemaParameters(route.schemas.params, "path", true),
        ...schemaParameters(route.schemas.query, "query", false),
        ...schemaParameters(route.schemas.headers, "header", false),
      ];
      if (parameters.length > 0) operation.parameters = parameters;
      if (route.schemas.body !== undefined) {
        operation.requestBody = {
          required: true,
          content: {
            "application/json": { schema: schemaDocument(route.schemas.body) },
          },
        };
      }
      operation.responses = responseDocuments(route.schemas.responses);
      (paths[path] ??= {})[method] = operation;
    }
    return {
      openapi: "3.1.0",
      info: {
        title: metadata.title || "80|20 service",
        version: metadata.version || "0.0.0",
        ...(metadata.description ? { description: metadata.description } : {}),
      },
      servers: [{ url: metadata.canonicalBasePath }],
      paths,
    };
  }

  #register<Definition extends RouteSchemas>(
    method: Method,
    path: string,
    definition: Definition,
    handler: Handler<Definition>,
  ): ServiceBuilder {
    validateRoute(path, definition, handler);
    this.#routes.push({ method, path, schemas: definition });
    const dispatch = async (
      honoContext: HonoContext<PlatformEnvironment>,
    ): Promise<Response> => {
      const request = honoContext.req.raw;
      const runtimeContext = honoContext.env.runtimeContext;
      try {
        const validated = {
          request,
          params: await validateInput(
            definition.params,
            honoContext.req.param(),
            "params",
            runtimeContext.meta.requestId,
          ),
          query: await validateInput(
            definition.query,
            queryValues(request.url),
            "query",
            runtimeContext.meta.requestId,
          ),
          headers: await validateInput(
            definition.headers,
            Object.fromEntries(request.headers),
            "headers",
            runtimeContext.meta.requestId,
          ),
          body: await validatedBody(
            definition.body,
            request,
            runtimeContext.meta.requestId,
          ),
          signal: runtimeContext.signal,
          meta: runtimeContext.meta,
        } as HandlerContext<Definition>;
        let index = -1;
        const next = async (): Promise<Response> => {
          index++;
          const middleware = this.#middleware[index];
          if (middleware !== undefined) {
            return await middleware({
              request,
              signal: runtimeContext.signal,
              meta: runtimeContext.meta,
            }, next);
          }
          const response = await handler(validated);
          if (!(response instanceof Response)) {
            throw new TypeError("service handlers must return a Response");
          }
          return response;
        };
        return await next();
      } catch (error) {
        return handleError(error, runtimeContext);
      }
    };
    switch (method) {
      case "GET":
        this.#app.get(path, dispatch);
        break;
      case "POST":
        this.#app.post(path, dispatch);
        break;
      case "PUT":
        this.#app.put(path, dispatch);
        break;
      case "PATCH":
        this.#app.patch(path, dispatch);
        break;
      case "DELETE":
        this.#app.delete(path, dispatch);
        break;
      case "OPTIONS":
        this.#app.options(path, dispatch);
        break;
      case "HEAD":
        // Hono implements HEAD through its GET matcher and removes the body
        // from the resulting Response according to HTTP semantics.
        this.#app.get(path, dispatch);
        break;
      case "ALL":
        this.#app.all(path, dispatch);
        break;
      case "WEBSOCKET":
        throw new TypeError("WebSocket routes use service.websocket()");
    }
    return this;
  }
}

function validateWebSocketRoute(
  path: string,
  handler: WebSocketHandler,
): void {
  validateRoute(path, {}, () => new Response(null));
  if (typeof handler !== "function") {
    throw new TypeError("WebSocket handler must be a function");
  }
}

export function defineService(): ServiceBuilder {
  return new Service();
}

function validateRoute(
  path: string,
  definition: RouteSchemas,
  handler: Handler,
): void {
  if (!path.startsWith("/") || path.startsWith("//") || path.includes("://")) {
    throw new TypeError(
      "service routes must be relative absolute-path patterns",
    );
  }
  if (definition === null || typeof definition !== "object") {
    throw new TypeError("route definition must be an object");
  }
  if (typeof handler !== "function") {
    throw new TypeError("route handler must be a function");
  }
  for (
    const [location, schema] of Object.entries({
      params: definition.params,
      query: definition.query,
      headers: definition.headers,
      body: definition.body,
    })
  ) {
    if (schema !== undefined && typeof schema.safeParseAsync !== "function") {
      throw new TypeError(`${location} schema must be a Zod schema`);
    }
  }
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function normalizedRuntimeContext(
  request: Request,
  context: RuntimeServiceContext,
): RuntimeServiceContext {
  const meta = context?.meta;
  if (
    meta === undefined || typeof meta.requestId !== "string" ||
    typeof meta.serviceId !== "string" ||
    !Number.isSafeInteger(meta.serviceGeneration) ||
    typeof meta.canonicalBasePath !== "string" ||
    typeof meta.originalUrl !== "string"
  ) {
    throw new TypeError("trusted service request metadata is required");
  }
  return {
    ...context,
    signal: request.signal,
    meta: { ...meta },
  };
}

class InputValidationError extends Error {
  constructor(
    readonly location: string,
    readonly issues: unknown[],
    readonly requestId: string,
  ) {
    super(`invalid ${location}`);
  }
}

async function validateInput(
  schema: z.ZodType | undefined,
  value: unknown,
  location: string,
  requestId: string,
): Promise<unknown> {
  if (schema === undefined) return {};
  const result = await schema.safeParseAsync(value);
  if (!result.success) {
    throw new InputValidationError(location, result.error.issues, requestId);
  }
  return result.data;
}

async function validatedBody(
  schema: z.ZodType | undefined,
  request: Request,
  requestId: string,
): Promise<unknown> {
  if (schema === undefined) return {};
  let value: unknown;
  try {
    value = await request.json();
  } catch {
    throw new InputValidationError(
      "body",
      [{
        code: "invalid_json",
        message: "request body must be valid JSON",
        path: [],
      }],
      requestId,
    );
  }
  return await validateInput(schema, value, "body", requestId);
}

function queryValues(rawUrl: string): Record<string, string | string[]> {
  const output: Record<string, string | string[]> = {};
  for (const [key, value] of new URL(rawUrl).searchParams) {
    const previous = output[key];
    output[key] = previous === undefined
      ? value
      : Array.isArray(previous)
      ? [...previous, value]
      : [previous, value];
  }
  return output;
}

function handleError(
  error: unknown,
  context: RuntimeServiceContext,
): Response {
  if (error instanceof InputValidationError) {
    return Response.json({
      error: {
        code: "validation_error",
        location: error.location,
        issues: error.issues,
      },
      request_id: error.requestId,
    }, { status: 400 });
  }
  if (error instanceof HTTPError) {
    const headers = new Headers(error.headers);
    if (error.body === null) {
      return new Response(null, { status: error.status, headers });
    }
    if (typeof error.body === "string") {
      return new Response(error.body, { status: error.status, headers });
    }
    if (error.body instanceof Uint8Array) {
      return new Response(new Uint8Array(error.body).buffer, {
        status: error.status,
        headers,
      });
    }
    headers.set("content-type", "application/json; charset=UTF-8");
    return new Response(JSON.stringify(error.body), {
      status: error.status,
      headers,
    });
  }
  context.log?.({
    level: "error",
    message: "uncaught service handler error",
    fields: {
      request_id: context.meta.requestId,
      service_id: context.meta.serviceId,
      error: error instanceof Error ? error.message : String(error),
    },
  });
  return Response.json({
    error: { code: "internal_error", message: "Internal server error" },
    request_id: context.meta.requestId,
  }, { status: 500 });
}

function openAPIPath(path: string): string {
  return path.replace(/:([A-Za-z0-9_]+)\??/g, "{$1}");
}

function schemaDocument(schema: z.ZodType): Record<string, unknown> {
  return z.toJSONSchema(schema, { target: "openapi-3.0" }) as Record<
    string,
    unknown
  >;
}

function schemaParameters(
  schema: z.ZodType | undefined,
  location: "path" | "query" | "header",
  forceRequired: boolean,
): Record<string, unknown>[] {
  if (schema === undefined) return [];
  const document = schemaDocument(schema);
  const properties = document.properties;
  if (
    properties === undefined || properties === null ||
    typeof properties !== "object" ||
    Array.isArray(properties)
  ) return [];
  const required = new Set(
    Array.isArray(document.required)
      ? document.required.filter((item): item is string =>
        typeof item === "string"
      )
      : [],
  );
  return Object.keys(properties).sort().map((name) => ({
    name,
    in: location,
    required: forceRequired || required.has(name),
    schema: (properties as Record<string, unknown>)[name],
  }));
}

function responseDocuments(
  schemas: Record<number, z.ZodType> | undefined,
): Record<string, unknown> {
  if (schemas === undefined || Object.keys(schemas).length === 0) {
    return { default: { description: "Service response" } };
  }
  const result: Record<string, unknown> = {};
  for (
    const status of Object.keys(schemas).sort((left, right) =>
      Number(left) - Number(right)
    )
  ) {
    const schema = schemas[Number(status)];
    if (schema !== undefined) {
      result[status] = {
        description: `HTTP ${status} response`,
        content: { "application/json": { schema: schemaDocument(schema) } },
      };
    }
  }
  return result;
}
