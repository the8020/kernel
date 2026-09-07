import { isId, newId } from "../identity/mod.ts";
import { AsyncLocalStorage } from "node:async_hooks";
// Preload the public module in the trusted bootstrap graph so applications can
// import it without gaining read access to the runtime implementation.
import "../context/mod.ts";
import { installContextProvider } from "../context/runtime.ts";
import type { ExecutionContext } from "../context/types.ts";
import type {
  ExecutionMetadata,
  ExecutionUserMetadata,
  ServiceRequestMetadata,
} from "../worker/contracts.ts";
import {
  canonicalExecutionOrigin,
  canonicalExecutionUser,
} from "../worker/contracts.ts";
import {
  type DatabaseBackend,
  kernelDatabaseBackendSymbol,
  type KernelInvoke,
  kernelInvokeSymbol,
  kernelPersistentRunSymbol,
  kernelSecretSymbol,
} from "./mod.ts";

export interface KernelExecutionContext {
  readonly contextId: string;
  readonly parentContextId?: string;
  readonly jobRunId?: string;
  readonly serviceId: string;
  readonly persistentExecutionId?: string;
  readonly persistentKeepAliveMilliseconds?: number;
  readonly user: ExecutionUserMetadata;
  readonly auth?: ServiceRequestMetadata["auth"];
  readonly secrets?: Record<string, string>;
  readonly signal?: AbortSignal;
}

interface BridgeMessage {
  type: string;
  correlationId?: string;
  payload?: unknown;
  error?: string;
}

interface Pending {
  resolve(value: unknown): void;
  reject(reason: Error): void;
  cleanup(): void;
}

export interface KernelBridge {
  executionContext(): KernelExecutionContext | undefined;
  withRequest<Result>(
    metadata: ServiceRequestMetadata,
    invoke: () => Result,
    signal?: AbortSignal,
  ): Result;
  withExecution<Result>(
    metadata: KernelExecutionContext,
    invoke: () => Result,
  ): Result;
  closeExecution(): Promise<void>;
  handle(message: BridgeMessage): boolean;
  close(): void;
}

export function createKernelBridge(
  port: MessagePort,
  metadata: ExecutionMetadata,
): KernelBridge {
  const worker = Object.freeze({
    nodeId: metadata.nodeId,

    sandboxId: metadata.sandboxId,
    workerId: metadata.workerId,
    workloadType: metadata.workloadType,
    workloadId: metadata.workloadId,
    serviceId: metadata.service?.serviceId,
    databaseBackend: metadata.databaseBackend,
    user: canonicalExecutionUser(metadata.user),
    origin: canonicalExecutionOrigin(metadata.origin, metadata.workloadType),
  });
  const databaseBackend: DatabaseBackend = worker.databaseBackend;
  const requestContext = new AsyncLocalStorage<KernelExecutionContext>();
  const lifetime = new AbortController();
  const pending = new Map<string, Pending>();
  const releaseContextProvider = installContextProvider(() => {
    const active = requestContext.getStore();
    if (active === undefined) return undefined;
    const user = active.user;
    const origin = worker.origin;
    return Object.freeze({
      type: origin.type,
      id: origin.id,
      userId: user.userId,
      username: user.username,
      authenticated: active.auth?.authenticated === true,
      nodeId: worker.nodeId,

      sandboxId: worker.sandboxId,
      workerId: worker.workerId,
      contextId: active.contextId,
      parentContextId: active.parentContextId,
      jobRunId: active.jobRunId,
      serviceInstanceId: worker.workloadType === "service"
        ? worker.workloadId
        : undefined,
      persistentExecutionId: active.persistentExecutionId,
    }) satisfies ExecutionContext;
  });
  const invoke: KernelInvoke = (operation, input, callSignal) => {
    const request = requestContext.getStore();
    if (request === undefined && operation !== "database.info") {
      return Promise.reject(
        new Error("kernel API call must begin inside an execution"),
      );
    }
    if (
      (operation === "execution.completePersistent" ||
        operation === "execution.retainPersistent") &&
      request?.persistentExecutionId === undefined
    ) {
      return Promise.reject(
        new Error("persistent execution context is unavailable"),
      );
    }
    const correlationId = newId("cor");
    if (pending.has(correlationId)) {
      return Promise.reject(new Error("kernel correlation collision"));
    }
    const result = new Promise<unknown>((resolve, reject) => {
      const cleanupOperation = operation === "database.scope.close" ||
        operation === "runtime.operation" &&
          input.operation === "terminal.detach";
      const signal = cleanupOperation
        ? undefined
        : callSignal && request?.signal
        ? AbortSignal.any([callSignal, request.signal])
        : callSignal ?? request?.signal;
      const abort = (): void => {
        const call = pending.get(correlationId);
        if (call === undefined) return;
        pending.delete(correlationId);
        call.cleanup();
        port.postMessage({ type: "kernel_cancel", correlationId });
        reject(signal?.reason ?? new DOMException("Aborted", "AbortError"));
      };
      const cleanup = (): void => {
        signal?.removeEventListener("abort", abort);
      };
      pending.set(correlationId, { resolve, reject, cleanup });
      signal?.addEventListener("abort", abort, { once: true });
      if (signal?.aborted) abort();
    });
    if (!pending.has(correlationId)) return result;
    port.postMessage({
      type: "kernel_call",
      correlationId,
      payload: {
        operation,
        arguments: input,
        request: request === undefined ? undefined : {
          contextId: request.contextId,
          parentContextId: request.parentContextId,
          jobRunId: request.jobRunId,
          serviceId: request.serviceId,
          persistentExecutionId: request.persistentExecutionId,
          user: request.user,
        },
      },
    });
    return result;
  };
  (globalThis as unknown as Record<symbol, unknown>)[kernelInvokeSymbol] =
    invoke;
  (globalThis as unknown as Record<symbol, unknown>)[
    kernelPersistentRunSymbol
  ] = (handler: () => Promise<void>): Promise<void> => {
    const request = requestContext.getStore();
    if (
      worker.workloadType !== "service" ||
      request?.persistentExecutionId === undefined ||
      request.persistentKeepAliveMilliseconds !== 0
    ) {
      return Promise.reject(
        new Error("runPersistent requires a zero-keepalive service execution"),
      );
    }
    const retained = Object.freeze({ ...request, signal: lifetime.signal });
    return requestContext.run(retained, async () => {
      // Pin ownership before calling application code. A lost establishment
      // response can no longer discard work the handler accepted.
      const claim = await invoke("execution.retainPersistent", {}) as {
        contextId: string;
      };
      try {
        if (!isId(claim.contextId, "ctx")) {
          throw new Error("retained execution requires a canonical context");
        }
        await requestContext.run(
          Object.freeze({
            ...retained,
            contextId: claim.contextId,
            parentContextId: request.contextId,
          }),
          async () => {
            try {
              await handler();
            } finally {
              if (
                !lifetime.signal.aborted && metadata.databaseAccess !== "none"
              ) {
                // The retained scope is independent of the original HTTP scope.
                // Abrupt Worker release repeats cleanup for every remaining scope.
                await invoke("database.scope.close", {}).catch(() => {});
              }
            }
          },
        );
      } finally {
        if (!lifetime.signal.aborted) {
          await invoke("execution.completePersistent", {});
        }
      }
    });
  };
  (globalThis as unknown as Record<symbol, unknown>)[kernelSecretSymbol] = (
    name: string,
  ): string | undefined => requestContext.getStore()?.secrets?.[name];
  (globalThis as unknown as Record<symbol, unknown>)[
    kernelDatabaseBackendSymbol
  ] = databaseBackend;

  return {
    executionContext: () => requestContext.getStore(),
    async closeExecution(): Promise<void> {
      await invoke("database.scope.close", {});
    },
    withRequest<Result>(
      metadata: ServiceRequestMetadata,
      callback: () => Result,
      signal?: AbortSignal,
    ): Result {
      return requestContext.run(
        Object.freeze({
          contextId: metadata.contextId,
          parentContextId: metadata.parentContextId,
          serviceId: metadata.serviceId,
          persistentExecutionId: metadata.persistentExecutionId,
          persistentKeepAliveMilliseconds:
            metadata.persistentKeepAliveMilliseconds,
          user: canonicalExecutionUser(metadata.user),
          auth: Object.freeze({ ...metadata.auth }),
          signal,
        }),
        callback,
      );
    },
    withExecution<Result>(
      metadata: KernelExecutionContext,
      callback: () => Result,
    ): Result {
      return requestContext.run(immutableKernelContext(metadata), callback);
    },
    handle(message: BridgeMessage): boolean {
      if (
        message.type !== "kernel_result" || message.correlationId === undefined
      ) {
        return false;
      }
      const call = pending.get(message.correlationId);
      if (call === undefined) return true;
      pending.delete(message.correlationId);
      call.cleanup();
      if (message.error !== undefined) call.reject(new Error(message.error));
      else call.resolve(message.payload);
      return true;
    },
    close(): void {
      lifetime.abort(new Error("Worker lifetime ended"));
      delete (globalThis as unknown as Record<symbol, unknown>)[
        kernelPersistentRunSymbol
      ];
      delete (globalThis as unknown as Record<symbol, unknown>)[
        kernelInvokeSymbol
      ];
      delete (globalThis as unknown as Record<symbol, unknown>)[
        kernelSecretSymbol
      ];
      delete (globalThis as unknown as Record<symbol, unknown>)[
        kernelDatabaseBackendSymbol
      ];
      for (const call of pending.values()) {
        call.cleanup();
        call.reject(new Error("kernel API bridge closed"));
      }
      pending.clear();
      requestContext.disable();
      releaseContextProvider();
    },
  };
}

function immutableKernelContext(
  value: KernelExecutionContext,
): KernelExecutionContext {
  return Object.freeze({
    contextId: value.contextId,
    parentContextId: value.parentContextId,
    jobRunId: value.jobRunId,
    serviceId: value.serviceId,
    persistentExecutionId: value.persistentExecutionId,
    persistentKeepAliveMilliseconds: value.persistentKeepAliveMilliseconds,
    user: canonicalExecutionUser(value.user),
    auth: value.auth === undefined
      ? undefined
      : Object.freeze({ ...value.auth }),
    secrets: value.secrets,
    signal: value.signal,
  });
}
