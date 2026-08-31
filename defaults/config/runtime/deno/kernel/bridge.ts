import type { ServiceRequestMetadata } from "../worker/contracts.ts";
import { type KernelInvoke, kernelInvokeSymbol } from "./mod.ts";

interface BridgeMessage {
  type: string;
  correlationId?: string;
  payload?: unknown;
  error?: string;
}

interface Pending {
  resolve(value: unknown): void;
  reject(reason: Error): void;
}

export interface KernelBridge {
  withRequest<Result>(
    metadata: ServiceRequestMetadata,
    invoke: () => Result,
  ): Result;
  beginPersistentRequest(metadata: ServiceRequestMetadata): void;
  endPersistentRequest(
    metadata: ServiceRequestMetadata,
    successful: boolean,
  ): void;
  handle(message: BridgeMessage): boolean;
  close(): void;
}

export function createKernelBridge(port: MessagePort): KernelBridge {
  let synchronousRequest: ServiceRequestMetadata | undefined;
  let sequence = 0;
  const persistentRequests = new Map<
    string,
    {
      metadata: ServiceRequestMetadata;
      active: number;
      expiresAt: number;
      timer?: ReturnType<typeof setTimeout>;
    }
  >();
  const pending = new Map<string, Pending>();
  const activeRequests = new Map<string, ServiceRequestMetadata>();
  const invoke: KernelInvoke = (operation, input) => {
    const request = synchronousRequest ??
      (activeRequests.size === 1
        ? activeRequests.values().next().value
        : activeRequests.size === 0
        ? persistentRequests.size === 1
          ? persistentRequests.values().next().value?.metadata
          : undefined
        : undefined);
    if (request === undefined) {
      return Promise.reject(
        new Error(
          activeRequests.size === 0
            ? "kernel API call must begin inside a service request"
            : "kernel API request context is ambiguous",
        ),
      );
    }
    if (operation === "auth.currentUser") {
      const auth = request.auth;
      return Promise.resolve(
        auth.authenticated && auth.realm === "bootstrap-admin" &&
          auth.userId !== undefined && auth.username !== undefined
          ? {
            id: auth.userId,
            username: auth.username,
            realm: auth.realm,
          }
          : undefined,
      );
    }
    if (
      operation === "execution.completePersistent" &&
      request.persistentExecutionId === undefined
    ) {
      return Promise.reject(
        new Error("persistent execution context is unavailable"),
      );
    }
    const correlationId = `kernel-${++sequence}-${crypto.randomUUID()}`;
    const result = new Promise<unknown>((resolve, reject) => {
      pending.set(correlationId, { resolve, reject });
    });
    port.postMessage({
      type: "kernel_call",
      correlationId,
      payload: {
        operation,
        arguments: input,
        request: {
          requestId: request.requestId,
          serviceId: request.serviceId,
          persistentExecutionId: request.persistentExecutionId,
        },
      },
    });
    return result;
  };
  (globalThis as unknown as Record<symbol, unknown>)[kernelInvokeSymbol] =
    invoke;

  return {
    beginPersistentRequest(metadata: ServiceRequestMetadata): void {
      const executionId = metadata.persistentExecutionId;
      const keepAlive = metadata.persistentKeepAliveMilliseconds;
      if (executionId === undefined || keepAlive === undefined) return;
      const existing = persistentRequests.get(executionId);
      if (existing !== undefined) {
        if (existing.timer !== undefined) clearTimeout(existing.timer);
        existing.timer = undefined;
        existing.metadata = metadata;
        existing.active++;
        return;
      }
      persistentRequests.set(executionId, {
        metadata,
        active: 1,
        expiresAt: Date.now() + keepAlive,
      });
    },
    endPersistentRequest(
      metadata: ServiceRequestMetadata,
      successful: boolean,
    ): void {
      const executionId = metadata.persistentExecutionId;
      if (executionId === undefined) return;
      const entry = persistentRequests.get(executionId);
      if (entry === undefined) return;
      entry.active = Math.max(0, entry.active - 1);
      if (entry.active > 0) return;
      if (successful) {
        entry.expiresAt = Date.now() +
          (metadata.persistentKeepAliveMilliseconds ?? 0);
      }
      const remaining = Math.max(0, entry.expiresAt - Date.now());
      entry.timer = setTimeout(() => {
        const current = persistentRequests.get(executionId);
        if (current === entry && current.active === 0) {
          persistentRequests.delete(executionId);
        }
      }, remaining);
    },
    withRequest<Result>(
      metadata: ServiceRequestMetadata,
      callback: () => Result,
    ): Result {
      if (activeRequests.has(metadata.requestId)) {
        throw new Error("duplicate active kernel request context");
      }
      activeRequests.set(metadata.requestId, metadata);
      const previous = synchronousRequest;
      synchronousRequest = metadata;
      let result: Result;
      try {
        result = callback();
      } catch (error) {
        activeRequests.delete(metadata.requestId);
        throw error;
      } finally {
        synchronousRequest = previous;
      }
      if (
        result !== null && typeof result === "object" &&
        typeof (result as { then?: unknown }).then === "function"
      ) {
        return Promise.resolve(result).finally(() => {
          activeRequests.delete(metadata.requestId);
        }) as Result;
      }
      activeRequests.delete(metadata.requestId);
      return result;
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
      if (message.error !== undefined) call.reject(new Error(message.error));
      else call.resolve(message.payload);
      return true;
    },
    close(): void {
      delete (globalThis as unknown as Record<symbol, unknown>)[
        kernelInvokeSymbol
      ];
      for (const call of pending.values()) {
        call.reject(new Error("kernel API bridge closed"));
      }
      pending.clear();
      activeRequests.clear();
      for (const request of persistentRequests.values()) {
        if (request.timer !== undefined) clearTimeout(request.timer);
      }
      persistentRequests.clear();
    },
  };
}
