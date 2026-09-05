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
  kernelSecretSymbol,
} from "./mod.ts";

export interface KernelExecutionContext {
  readonly requestId: string;
  readonly serviceId: string;
  readonly persistentExecutionId?: string;
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
    runtimeGroupId: metadata.runtimeGroupId,
    sandboxId: metadata.sandboxId,
    workerId: metadata.workerId,
    executionId: metadata.executionId,
    workloadType: metadata.workloadType,
    workloadId: metadata.workloadId,
    serviceId: metadata.service?.serviceId,
    databaseBackend: metadata.databaseBackend,
    user: canonicalExecutionUser(metadata.user),
    origin: canonicalExecutionOrigin(metadata.origin, metadata.workloadType),
  });
  const databaseBackend: DatabaseBackend = worker.databaseBackend;
  let sequence = 0;
  const requestContext = new AsyncLocalStorage<KernelExecutionContext>();
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
      runtimeGroupId: worker.runtimeGroupId,
      sandboxId: worker.sandboxId,
      workerId: worker.workerId,
      executionId: worker.executionId,
      requestId: active.requestId,
      persistentExecutionId: active.persistentExecutionId,
    }) satisfies ExecutionContext;
  });
  const invoke: KernelInvoke = (operation, input) => {
    const request = requestContext.getStore();
    if (request === undefined && operation !== "database.info") {
      return Promise.reject(
        new Error("kernel API call must begin inside an execution"),
      );
    }
    if (
      operation === "execution.completePersistent" &&
      request?.persistentExecutionId === undefined
    ) {
      return Promise.reject(
        new Error("persistent execution context is unavailable"),
      );
    }
    const correlationId = `kernel-${++sequence}-${crypto.randomUUID()}`;
    const result = new Promise<unknown>((resolve, reject) => {
      const signal = operation === "database.scope.close"
        ? undefined
        : request?.signal;
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
          requestId: request.requestId,
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
  (globalThis as unknown as Record<symbol, unknown>)[kernelSecretSymbol] = (
    name: string,
  ): string | undefined => requestContext.getStore()?.secrets?.[name];
  (globalThis as unknown as Record<symbol, unknown>)[
    kernelDatabaseBackendSymbol
  ] = databaseBackend;

  return {
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
          requestId: metadata.requestId,
          serviceId: metadata.serviceId,
          persistentExecutionId: metadata.persistentExecutionId,
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
    requestId: value.requestId,
    serviceId: value.serviceId,
    persistentExecutionId: value.persistentExecutionId,
    user: canonicalExecutionUser(value.user),
    auth: value.auth === undefined
      ? undefined
      : Object.freeze({ ...value.auth }),
    secrets: value.secrets,
    signal: value.signal,
  });
}
