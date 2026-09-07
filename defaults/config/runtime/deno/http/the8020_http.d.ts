import { z as Zod } from "zod";

export { Zod as z };
export type Schema<Output = unknown> = Zod.ZodType<Output>;
export type SchemaOutput<Value> = Value extends Schema<infer Output> ? Output
  : never;

export interface RequestMetadata {
  contextId: string;
  serviceId: string;
  serviceGeneration: number;
  canonicalBasePath: string;
  originalUrl: string;
  client: ClientConnectionMetadata;
  persistentExecutionId?: string;
  persistentKeepAliveMilliseconds?: number;
  execution: CurrentExecutionMetadata;
  user: ExecutionUserMetadata;
  auth: AuthContext;
}

export interface ExecutionUserMetadata {
  userId: string;
  username: string;
}

export interface ClientConnectionMetadata {
  ipAddress: string;
  networkScope: "loopback" | "private" | "link_local" | "public" | "special";
}

export interface CurrentExecutionMetadata {
  nodeId: string;

  sandboxId: string;
  workerId: string;

  persistentExecutionId?: string;
}

export interface AuthContext {
  authenticated: boolean;
  realm?: "user";
  userId?: string;
  username?: string;
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

export interface RouteSchemas {
  summary?: string;
  description?: string;
  params?: Schema;
  query?: Schema;
  headers?: Schema;
  body?: Schema;
  responses?: Record<number, Schema>;
}

type DefinitionOutput<
  Definition extends RouteSchemas,
  Key extends "params" | "query" | "headers" | "body",
> = Definition[Key] extends Schema<infer Output> ? Output
  : Record<string, never>;

export interface HandlerContext<Definition extends RouteSchemas> {
  request: Request;
  params: DefinitionOutput<Definition, "params">;
  query: DefinitionOutput<Definition, "query">;
  headers: DefinitionOutput<Definition, "headers">;
  body: DefinitionOutput<Definition, "body">;
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

export function defineService(): ServiceBuilder;

export class HTTPError extends Error {
  readonly status: number;
  readonly body: unknown;
  readonly headers: Headers;
  constructor(status: number, body?: unknown, headers?: HeadersInit);
}
