import { registerHooks } from "node:module";
import { fileURLToPath } from "node:url";
import type {
  BaseContext,
  ExecutionMetadata,
  InvocationMetadata,
  JobEntrypoint,
  RuntimeLogEvent,
  ServiceContext,
  ServiceEntrypoint,
  ServiceRequestMetadata,
  WorkerControlFunctions,
  WorkerExecutionFailure,
} from "./contracts.ts";
import { canonicalExecutionUser, canonicalInvocation } from "./contracts.ts";
import { authenticateRequest } from "./request_authentication.ts";
import { createKernelBridge } from "../kernel/bridge.ts";
import { installConsoleCapture, runtimeLog } from "../logging/capture.ts";
import { INITIAL_POLICY, type LogPolicy } from "../logging/protocol.ts";
import { WorkerLogSender } from "../logging/worker_channel.ts";
import { formatValues } from "../logging/format.ts";

interface InitializeMessage {
  type: "initialize";
  metadata: ExecutionMetadata;
  invocation?: InvocationMetadata;
  logPolicy?: LogPolicy;
  port: MessagePort;
}

interface ControlMessage {
  type:
    | "job_run"
    | "worker_invoke"
    | "service_request"
    | "service_openapi"
    | "service_websocket_open"
    | "service_websocket_message"
    | "service_websocket_close"
    | "cancel"
    | "stop";
  correlationId: string;
  payload?: unknown;
  headers?: [string, string][];
  body?: ReadableStream<Uint8Array> | null;
}

interface PlatformService {
  readonly __the8020Service: true;
  fetch(
    request: Request,
    context: {
      signal: AbortSignal;
      meta: ServiceRequestMetadata;
      log(event: RuntimeLogEvent): void;
    },
  ): Promise<Response>;
  openapi(metadata: {
    title?: string;
    version?: string;
    description?: string;
    canonicalBasePath: string;
  }): Record<string, unknown>;
  connectWebSocket(
    request: Request,
    context: {
      signal: AbortSignal;
      meta: ServiceRequestMetadata;
      log(event: RuntimeLogEvent): void;
    },
    socket: WorkerWebSocketSession,
  ): Promise<Response>;
}

class AsyncQueue<T> {
  #items: T[] = [];
  #waiters: Array<(value: T) => void> = [];

  push(value: T): void {
    const waiter = this.#waiters.shift();
    if (waiter === undefined) this.#items.push(value);
    else waiter(value);
  }

  async shift(): Promise<T> {
    const value = this.#items.shift();
    if (value !== undefined) return value;
    return await new Promise<T>((resolve) => this.#waiters.push(resolve));
  }
}

type WebSocketData = string | Uint8Array;
type WebSocketInboundEvent =
  | { type: "message"; data: WebSocketData }
  | { type: "close"; code: number; reason: string };

class WorkerWebSocketSession {
  readonly protocol: string;
  readonly #port: MessagePort;
  readonly #connectionId: string;
  readonly #controller = new AbortController();
  readonly #input = new AsyncQueue<WebSocketInboundEvent>();
  readonly #ended: () => void;
  #closed = false;

  constructor(
    port: MessagePort,
    connectionId: string,
    protocol: string,
    ended: () => void,
  ) {
    this.#port = port;
    this.#connectionId = connectionId;
    this.protocol = protocol;
    this.#ended = ended;
  }

  get signal(): AbortSignal {
    return this.#controller.signal;
  }

  send(data: WebSocketData): void {
    if (this.#closed) throw new Error("WebSocket is closed");
    if (typeof data !== "string" && !(data instanceof Uint8Array)) {
      throw new TypeError("WebSocket data must be text or bytes");
    }
    this.#port.postMessage({
      type: "service_websocket_send",
      payload: { connectionId: this.#connectionId, data },
    });
  }

  receive(): Promise<WebSocketInboundEvent> {
    return this.#input.shift();
  }

  close(code = 1000, reason = ""): void {
    if (this.#closed) return;
    validateWebSocketClose(code, reason);
    this.#closed = true;
    this.#controller.abort(new DOMException("WebSocket closed", "AbortError"));
    this.#input.push({ type: "close", code, reason });
    this.#port.postMessage({
      type: "service_websocket_close",
      payload: { connectionId: this.#connectionId, code, reason },
    });
    this.#ended();
  }

  remoteMessage(data: WebSocketData): void {
    if (!this.#closed) this.#input.push({ type: "message", data });
  }

  remoteClose(code: number, reason: string): void {
    if (this.#closed) return;
    this.#closed = true;
    this.#controller.abort(new DOMException("WebSocket closed", "AbortError"));
    this.#input.push({ type: "close", code, reason });
    this.#ended();
  }
}

let initialized = false;

self.onmessage = async (event: MessageEvent<InitializeMessage>) => {
  if (initialized || event.data.type !== "initialize") return;
  initialized = true;
  const { metadata, port } = event.data;
  registerHooks({
    load(url, context, nextLoad) {
      const loaded = nextLoad(url, context);
      if (url.startsWith("file:")) {
        port.postMessage({
          type: "module_loaded",
          payload: fileURLToPath(url),
        });
      }
      return loaded;
    },
  });
  const kernelBridge = createKernelBridge(port, metadata);
  const logSender = new WorkerLogSender(
    port,
    metadata,
    kernelBridge.executionContext,
    event.data.logPolicy ?? INITIAL_POLICY,
  );
  const controller = new AbortController();
  const activeRequests = new Map<string, AbortController>();
  const activeControls = new Map<string, AbortController>();
  const activeWebSockets = new Map<string, WorkerWebSocketSession>();
  const pendingControls: MessageEvent<ControlMessage>[] = [];
  let dispatchControl:
    | ((event: MessageEvent<ControlMessage>) => Promise<void>)
    | undefined;

  port.onmessage = (controlEvent: MessageEvent<ControlMessage>) => {
    if (logSender.handle(controlEvent.data)) return;
    if (kernelBridge.handle(controlEvent.data)) return;
    if (dispatchControl === undefined) {
      pendingControls.push(controlEvent);
      return;
    }
    void dispatchControl(controlEvent);
  };
  port.start();

  const log = (logEvent: RuntimeLogEvent): void =>
    runtimeLog(logSender, logEvent);
  installConsoleCapture(logSender);
  const base: BaseContext = { metadata, signal: controller.signal, log };
  const kernelServiceId = metadata.service?.serviceId ?? metadata.workloadId;
  const closeDatabaseScope = async (): Promise<void> => {
    if (metadata.databaseAccess === "none") return;
    try {
      await kernelBridge.closeExecution();
    } catch {
      // Abrupt Worker cleanup repeats this at Worker scope.
    }
  };
  const closeRequestDatabaseScope = (
    request: ServiceRequestMetadata,
  ): Promise<void> => kernelBridge.withRequest(request, closeDatabaseScope);
  const responseBody = (
    body: ReadableStream<Uint8Array>,
    request: ServiceRequestMetadata,
    signal: AbortSignal,
  ): ReadableStream<Uint8Array> => {
    const reader = body.getReader();
    let closed = false;
    const close = async () => {
      if (closed) return;
      closed = true;
      await closeRequestDatabaseScope(request);
    };
    return new ReadableStream({
      async pull(controller) {
        try {
          const next = await kernelBridge.withRequest(
            request,
            () => reader.read(),
            signal,
          );
          if (next.done) {
            controller.close();
            await close();
          } else {
            controller.enqueue(next.value);
          }
        } catch (error) {
          controller.error(error);
          await close();
        }
      },
      async cancel(reason) {
        try {
          await kernelBridge.withRequest(
            request,
            () => reader.cancel(reason),
            signal,
          );
        } finally {
          await close();
        }
      },
    });
  };

  try {
    const loadModule = async (): Promise<Record<string, unknown>> => {
      try {
        return await import(metadata.entrypoint);
      } catch (error) {
        logSender.print("ERROR", ["Worker import failed", error]);
        throw error;
      }
    };
    const module = event.data.invocation === undefined
      ? await loadModule()
      : await kernelBridge.withExecution({
        ...canonicalInvocation(event.data.invocation),
        serviceId: kernelServiceId,
        user: metadata.user,
        signal: controller.signal,
      }, loadModule);
    const workerFunctions = registeredWorkerFunctions(module.workerFunctions);
    const platformService = isPlatformService(module.default)
      ? module.default
      : undefined;
    const serviceEntrypoint = platformService === undefined
      ? module.fetch as ServiceEntrypoint | undefined
      : undefined;
    const jobEntrypoint = module.default as JobEntrypoint | undefined;

    if (
      metadata.workloadType === "service" &&
      typeof serviceEntrypoint !== "function" && platformService === undefined
    ) {
      throw new TypeError(
        "service entrypoint must default-export defineService() or export fetch(request, context)",
      );
    }
    if (
      metadata.workloadType === "service" && platformService !== undefined
    ) {
      if (
        metadata.service === undefined ||
        metadata.service.serviceId.length === 0 ||
        !Number.isSafeInteger(metadata.service.generation) ||
        !metadata.service.canonicalBasePath.startsWith("/")
      ) {
        throw new TypeError(
          "framework service identity, generation, and canonical base path are required",
        );
      }
      // Building the document proves route initialization and schema/OpenAPI
      // validity before this Worker reports readiness.
      platformService.openapi({
        ...metadata.service.openapi,
        canonicalBasePath: metadata.service.canonicalBasePath,
      });
    }
    if (
      metadata.workloadType === "job" && typeof jobEntrypoint !== "function"
    ) {
      throw new TypeError(
        "job entrypoint must default-export a function",
      );
    }

    dispatchControl = async (controlEvent: MessageEvent<ControlMessage>) => {
      const message = controlEvent.data;
      try {
        switch (message.type) {
          case "job_run": {
            const input = message.payload as {
              arguments?: unknown;
              secrets?: unknown;
              invocation?: unknown;
            };
            if (!Array.isArray(input.arguments)) {
              throw new TypeError("job arguments must be an array");
            }
            const arguments_ = input.arguments;
            if (
              input.secrets === null || typeof input.secrets !== "object" ||
              Array.isArray(input.secrets) ||
              !Object.values(input.secrets).every((value) =>
                typeof value === "string"
              )
            ) {
              throw new TypeError("job secrets must be a string map");
            }
            const secrets = { ...(input.secrets as Record<string, string>) };
            const result = await kernelBridge.withExecution(
              {
                ...canonicalInvocation(input.invocation),
                serviceId: metadata.workloadId,
                user: metadata.user,
                secrets,
                signal: controller.signal,
              },
              async () => {
                try {
                  return await jobEntrypoint!(...arguments_);
                } catch (error) {
                  logSender.print("ERROR", ["Job failed", error]);
                  throw error;
                } finally {
                  for (const name of Object.keys(secrets)) delete secrets[name];
                  await closeDatabaseScope();
                }
              },
            );
            port.postMessage({
              type: "job_result",
              correlationId: message.correlationId,
              payload: result,
            });
            break;
          }
          case "worker_invoke": {
            const input = message.payload as {
              function?: unknown;
              input?: unknown;
              invocation?: unknown;
              persistentExecutionId?: unknown;
              user?: unknown;
            };
            const name = typeof input.function === "string"
              ? input.function
              : "";
            const handler = workerFunctions[name];
            const persistentExecutionId = input.persistentExecutionId;
            if (
              persistentExecutionId !== undefined &&
              (typeof persistentExecutionId !== "string" ||
                persistentExecutionId.length === 0)
            ) {
              throw new TypeError("persistent execution ID must be non-empty");
            }
            if (handler === undefined) {
              port.postMessage({
                type: "worker_result",
                correlationId: message.correlationId,
                payload: {
                  ok: false,
                  error: {
                    code: "function_not_found",
                    message: `Worker function ${
                      name || "<empty>"
                    } is not registered`,
                  },
                },
              });
              break;
            }
            const control = new AbortController();
            activeControls.set(message.correlationId, control);
            try {
              const output = await kernelBridge.withExecution(
                {
                  ...canonicalInvocation(input.invocation),
                  serviceId: kernelServiceId,
                  persistentExecutionId,
                  user: canonicalExecutionUser(input.user),
                  signal: control.signal,
                },
                async () => {
                  try {
                    return await handler(input.input, {
                      ...base,
                      signal: control.signal,
                    });
                  } catch (error) {
                    logSender.print("ERROR", [
                      "Worker invocation failed",
                      error,
                    ]);
                    throw error;
                  } finally {
                    await closeDatabaseScope();
                  }
                },
              );
              assertJSONValue(output);
              port.postMessage({
                type: "worker_result",
                correlationId: message.correlationId,
                payload: { ok: true, output },
              });
            } catch (error) {
              port.postMessage({
                type: "worker_result",
                correlationId: message.correlationId,
                payload: {
                  ok: false,
                  error: {
                    code: control.signal.aborted
                      ? "timeout"
                      : "application_error",
                    message: errorMessage(error),
                  },
                },
              });
            } finally {
              activeControls.delete(message.correlationId);
            }
            break;
          }
          case "service_request": {
            const requestMetadata = message.payload as {
              method: string;
              url: string;
              meta: ServiceRequestMetadata;
            };
            const requestController = new AbortController();
            activeRequests.set(message.correlationId, requestController);
            const request = new Request(requestMetadata.url, {
              method: requestMetadata.method,
              headers: message.headers,
              body: message.body,
              signal: requestController.signal,
            });
            let context: ServiceContext = {
              ...base,
              signal: requestController.signal,
              contextId: requestMetadata.meta.contextId,
              meta: requestMetadata.meta,
            };
            let response: Response;
            try {
              const setup = await authenticateRequest(
                request,
                context.meta,
                kernelBridge,
                requestController.signal,
              );
              context = { ...context, meta: setup.meta };
              response = setup.response ?? await kernelBridge.withRequest(
                context.meta,
                () =>
                  platformService === undefined
                    ? serviceEntrypoint!(request, context)
                    : platformService.fetch(request, {
                      signal: context.signal,
                      meta: context.meta,
                      log,
                    }),
                requestController.signal,
              );
            } catch (error) {
              kernelBridge.withRequest(
                context.meta,
                () =>
                  logSender.print("ERROR", ["Service execution failed", error]),
              );
              await closeRequestDatabaseScope(context.meta);
              throw error;
            } finally {
              activeRequests.delete(message.correlationId);
              if (request.body !== null && !request.body.locked) {
                await request.body.cancel(
                  "service did not consume request body",
                );
              }
            }
            const body = response.body === null ? null : responseBody(
              response.body,
              context.meta,
              requestController.signal,
            );
            if (body === null) await closeRequestDatabaseScope(context.meta);
            port.postMessage(
              {
                type: "service_response",
                correlationId: message.correlationId,
                status: response.status,
                headers: [...response.headers.entries()],
                body,
              },
              body === null ? [] : [body],
            );
            break;
          }
          case "service_openapi": {
            if (
              platformService === undefined || metadata.service === undefined
            ) {
              throw new TypeError(
                "OpenAPI is available only for a defineService() entrypoint",
              );
            }
            port.postMessage({
              type: "service_openapi",
              correlationId: message.correlationId,
              payload: platformService.openapi({
                ...metadata.service.openapi,
                canonicalBasePath: metadata.service.canonicalBasePath,
              }),
            });
            break;
          }
          case "service_websocket_open": {
            if (platformService === undefined) {
              throw new TypeError(
                "WebSocket routes require a defineService() entrypoint",
              );
            }
            const input = message.payload as {
              connectionId?: unknown;
              method?: unknown;
              url?: unknown;
              protocol?: unknown;
              meta: ServiceRequestMetadata;
            };
            if (
              typeof input.connectionId !== "string" ||
              input.connectionId.length === 0 || input.method !== "GET" ||
              typeof input.url !== "string" ||
              typeof input.protocol !== "string" || input.meta === undefined
            ) throw new TypeError("invalid service WebSocket request");
            if (activeWebSockets.has(input.connectionId)) {
              throw new TypeError("duplicate service WebSocket connection");
            }
            const socket = new WorkerWebSocketSession(
              port,
              input.connectionId,
              input.protocol,
              () => activeWebSockets.delete(input.connectionId as string),
            );
            activeWebSockets.set(input.connectionId, socket);
            const request = new Request(input.url, {
              method: "GET",
              headers: message.headers,
              signal: socket.signal,
            });
            let response: Response;
            try {
              const setup = await authenticateRequest(
                request,
                input.meta,
                kernelBridge,
                socket.signal,
              );
              input.meta = setup.meta;
              response = setup.response ?? await kernelBridge.withRequest(
                input.meta,
                () =>
                  platformService.connectWebSocket(request, {
                    signal: socket.signal,
                    meta: input.meta!,
                    log,
                  }, socket),
                socket.signal,
              );
            } catch (error) {
              kernelBridge.withRequest(
                input.meta,
                () =>
                  logSender.print("ERROR", [
                    "WebSocket execution failed",
                    error,
                  ]),
              );
              await closeRequestDatabaseScope(input.meta);
              throw error;
            }
            if (
              response.status !== 204 ||
              response.headers.get("the8020-internal-websocket-accepted") !==
                "true"
            ) {
              await closeRequestDatabaseScope(input.meta);
              socket.remoteClose(1008, "WebSocket route rejected");
              port.postMessage({
                type: "service_websocket_rejected",
                correlationId: message.correlationId,
                status: response.status,
                headers: [...response.headers.entries()].filter(([name]) =>
                  name.toLowerCase() !== "the8020-internal-websocket-accepted"
                ),
              });
              break;
            }
            socket.signal.addEventListener("abort", () => {
              void closeRequestDatabaseScope(input.meta!);
            }, { once: true });
            port.postMessage({
              type: "service_websocket_ready",
              correlationId: message.correlationId,
            });
            break;
          }
          case "service_websocket_message": {
            const input = message.payload as {
              connectionId?: unknown;
              data?: unknown;
            };
            const socket = typeof input.connectionId === "string"
              ? activeWebSockets.get(input.connectionId)
              : undefined;
            if (
              socket === undefined ||
              typeof input.data !== "string" &&
                !(input.data instanceof Uint8Array)
            ) throw new TypeError("invalid service WebSocket message");
            socket.remoteMessage(input.data);
            break;
          }
          case "service_websocket_close": {
            const input = message.payload as {
              connectionId?: unknown;
              code?: unknown;
              reason?: unknown;
            };
            const socket = typeof input.connectionId === "string"
              ? activeWebSockets.get(input.connectionId)
              : undefined;
            if (
              socket === undefined || !Number.isInteger(input.code) ||
              typeof input.reason !== "string"
            ) throw new TypeError("invalid service WebSocket close");
            socket.remoteClose(Number(input.code), input.reason);
            break;
          }
          case "cancel":
            activeRequests.get(message.correlationId)?.abort(
              new DOMException("Request cancelled", "AbortError"),
            );
            activeControls.get(message.correlationId)?.abort(
              new DOMException("Control invocation cancelled", "AbortError"),
            );
            break;
          case "stop":
            controller.abort();
            for (const socket of activeWebSockets.values()) {
              socket.remoteClose(1001, "Worker stopping");
            }
            activeWebSockets.clear();
            kernelBridge.close();
            for (const requestController of activeRequests.values()) {
              requestController.abort(
                new DOMException("Worker stopping", "AbortError"),
              );
            }
            for (const control of activeControls.values()) {
              control.abort(new DOMException("Worker stopping", "AbortError"));
            }
            port.postMessage({
              type: "stopped",
              correlationId: message.correlationId,
            });
            port.close();
            self.close();
            break;
        }
      } catch (error: unknown) {
        port.postMessage({
          type: "execution_error",
          correlationId: message.correlationId,
          failure: executionFailure(error),
        });
      }
    };
    for (const controlEvent of pendingControls.splice(0)) {
      void dispatchControl(controlEvent);
    }
    port.postMessage({ type: "ready" });
  } catch (error: unknown) {
    port.postMessage({ type: "fatal", error: errorMessage(error) });
  }
};

function isPlatformService(value: unknown): value is PlatformService {
  return value !== null && typeof value === "object" &&
    (value as Partial<PlatformService>).__the8020Service === true &&
    typeof (value as Partial<PlatformService>).fetch === "function" &&
    typeof (value as Partial<PlatformService>).openapi === "function" &&
    typeof (value as Partial<PlatformService>).connectWebSocket === "function";
}

function registeredWorkerFunctions(value: unknown): WorkerControlFunctions {
  if (value === undefined) return Object.freeze({});
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new TypeError("workerFunctions must be a function map");
  }
  const result: Record<string, WorkerControlFunctions[string]> = {};
  for (const [name, handler] of Object.entries(value)) {
    if (
      !/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(name) ||
      typeof handler !== "function"
    ) throw new TypeError("workerFunctions contains an invalid registration");
    result[name] = handler as WorkerControlFunctions[string];
  }
  return Object.freeze(result);
}

function assertJSONValue(value: unknown): void {
  let encoded: string | undefined;
  try {
    encoded = JSON.stringify(value);
  } catch {
    throw new TypeError("Worker function output must be JSON serializable");
  }
  if (encoded === undefined) {
    throw new TypeError("Worker function output must be a JSON value");
  }
  if (new TextEncoder().encode(encoded).byteLength > 1_048_576) {
    throw new TypeError("Worker function output exceeds 1 MiB");
  }
}

function validateWebSocketClose(code: number, reason: string): void {
  if (!Number.isInteger(code) || code < 1000 || code > 4999) {
    throw new TypeError("WebSocket close code must be between 1000 and 4999");
  }
  if (new TextEncoder().encode(reason).byteLength > 123) {
    throw new TypeError("WebSocket close reason exceeds 123 bytes");
  }
}

function errorMessage(error: unknown): string {
  return formatValues([error], 4096);
}

function executionFailure(error: unknown): WorkerExecutionFailure {
  if (
    error instanceof Error && error.name === "AdminCommandError" &&
    typeof (error as Error & { code?: unknown }).code === "string"
  ) {
    const details = (error as Error & { details?: unknown }).details;
    return {
      message: error.message,
      code: (error as Error & { code: string }).code,
      ...(details !== null && typeof details === "object" &&
          !Array.isArray(details)
        ? { details: details as Record<string, unknown> }
        : {}),
    };
  }
  return { message: errorMessage(error) };
}
