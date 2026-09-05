import { trackStream } from "./streams.ts";
import type {
  ExecutionMetadata,
  ExecutionUserMetadata,
  KernelCall,
  KernelOperation,
  RuntimeLogEvent,
  ServiceRequestMetadata,
  WorkerExecutionFailure,
  WorkerPermissionSet,
} from "./contracts.ts";

interface RuntimeWorkerOptions {
  metadata: ExecutionMetadata;
  permissions: WorkerPermissionSet;
  kernelCall?: KernelCall;
  now?: () => number;
  onCapacityChange?: () => void;
  onClose?: () => void;
}

interface WorkerMessage {
  type: string;
  correlationId?: string;
  payload?: unknown;
  error?: string;
  failure?: WorkerExecutionFailure;
  status?: number;
  headers?: [string, string][];
  body?: ReadableStream<Uint8Array> | null;
}

export class WorkerExecutionError extends Error {
  readonly code?: string;
  readonly details?: Record<string, unknown>;

  constructor(failure: WorkerExecutionFailure) {
    super(failure.message);
    this.name = "WorkerExecutionError";
    this.code = failure.code;
    this.details = failure.details;
  }
}

interface Pending {
  resolve(value: unknown): void;
  reject(reason: Error): void;
  cleanup(): void;
  retainInFlight: boolean;
}

interface KernelCallPayload {
  operation: KernelOperation;
  arguments: Record<string, unknown>;
  request?: {
    requestId: string;
    serviceId: string;
    persistentExecutionId?: string;
    user?: ExecutionUserMetadata;
  };
}

export interface WorkerInvocationResult {
  ok: boolean;
  output?: unknown;
  error?: {
    code: string;
    message: string;
  };
}

type WebSocketData = string | Uint8Array;

export interface ServiceWebSocketConnection {
  send(data: WebSocketData): void;
  close(code?: number, reason?: string): void;
}

export type ServiceWebSocketOpenResult =
  | { accepted: true; connection: ServiceWebSocketConnection }
  | { accepted: false; status: number; headers: [string, string][] };

interface ServiceWebSocketCallbacks {
  send(data: WebSocketData): void;
  close(code: number, reason: string): void;
}

export class RuntimeWorker {
  readonly metadata: ExecutionMetadata;
  readonly ready: Promise<void>;
  #worker: Worker;
  #port: MessagePort;
  #pending = new Map<string, Pending>();
  #sequence = 0;
  #closed = false;
  #draining = false;
  #failure?: string;
  #starting = true;
  #inFlight = 0;
  #idleWaiters = new Set<() => void>();
  #idleSinceMilliseconds: number | undefined;
  #logs: RuntimeLogEvent[] = [];
  #kernelCall?: KernelCall;
  #kernelCalls = new Map<string, AbortController>();
  #webSockets = new Map<string, ServiceWebSocketCallbacks>();
  #now: () => number;
  #onCapacityChange?: () => void;
  #onClose?: () => void;

  constructor(options: RuntimeWorkerOptions) {
    this.metadata = options.metadata;
    this.#kernelCall = options.kernelCall;
    this.#now = options.now ?? Date.now;
    this.#onCapacityChange = options.onCapacityChange;
    this.#onClose = options.onClose;
    const channel = new MessageChannel();
    this.#port = channel.port1;
    const workerOptions: WorkerOptions & {
      deno: { permissions: WorkerPermissionSet };
    } = {
      type: "module",
      name: options.metadata.debuggerName,
      deno: { permissions: options.permissions },
    };
    this.#worker = new Worker(
      new URL("./bootstrap.ts", import.meta.url).href,
      workerOptions,
    );
    let readyResolve!: () => void;
    let readyReject!: (reason: Error) => void;
    this.ready = new Promise<void>((resolve, reject) => {
      readyResolve = resolve;
      readyReject = reject;
    });
    this.#port.onmessage = (event: MessageEvent<WorkerMessage>) => {
      const message = event.data;
      if (message.type === "ready") {
        this.#starting = false;
        this.#idleSinceMilliseconds = this.#now();
        this.#onCapacityChange?.();
        readyResolve();
        return;
      }
      if (message.type === "fatal") {
        const reason = message.error ?? "Worker failed";
        readyReject(new Error(reason));
        this.#fail(reason);
        return;
      }
      if (message.type === "log") {
        const log = message.payload as RuntimeLogEvent;
        if (
          log !== null && typeof log === "object" &&
          typeof log.level === "string" && typeof log.message === "string"
        ) {
          this.#logs.push(log);
          if (this.#logs.length > 1_000) this.#logs.shift();
        }
        return;
      }
      if (message.type === "kernel_call") {
        void this.#handleKernelCall(message);
        return;
      }
      if (
        message.type === "kernel_cancel" && message.correlationId !== undefined
      ) {
        this.#kernelCalls.get(message.correlationId)?.abort(
          new DOMException("Kernel call cancelled", "AbortError"),
        );
        return;
      }
      if (message.type === "service_websocket_send") {
        const payload = message.payload as {
          connectionId?: unknown;
          data?: unknown;
        };
        const callbacks = typeof payload.connectionId === "string"
          ? this.#webSockets.get(payload.connectionId)
          : undefined;
        if (
          callbacks !== undefined &&
          (typeof payload.data === "string" ||
            payload.data instanceof Uint8Array)
        ) callbacks.send(payload.data);
        return;
      }
      if (message.type === "service_websocket_close") {
        const payload = message.payload as {
          connectionId?: unknown;
          code?: unknown;
          reason?: unknown;
        };
        const connectionId = typeof payload.connectionId === "string"
          ? payload.connectionId
          : "";
        const callbacks = this.#webSockets.get(connectionId);
        if (callbacks !== undefined) {
          callbacks.close(
            Number.isInteger(payload.code) ? Number(payload.code) : 1011,
            typeof payload.reason === "string" ? payload.reason : "",
          );
          this.#webSockets.delete(connectionId);
          this.#completeInFlight();
        }
        return;
      }
      if (message.correlationId === undefined) return;
      const pending = this.#pending.get(message.correlationId);
      if (pending === undefined) return;
      this.#pending.delete(message.correlationId);
      if (
        message.error !== undefined || message.failure !== undefined ||
        !pending.retainInFlight
      ) {
        this.#completeInFlight();
      }
      pending.cleanup();
      if (message.failure !== undefined) {
        pending.reject(new WorkerExecutionError(message.failure));
      } else if (message.error !== undefined) {
        pending.reject(new Error(message.error));
      } else pending.resolve(message);
    };
    this.#port.start();
    this.#worker.onerror = (event) => {
      event.preventDefault();
      const error = new Error(event.message);
      readyReject(error);
      this.#fail(error.message);
    };
    this.#worker.postMessage({
      type: "initialize",
      metadata: options.metadata,
      port: channel.port2,
    }, [channel.port2]);
  }

  async #handleKernelCall(message: WorkerMessage): Promise<void> {
    const correlationId = message.correlationId;
    try {
      if (correlationId === undefined || this.#kernelCall === undefined) {
        throw new Error("kernel API is unavailable");
      }
      const controller = new AbortController();
      this.#kernelCalls.set(correlationId, controller);
      const payload = message.payload as Partial<KernelCallPayload> | undefined;
      if (
        payload === undefined ||
        (payload.operation !== "admin.execute" &&
          payload.operation !== "runtime.operation" &&
          payload.operation !== "database.info" &&
          payload.operation !== "database.execute" &&
          payload.operation !== "database.scope.close" &&
          payload.operation !== "database.transaction.begin" &&
          payload.operation !== "database.transaction.commit" &&
          payload.operation !== "database.transaction.rollback" &&
          payload.operation !== "worker.invoke" &&
          payload.operation !== "execution.completePersistent") ||
        payload.arguments === null || typeof payload.arguments !== "object"
      ) {
        throw new Error("invalid kernel API call");
      }
      const databaseOperation = payload.operation.startsWith("database.");
      const access = this.metadata.databaseAccess ?? "full";
      if (
        databaseOperation &&
        payload.operation !== "database.scope.close" &&
        access === "none"
      ) {
        throw new Error("database SQL is not available to this Worker");
      }
      const result = await this.#kernelCall({
        operation: payload.operation,
        arguments: payload.arguments as Record<string, unknown>,
        requestId: payload.request?.requestId,
        serviceId: payload.request?.serviceId ?? this.metadata.workloadId,
        executionId: this.metadata.executionId,
        workerId: this.metadata.workerId,
        persistentExecutionId: payload.request?.persistentExecutionId,
        user: payload.request?.user,
      }, controller.signal);
      this.#port.postMessage({
        type: "kernel_result",
        correlationId,
        payload: result,
      });
    } catch (error) {
      if (correlationId !== undefined) {
        this.#port.postMessage({
          type: "kernel_result",
          correlationId,
          error: error instanceof Error
            ? error.message
            : "kernel API call failed",
        });
      }
    } finally {
      if (correlationId !== undefined) this.#kernelCalls.delete(correlationId);
    }
  }

  get inFlight(): number {
    return this.#inFlight;
  }

  get idleSinceMilliseconds(): number | undefined {
    return this.#inFlight === 0 && !this.#closed
      ? this.#idleSinceMilliseconds
      : undefined;
  }

  get closed(): boolean {
    return this.#closed;
  }

  get draining(): boolean {
    return this.#draining;
  }

  get failure(): string | undefined {
    return this.#failure;
  }

  get starting(): boolean {
    return this.#starting;
  }

  get logs(): RuntimeLogEvent[] {
    return this.#logs.map((event) => ({
      ...event,
      fields: event.fields === undefined ? undefined : { ...event.fields },
    }));
  }

  async runJob(
    arguments_: unknown[],
    secrets: Record<string, string> = {},
  ): Promise<unknown> {
    this.#logs = [];
    const message = await this.#request({
      type: "job_run",
      payload: { arguments: arguments_, secrets },
    });
    return (message as WorkerMessage).payload;
  }

  async invoke(
    functionName: string,
    input: unknown,
    signal: AbortSignal | undefined,
    persistentExecutionId: string | undefined,
    user: ExecutionUserMetadata,
  ): Promise<WorkerInvocationResult> {
    const response = await this.#request(
      {
        type: "worker_invoke",
        payload: { function: functionName, input, persistentExecutionId, user },
      },
      [],
      signal,
    ) as WorkerMessage;
    const result = response.payload as WorkerInvocationResult;
    if (
      result === null || typeof result !== "object" ||
      typeof result.ok !== "boolean"
    ) throw new TypeError("Worker returned an invalid invocation result");
    return result;
  }

  async dispatchService(
    request: Request,
    metadata?: ServiceRequestMetadata,
  ): Promise<Response> {
    if (this.#draining) throw new Error("Worker is draining");
    metadata ??= {
      requestId: crypto.randomUUID(),
      serviceId: this.metadata.service?.serviceId ?? this.metadata.workloadId,
      serviceGeneration: this.metadata.service?.generation ?? 0,
      canonicalBasePath: this.metadata.service?.canonicalBasePath ?? "/",
      originalUrl: request.url,
      client: { ipAddress: "", networkScope: "special" },
      execution: {
        nodeId: this.metadata.nodeId,
        runtimeGroupId: this.metadata.runtimeGroupId,
        sandboxId: this.metadata.sandboxId,
        workerId: this.metadata.workerId,
        workerExecutionId: this.metadata.executionId,
      },
      auth: { authenticated: false },
      user: this.metadata.user,
    };
    const headers = [...request.headers.entries()];
    const body = request.body;
    const response = await this.#request(
      {
        type: "service_request",
        payload: { method: request.method, url: request.url, meta: metadata },
        headers,
        body,
      },
      body === null ? [] : [body],
      request.signal,
      true,
    ) as WorkerMessage;
    const responseBody = response.body === undefined || response.body === null
      ? null
      : trackStream(response.body, () => this.#completeInFlight());
    if (responseBody === null) this.#completeInFlight();
    return new Response(responseBody, {
      status: response.status ?? 500,
      headers: response.headers,
    });
  }

  async serviceOpenAPI(): Promise<Record<string, unknown>> {
    const message = await this.#request({ type: "service_openapi" });
    const payload = (message as WorkerMessage).payload;
    if (
      payload === null || typeof payload !== "object" || Array.isArray(payload)
    ) {
      throw new TypeError("service OpenAPI response must be an object");
    }
    return payload as Record<string, unknown>;
  }

  async openServiceWebSocket(
    request: Request,
    metadata: ServiceRequestMetadata,
    protocol: string,
    callbacks: ServiceWebSocketCallbacks,
  ): Promise<ServiceWebSocketOpenResult> {
    if (this.#draining) throw new Error("Worker is draining");
    const connectionId = `websocket-${crypto.randomUUID()}`;
    this.#webSockets.set(connectionId, callbacks);
    let response: WorkerMessage;
    try {
      response = await this.#request(
        {
          type: "service_websocket_open",
          payload: {
            connectionId,
            method: "GET",
            url: request.url,
            protocol,
            meta: metadata,
          },
          headers: [...request.headers.entries()],
        },
        [],
        request.signal,
        true,
      ) as WorkerMessage;
    } catch (error) {
      this.#webSockets.delete(connectionId);
      throw error;
    }
    if (response.type === "service_websocket_rejected") {
      this.#webSockets.delete(connectionId);
      this.#completeInFlight();
      return {
        accepted: false,
        status: response.status ?? 404,
        headers: response.headers ?? [],
      };
    }
    if (response.type !== "service_websocket_ready") {
      this.#webSockets.delete(connectionId);
      this.#completeInFlight();
      throw new Error("Worker returned an invalid WebSocket response");
    }
    let closed = false;
    return {
      accepted: true,
      connection: {
        send: (data) => {
          if (closed || this.#closed) return;
          this.#port.postMessage({
            type: "service_websocket_message",
            payload: { connectionId, data },
          });
        },
        close: (code = 1000, reason = "") => {
          if (closed) return;
          closed = true;
          this.#webSockets.delete(connectionId);
          if (!this.#closed) {
            this.#port.postMessage({
              type: "service_websocket_close",
              payload: { connectionId, code, reason },
            });
          }
          this.#completeInFlight();
        },
      },
    };
  }

  async stop(graceMilliseconds = 1_000): Promise<void> {
    if (this.#closed) return;
    this.#draining = true;
    const deadline = Date.now() + graceMilliseconds;
    await this.#waitForIdle(Math.max(0, deadline - Date.now()));
    if (this.#inFlight > 0) {
      this.kill();
      return;
    }
    let forced = false;
    const timeout = setTimeout(() => {
      forced = true;
      this.kill();
    }, Math.max(1, deadline - Date.now()));
    try {
      await this.#request({ type: "stop" });
      this.kill();
    } catch (error) {
      if (!forced) throw error;
    } finally {
      clearTimeout(timeout);
    }
  }

  kill(): void {
    if (this.#closed) return;
    this.#closed = true;
    this.#worker.terminate();
    for (const controller of this.#kernelCalls.values()) controller.abort();
    this.#kernelCalls.clear();
    this.#port.close();
    this.#closedNotification();
    this.#closeWebSockets("Worker terminated");
    this.#rejectAll("Worker terminated");
  }

  async #request(
    message: WorkerMessage,
    transfer: Transferable[] = [],
    signal?: AbortSignal,
    retainInFlight = false,
  ): Promise<unknown> {
    if (this.#closed) throw new Error(this.#failure ?? "Worker is closed");
    if (this.#starting) await this.ready;
    if (signal?.aborted) {
      throw signal.reason ?? new DOMException("Aborted", "AbortError");
    }
    const correlationId = `${this.metadata.workerId}-${++this.#sequence}`;
    this.#idleSinceMilliseconds = undefined;
    this.#inFlight++;
    this.#onCapacityChange?.();
    let abort: (() => void) | undefined;
    const result = new Promise<unknown>((resolve, reject) => {
      const cleanup = (): void => {
        if (abort !== undefined) signal?.removeEventListener("abort", abort);
      };
      this.#pending.set(correlationId, {
        resolve,
        reject,
        cleanup,
        retainInFlight,
      });
      abort = (): void => {
        const pending = this.#pending.get(correlationId);
        if (pending === undefined) return;
        this.#pending.delete(correlationId);
        this.#completeInFlight();
        pending.cleanup();
        this.#port.postMessage({ type: "cancel", correlationId });
        reject(signal?.reason ?? new DOMException("Aborted", "AbortError"));
      };
      signal?.addEventListener("abort", abort, { once: true });
    });
    this.#port.postMessage({ ...message, correlationId }, transfer);
    return await result;
  }

  #rejectAll(reason: string): void {
    for (const pending of this.#pending.values()) {
      pending.cleanup();
      pending.reject(new Error(reason));
    }
    this.#pending.clear();
    this.#inFlight = 0;
    this.#idleSinceMilliseconds = undefined;
    this.#notifyIdle();
    this.#onCapacityChange?.();
  }

  #completeInFlight(): void {
    this.#inFlight = Math.max(0, this.#inFlight - 1);
    if (this.#inFlight === 0) {
      this.#idleSinceMilliseconds = this.#now();
      this.#notifyIdle();
    }
    this.#onCapacityChange?.();
  }

  #waitForIdle(maximumMilliseconds: number): Promise<void> {
    if (this.#inFlight === 0) return Promise.resolve();
    return new Promise((resolve) => {
      const finish = (): void => {
        this.#idleWaiters.delete(finish);
        clearTimeout(timer);
        resolve();
      };
      const timer = setTimeout(finish, maximumMilliseconds);
      this.#idleWaiters.add(finish);
    });
  }

  #notifyIdle(): void {
    const waiters = [...this.#idleWaiters];
    this.#idleWaiters.clear();
    for (const finish of waiters) finish();
  }

  #fail(reason: string): void {
    if (this.#closed) return;
    this.#failure = reason;
    this.#closed = true;
    this.#worker.terminate();
    this.#port.close();
    this.#closedNotification();
    this.#closeWebSockets(reason);
    this.#rejectAll(reason);
  }

  #closeWebSockets(reason: string): void {
    for (const callbacks of this.#webSockets.values()) {
      callbacks.close(1011, reason.slice(0, 123));
    }
    this.#webSockets.clear();
  }

  #closedNotification(): void {
    const notify = this.#onClose;
    this.#onClose = undefined;
    notify?.();
  }
}
