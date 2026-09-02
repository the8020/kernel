declare namespace Zod {
  export type ZodRawShape = Record<string, ZodType>;
  export type infer<Value extends ZodType> = Value["_output"];
  export type SchemaOutput<Value> = Value extends ZodType<infer Output> ? Output
    : never;
  export type ObjectOutput<Shape extends ZodRawShape> =
    & {
      -readonly [
        Key in keyof Shape as undefined extends SchemaOutput<Shape[Key]> ? never
          : Key
      ]: SchemaOutput<Shape[Key]>;
    }
    & {
      -readonly [
        Key in keyof Shape as undefined extends SchemaOutput<Shape[Key]> ? Key
          : never
      ]?: Exclude<SchemaOutput<Shape[Key]>, undefined>;
    };

  export interface ParseSuccess<Output> {
    success: true;
    data: Output;
  }
  export interface ParseFailure {
    success: false;
    error: Error;
  }

  export abstract class ZodType<Output = unknown> {
    readonly _output: Output;
    parse(input: unknown): Output;
    safeParse(input: unknown): ParseSuccess<Output> | ParseFailure;
    optional(): ZodOptional<this>;
    nullable(): ZodNullable<this>;
    array(): ZodArray<this>;
  }

  export class ZodString extends ZodType<string> {
    min(length: number): ZodString;
    max(length: number): ZodString;
    regex(pattern: RegExp): ZodString;
    email(): ZodString;
    url(): ZodString;
  }
  export class ZodNumber extends ZodType<number> {
    int(): ZodNumber;
    positive(): ZodNumber;
    nonnegative(): ZodNumber;
    min(value: number): ZodNumber;
    max(value: number): ZodNumber;
  }
  export class ZodBigInt extends ZodType<bigint> {}
  export class ZodBoolean extends ZodType<boolean> {}
  export class ZodObject<Shape extends ZodRawShape> extends ZodType<
    ObjectOutput<Shape>
  > {
    readonly shape: Shape;
  }
  export class ZodArray<Element extends ZodType> extends ZodType<
    Array<SchemaOutput<Element>>
  > {
    readonly element: Element;
  }
  export class ZodEnum<
    Values extends readonly [string, ...string[]] = readonly [
      string,
      ...string[],
    ],
  > extends ZodType<
    Values[number]
  > {
    readonly options: readonly string[];
  }
  export class ZodOptional<Inner extends ZodType> extends ZodType<
    SchemaOutput<Inner> | undefined
  > {
    unwrap(): Inner;
  }
  export class ZodNullable<Inner extends ZodType> extends ZodType<
    SchemaOutput<Inner> | null
  > {
    unwrap(): Inner;
  }
  export class ZodDefault<Inner extends ZodType> extends ZodType<
    SchemaOutput<Inner>
  > {
    unwrap(): Inner;
  }
  export class ZodCatch<Inner extends ZodType>
    extends ZodType<SchemaOutput<Inner>> {
    unwrap(): Inner;
  }
  export class ZodReadonly<Inner extends ZodType> extends ZodType<
    Readonly<SchemaOutput<Inner>>
  > {
    unwrap(): Inner;
  }

  export function string(): ZodString;
  export function number(): ZodNumber;
  export function bigint(): ZodBigInt;
  export function boolean(): ZodBoolean;
  export function literal<const Value extends string | number | boolean>(
    value: Value,
  ): ZodType<Value>;
  export function enum_<const Values extends readonly [string, ...string[]]>(
    values: Values,
  ): ZodEnum<Values>;
  export { enum_ as enum };
  export function object<const Shape extends ZodRawShape>(
    shape: Shape,
  ): ZodObject<Shape>;
  export function array<Element extends ZodType>(
    schema: Element,
  ): ZodArray<Element>;
  export function union<const Values extends readonly ZodType[]>(
    values: Values,
  ): ZodType<SchemaOutput<Values[number]>>;
  export namespace coerce {
    export function string(): ZodString;
    export function number(): ZodNumber;
    export function boolean(): ZodBoolean;
  }
}

export { Zod as z };
export type Schema<Output = unknown> = Zod.ZodType<Output>;
export type SchemaOutput<Value> = Value extends Schema<infer Output> ? Output
  : never;

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
