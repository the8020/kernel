import {
  assertEnvelope,
  type Envelope,
  type MessageType,
  PROTOCOL_VERSION,
} from "@the8020/protocol";
import type {
  ExecutionMetadata,
  KernelCall,
  RuntimeLogEvent,
  ServiceRequestMetadata,
  WorkerPermissionSet,
  WorkloadType,
} from "../worker/contracts.ts";
import {
  RuntimeWorker,
  WorkerExecutionError,
} from "../worker/runtime_worker.ts";
import type { WorkerInvocationResult } from "../worker/runtime_worker.ts";

export interface SupervisorOptions {
  runtimeGroupId: string;
  sandboxId: string;
  workloadType: WorkloadType;
  token: string;
  supervisorVersion: string;
  denoVersion?: string;
  startedAt?: number;
  now?: () => number;
  workerStopGraceMilliseconds?: number;
  kernelCall?: KernelCall;
  nodeId?: string;
  entrypointValidator?: (entrypoints: string[]) => Promise<void>;
  moduleAnalyzer?: (entrypoints: string[]) => Promise<ModuleDependencies>;
  onStateChange?: () => void;
}

export type ModuleDependencies = Record<string, string[]>;

export interface StartWorkerOptions {
  metadata: ExecutionMetadata;
  permissions: WorkerPermissionSet;
}

export interface WorkerStatus {
  worker_id: string;
  execution_id: string;
  workload_id: string;
  owner_id: string;
  debugger_name: string;
  entrypoint: string;
  release_id: string;
  in_flight: number;
  idle_since_ms?: number;
  persistent_executions: number;
  state: "starting" | "ready" | "stopping" | "stopped" | "failed";
  failure?: string;
  logs: RuntimeLogEvent[];
}

interface PersistentBinding {
  workerId: string;
  expiresAt: number;
  connections: number;
}

interface ServiceWorkerLease {
  worker: RuntimeWorker;
  executionId?: string;
  keepAliveMilliseconds: number;
  created: boolean;
  admitted: boolean;
}

interface ServicePool {
  workers: Set<string>;
  concurrencyPerWorker: number;
  queueLimit: number;
  queued: number;
  executionMode: "stateless" | "persistent";
  bindings: Map<string, PersistentBinding>;
  admissions: Map<string, number>;
}

function canonicalJSON(value: unknown): string {
  const normalize = (item: unknown): unknown => {
    if (Array.isArray(item)) return item.map(normalize);
    if (item !== null && typeof item === "object") {
      return Object.fromEntries(
        Object.entries(item).filter(([, nested]) => nested !== undefined)
          .sort(([left], [right]) => left.localeCompare(right))
          .map(([key, nested]) => [key, normalize(nested)]),
      );
    }
    return item;
  };
  return JSON.stringify(normalize(value));
}

function routingLimit(concurrencyPerWorker: number): number {
  return concurrencyPerWorker === 1 ? 1 : concurrencyPerWorker + 1;
}

export class Supervisor {
  readonly options:
    & Required<
      Omit<
        SupervisorOptions,
        | "kernelCall"
        | "nodeId"
        | "entrypointValidator"
        | "moduleAnalyzer"
        | "onStateChange"
      >
    >
    & {
      kernelCall?: KernelCall;
      nodeId: string;
    };
  #workers = new Map<string, RuntimeWorker>();
  #servicePools = new Map<string, ServicePool>();
  #draining = false;
  #workerStartFingerprints = new Map<string, string>();
  #workerStarts = new Map<
    string,
    { fingerprint: string; promise: Promise<RuntimeWorker> }
  >();
  #workerStops = new Map<string, Promise<void>>();
  #lastReservationRelease = new Map<string, number>();
  #capacityWaiters = new Set<() => void>();
  #recentFailures: Array<{
    worker_id: string;
    execution_id: string;
    reason: string;
  }> = [];
  #entrypointValidator: (entrypoints: string[]) => Promise<void>;
  #moduleAnalyzer: (entrypoints: string[]) => Promise<ModuleDependencies>;
  #onStateChange?: () => void;
  #revision = 1;

  constructor(options: SupervisorOptions) {
    if (
      options.runtimeGroupId.length === 0 || options.sandboxId.length === 0 ||
      options.token.length < 16
    ) {
      throw new TypeError(
        "runtime-group ID, sandbox ID, and high-entropy token are required",
      );
    }
    const { entrypointValidator, moduleAnalyzer, ...runtimeOptions } = options;
    this.options = {
      ...runtimeOptions,
      denoVersion: options.denoVersion ?? Deno.version.deno,
      startedAt: options.startedAt ?? (options.now ?? Date.now)(),
      now: options.now ?? Date.now,
      workerStopGraceMilliseconds: options.workerStopGraceMilliseconds ?? 1_000,
      nodeId: options.nodeId ?? options.runtimeGroupId,
    };
    this.#entrypointValidator = entrypointValidator ?? validateEntrypoints;
    this.#moduleAnalyzer = moduleAnalyzer ?? analyzeModules;
    this.#onStateChange = options.onStateChange;
    if (
      !Number.isSafeInteger(this.options.workerStopGraceMilliseconds) ||
      this.options.workerStopGraceMilliseconds < 10
    ) {
      throw new TypeError(
        "Worker stop grace must be an integer of at least 10 milliseconds",
      );
    }
  }

  async startWorker(options: StartWorkerOptions): Promise<RuntimeWorker> {
    if (this.#draining) throw new Error("runtime group is draining");
    if (options.metadata.workloadType !== this.options.workloadType) {
      throw new Error("Worker workload type does not match runtime group");
    }
    if (
      options.metadata.databaseBackend !== "sqlite" &&
      options.metadata.databaseBackend !== "postgresql"
    ) {
      throw new Error("Worker database backend metadata is required");
    }
    const metadata = {
      ...options.metadata,
      nodeId: this.options.nodeId,
      runtimeGroupId: this.options.runtimeGroupId,
      sandboxId: this.options.sandboxId,
    };
    const fingerprint = canonicalJSON({
      metadata,
      permissions: options.permissions,
    });
    const existing = this.#workers.get(options.metadata.workerId);
    if (existing !== undefined) {
      if (
        !existing.closed &&
        this.#workerStartFingerprints.get(options.metadata.workerId) ===
          fingerprint
      ) {
        await existing.ready;
        return existing;
      }
      throw new Error(
        `Worker ${options.metadata.workerId} already exists with different configuration`,
      );
    }
    const starting = this.#workerStarts.get(options.metadata.workerId);
    if (starting !== undefined) {
      if (starting.fingerprint === fingerprint) return await starting.promise;
      throw new Error(
        `Worker ${options.metadata.workerId} already exists with different configuration`,
      );
    }
    const promise = this.#startNewWorker(options, metadata, fingerprint);
    this.#workerStarts.set(options.metadata.workerId, { fingerprint, promise });
    try {
      return await promise;
    } finally {
      if (
        this.#workerStarts.get(options.metadata.workerId)?.promise === promise
      ) {
        this.#workerStarts.delete(options.metadata.workerId);
      }
    }
  }

  async #startNewWorker(
    options: StartWorkerOptions,
    metadata: ExecutionMetadata,
    fingerprint: string,
  ): Promise<RuntimeWorker> {
    if (options.metadata.validateEntrypoint === true) {
      await this.#entrypointValidator([options.metadata.entrypoint]);
    }
    if (this.#draining) throw new Error("runtime group is draining");
    const worker = new RuntimeWorker({
      ...options,
      permissions: { ...options.permissions, net: true, import: true },
      metadata,
      now: this.options.now,
      onCapacityChange: () => this.#notifyCapacity(),
      onClose: () => {
        if (metadata.databaseAccess === "none") return;
        void this.options.kernelCall?.({
          operation: "database.scope.close",
          arguments: {},
          executionId: metadata.executionId,
          workerId: metadata.workerId,
        }).catch(() => {});
      },
      kernelCall: this.options.kernelCall === undefined
        ? undefined
        : async (call, signal) => {
          const databaseAccess = metadata.databaseAccess ?? "full";
          if (
            call.operation.startsWith("database.") &&
            call.operation !== "database.scope.close" &&
            databaseAccess !== "full"
          ) {
            throw new Error(
              "database SQL is not available to this Worker",
            );
          }
          const result = await this.options.kernelCall!(call, signal);
          if (
            call.operation === "execution.completePersistent" &&
            call.persistentExecutionId !== undefined
          ) {
            this.completePersistentExecution(
              metadata.workloadId,
              call.persistentExecutionId,
              call.workerId,
            );
          }
          return result;
        },
    });
    this.#workers.set(options.metadata.workerId, worker);
    this.#workerStartFingerprints.set(options.metadata.workerId, fingerprint);
    this.#stateChanged();
    try {
      await worker.ready;
      return worker;
    } catch (error) {
      this.#recordFailure(worker, error);
      this.#workers.delete(options.metadata.workerId);
      this.#workerStartFingerprints.delete(options.metadata.workerId);
      worker.kill();
      this.#stateChanged();
      throw error;
    }
  }

  completePersistentExecution(
    serviceId: string,
    executionId: string,
    workerId: string,
  ): void {
    const pool = this.#servicePools.get(serviceId);
    const binding = pool?.bindings.get(executionId);
    if (binding === undefined) return;
    if (binding.workerId !== workerId) {
      throw new Error("persistent execution binding does not match Worker");
    }
    pool!.bindings.delete(executionId);
    this.#lastReservationRelease.set(workerId, this.options.now());
    this.#notifyCapacity();
  }

  async invokeWorker(
    workerId: string,
    functionName: string,
    input: unknown,
    signal: AbortSignal,
    persistentExecutionId?: string,
  ): Promise<WorkerInvocationResult> {
    const worker = this.#workers.get(workerId);
    if (worker === undefined || worker.closed || worker.draining) {
      return {
        ok: false,
        error: {
          code: "target_not_found",
          message: `Worker ${workerId} is unavailable`,
        },
      };
    }
    if (persistentExecutionId !== undefined) {
      const binding = this.#servicePools.get(worker.metadata.workloadId)
        ?.bindings.get(persistentExecutionId);
      if (binding?.workerId !== workerId) {
        return {
          ok: false,
          error: {
            code: "target_mismatch",
            message: "persistent execution does not match Worker",
          },
        };
      }
    }
    return await worker.invoke(
      functionName,
      input,
      signal,
      persistentExecutionId,
    );
  }

  async stopWorker(workerId: string, immediate = false): Promise<void> {
    const stopping = this.#workerStops.get(workerId);
    if (stopping !== undefined) return await stopping;
    const promise = this.#stopWorker(workerId, immediate);
    this.#workerStops.set(workerId, promise);
    try {
      await promise;
    } finally {
      if (this.#workerStops.get(workerId) === promise) {
        this.#workerStops.delete(workerId);
      }
    }
  }

  async #stopWorker(workerId: string, immediate: boolean): Promise<void> {
    let worker = this.#workers.get(workerId);
    if (worker === undefined) {
      const starting = this.#workerStarts.get(workerId);
      if (starting === undefined) return;
      try {
        worker = await starting.promise;
      } catch {
        return;
      }
    }
    for (const pool of this.#servicePools.values()) {
      this.#sweepPersistentBindings(pool);
      const bound = [...pool.bindings.values()].some((binding) =>
        binding.workerId === workerId
      );
      if (bound && !immediate) {
        throw new Error(
          `Worker ${workerId} still owns persistent execution slots`,
        );
      }
      if (bound) {
        for (const [executionId, binding] of pool.bindings) {
          if (binding.workerId === workerId) pool.bindings.delete(executionId);
        }
      }
    }
    if (immediate) worker.kill();
    else await worker.stop(this.options.workerStopGraceMilliseconds);
    this.#workers.delete(workerId);
    this.#workerStartFingerprints.delete(workerId);
    this.#lastReservationRelease.delete(workerId);
    for (const pool of this.#servicePools.values()) {
      pool.workers.delete(workerId);
    }
    this.#notifyCapacity();
  }

  configureService(
    serviceId: string,
    workerIds: string[],
    concurrencyPerWorker: number,
    queueLimit = Math.max(
      1,
      Math.min(
        1_024,
        workerIds.length * Math.min(concurrencyPerWorker, 1_024),
      ),
    ),
  ): void {
    if (
      !Number.isSafeInteger(concurrencyPerWorker) || concurrencyPerWorker < 1
    ) {
      throw new TypeError("concurrency per Worker must be positive");
    }
    if (!Number.isSafeInteger(queueLimit) || queueLimit < 1) {
      throw new TypeError("service queue limit must be positive");
    }
    const pool = new Set<string>();
    for (const workerId of workerIds) {
      const worker = this.#workers.get(workerId);
      if (
        worker === undefined || worker.closed ||
        worker.draining ||
        worker.metadata.workloadId !== serviceId
      ) {
        throw new Error(
          `Worker ${workerId} does not belong to service ${serviceId}`,
        );
      }
      pool.add(workerId);
    }
    const previous = this.#servicePools.get(serviceId);
    const modes = [...pool].map((workerId) =>
      this.#workers.get(workerId)?.metadata.service?.executionMode
    ).filter((mode): mode is "stateless" | "persistent" =>
      mode === "stateless" || mode === "persistent"
    );
    const executionMode = modes[0] ?? previous?.executionMode ?? "stateless";
    if (modes.some((mode) => mode !== executionMode)) {
      throw new TypeError("service pool Workers disagree on execution mode");
    }
    const bindings = previous?.bindings ?? new Map<string, PersistentBinding>();
    const admissions = previous?.admissions ?? new Map<string, number>();
    const nextPool = {
      workers: pool,
      concurrencyPerWorker,
      queueLimit,
      queued: this.#servicePools.get(serviceId)?.queued ?? 0,
      executionMode,
      bindings,
      admissions,
    };
    this.#sweepPersistentBindings(nextPool);
    for (const binding of bindings.values()) {
      const worker = this.#workers.get(binding.workerId);
      if (worker !== undefined && !worker.closed) pool.add(binding.workerId);
    }
    this.#servicePools.set(serviceId, nextPool);
    this.#notifyCapacity();
  }

  selectServiceWorker(serviceId: string): RuntimeWorker {
    const pool = this.#servicePools.get(serviceId);
    if (pool === undefined || pool.workers.size === 0) {
      throw new Error(`service ${serviceId} has no ready Workers`);
    }
    const workers = [...pool.workers].map((id) => this.#workers.get(id)!)
      .filter((worker) =>
        !worker.closed && !worker.draining &&
        this.#workerLoad(pool, worker) < routingLimit(pool.concurrencyPerWorker)
      )
      .sort((left, right) =>
        Number(this.#workerLoad(pool, left) >= pool.concurrencyPerWorker) -
          Number(this.#workerLoad(pool, right) >= pool.concurrencyPerWorker) ||
        this.#workerLoad(pool, left) - this.#workerLoad(pool, right) ||
        left.metadata.workerId.localeCompare(right.metadata.workerId)
      );
    if (workers.length === 0) {
      throw new Error(
        `service ${serviceId} has no Worker below its in-flight limit`,
      );
    }
    return workers[0]!;
  }

  async #acquireServiceWorker(
    serviceId: string,
    signal: AbortSignal,
  ): Promise<RuntimeWorker> {
    const pool = this.#servicePools.get(serviceId);
    if (pool === undefined || pool.workers.size === 0) {
      throw new ServiceUnavailableError(
        `service ${serviceId} has no ready Workers`,
      );
    }
    try {
      return this.#admitSelectedWorker(serviceId, pool);
    } catch {
      if (pool.queued >= pool.queueLimit) {
        throw new ServiceUnavailableError(
          `service ${serviceId} request queue is full`,
        );
      }
    }
    pool.queued++;
    try {
      while (!signal.aborted) {
        try {
          return this.#admitSelectedWorker(serviceId, pool);
        } catch {
          await this.#waitForCapacity(pool, signal);
        }
      }
      throw signal.reason ??
        new DOMException("Request cancelled", "AbortError");
    } finally {
      pool.queued = Math.max(0, pool.queued - 1);
    }
  }

  async #acquireServiceWorkerLease(
    serviceId: string,
    headers: Headers,
    signal: AbortSignal,
  ): Promise<ServiceWorkerLease> {
    const pool = this.#servicePools.get(serviceId);
    if (pool === undefined) {
      throw new ServiceUnavailableError(`service ${serviceId} is unavailable`);
    }
    const targetWorkerID = headers.get(
      "x-80-20-internal-target-worker-id",
    );
    if (targetWorkerID !== null) {
      const target = pool.workers.has(targetWorkerID)
        ? this.#workers.get(targetWorkerID)
        : undefined;
      if (target === undefined || target.closed || target.draining) {
        throw new ServiceUnavailableError(
          `target Worker ${targetWorkerID} is unavailable`,
        );
      }
      const executionId = headers.get(
        "x-80-20-internal-persistent-execution-id",
      ) ?? "";
      if (pool.executionMode === "persistent" && executionId !== "") {
        const keepAliveMilliseconds = Number(
          headers.get("x-80-20-internal-persistent-keep-alive-ms") ?? "0",
        );
        if (
          !Number.isSafeInteger(keepAliveMilliseconds) ||
          keepAliveMilliseconds < 1
        ) {
          throw new TypeError(
            "persistent keepalive must be a positive integer",
          );
        }
        this.#sweepPersistentBindings(pool);
        const existing = pool.bindings.get(executionId);
        if (existing !== undefined && existing.workerId !== targetWorkerID) {
          throw new ServiceUnavailableError(
            `persistent execution ${executionId} belongs to another Worker`,
          );
        }
        if (existing === undefined) {
          let reservations = 0;
          for (const binding of pool.bindings.values()) {
            if (binding.workerId === targetWorkerID) reservations++;
          }
          if (reservations >= routingLimit(pool.concurrencyPerWorker)) {
            throw new ServiceUnavailableError(
              `target Worker ${targetWorkerID} has no persistent execution slot`,
            );
          }
          pool.bindings.set(executionId, {
            workerId: targetWorkerID,
            expiresAt: this.options.now() + keepAliveMilliseconds,
            connections: 0,
          });
          this.#notifyCapacity();
        }
        return await this.#admitPersistentLease(serviceId, pool, {
          worker: target,
          executionId,
          keepAliveMilliseconds,
          created: existing === undefined,
          admitted: false,
        }, signal);
      }
      await this.#waitForWorkerCapacity(serviceId, pool, target, signal);
      return {
        worker: target,
        keepAliveMilliseconds: 0,
        created: false,
        admitted: true,
      };
    }
    if (pool.executionMode === "stateless") {
      return {
        worker: await this.#acquireServiceWorker(serviceId, signal),
        keepAliveMilliseconds: 0,
        created: false,
        admitted: true,
      };
    }
    const executionId = headers.get(
      "x-80-20-internal-persistent-execution-id",
    ) ?? "";
    const keepAliveMilliseconds = Number(
      headers.get("x-80-20-internal-persistent-keep-alive-ms") ?? "0",
    );
    if (executionId.length === 0 || executionId.length > 256) {
      throw new TypeError("persistent execution ID is required");
    }
    if (
      !Number.isSafeInteger(keepAliveMilliseconds) ||
      keepAliveMilliseconds < 1
    ) throw new TypeError("persistent keepalive must be a positive integer");

    const acquire = (): ServiceWorkerLease | undefined => {
      this.#sweepPersistentBindings(pool);
      const existing = pool.bindings.get(executionId);
      if (existing !== undefined) {
        const worker = this.#workers.get(existing.workerId);
        if (worker !== undefined && !worker.closed && !worker.draining) {
          return {
            worker,
            executionId,
            keepAliveMilliseconds,
            created: false,
            admitted: false,
          };
        }
        pool.bindings.delete(executionId);
      }
      const reserved = new Map<string, number>();
      for (const binding of pool.bindings.values()) {
        reserved.set(
          binding.workerId,
          (reserved.get(binding.workerId) ?? 0) + 1,
        );
      }
      const candidates = [...pool.workers].map((id) => this.#workers.get(id)!)
        .filter((worker) =>
          !worker.closed && !worker.draining &&
          (reserved.get(worker.metadata.workerId) ?? 0) <
            routingLimit(pool.concurrencyPerWorker)
        ).sort((left, right) =>
          Number(
              (reserved.get(left.metadata.workerId) ?? 0) >=
                pool.concurrencyPerWorker,
            ) - Number(
              (reserved.get(right.metadata.workerId) ?? 0) >=
                pool.concurrencyPerWorker,
            ) ||
          this.#workerLoad(pool, left) - this.#workerLoad(pool, right) ||
          (reserved.get(left.metadata.workerId) ?? 0) -
            (reserved.get(right.metadata.workerId) ?? 0) ||
          left.metadata.workerId.localeCompare(right.metadata.workerId)
        );
      const worker = candidates[0];
      if (worker === undefined) return undefined;
      pool.bindings.set(executionId, {
        workerId: worker.metadata.workerId,
        expiresAt: this.options.now() + keepAliveMilliseconds,
        connections: 0,
      });
      this.#notifyCapacity();
      return {
        worker,
        executionId,
        keepAliveMilliseconds,
        created: true,
        admitted: false,
      };
    };

    let lease = acquire();
    if (lease !== undefined) {
      return await this.#admitPersistentLease(
        serviceId,
        pool,
        lease,
        signal,
      );
    }
    if (pool.queued >= pool.queueLimit) {
      throw new ServiceUnavailableError(
        `service ${serviceId} has no persistent execution slot`,
      );
    }
    pool.queued++;
    try {
      while (!signal.aborted) {
        lease = acquire();
        if (lease !== undefined) {
          return await this.#admitPersistentLease(
            serviceId,
            pool,
            lease,
            signal,
            true,
          );
        }
        await this.#waitForCapacity(pool, signal);
      }
      throw signal.reason ??
        new DOMException("Request cancelled", "AbortError");
    } finally {
      pool.queued = Math.max(0, pool.queued - 1);
    }
  }

  async #admitPersistentLease(
    serviceId: string,
    pool: ServicePool,
    lease: ServiceWorkerLease,
    signal: AbortSignal,
    alreadyQueued = false,
  ): Promise<ServiceWorkerLease> {
    try {
      await this.#waitForWorkerCapacity(
        serviceId,
        pool,
        lease.worker,
        signal,
        alreadyQueued,
      );
      lease.admitted = true;
      return lease;
    } catch (error) {
      if (lease.created) {
        pool.bindings.delete(lease.executionId!);
        this.#notifyCapacity();
      }
      throw error;
    }
  }

  async #waitForWorkerCapacity(
    serviceId: string,
    pool: ServicePool,
    worker: RuntimeWorker,
    signal: AbortSignal,
    alreadyQueued = false,
  ): Promise<void> {
    const available = (): boolean => {
      if (
        worker.closed || worker.draining ||
        !pool.workers.has(worker.metadata.workerId)
      ) {
        throw new ServiceUnavailableError(
          `target Worker ${worker.metadata.workerId} is unavailable`,
        );
      }
      return this.#workerLoad(pool, worker) <
        routingLimit(pool.concurrencyPerWorker);
    };
    if (available()) {
      this.#admitWorker(pool, worker);
      return;
    }
    if (!alreadyQueued) {
      if (pool.queued >= pool.queueLimit) {
        throw new ServiceUnavailableError(
          `service ${serviceId} request queue is full`,
        );
      }
      pool.queued++;
    }
    try {
      while (!signal.aborted) {
        if (available()) {
          this.#admitWorker(pool, worker);
          return;
        }
        await this.#waitForCapacity(pool, signal);
      }
      throw signal.reason ??
        new DOMException("Request cancelled", "AbortError");
    } finally {
      if (!alreadyQueued) pool.queued = Math.max(0, pool.queued - 1);
    }
  }

  #finishServiceWorkerLease(
    serviceId: string,
    lease: ServiceWorkerLease,
    successful: boolean,
  ): void {
    this.#releaseWorkerAdmission(serviceId, lease);
    if (lease.executionId === undefined) return;
    const pool = this.#servicePools.get(serviceId);
    const binding = pool?.bindings.get(lease.executionId);
    if (binding?.workerId !== lease.worker.metadata.workerId) return;
    if (successful) {
      binding.expiresAt = this.options.now() + lease.keepAliveMilliseconds;
    } else if (lease.created) {
      pool!.bindings.delete(lease.executionId);
      this.#lastReservationRelease.set(
        lease.worker.metadata.workerId,
        this.options.now(),
      );
      this.#notifyCapacity();
    }
  }

  #admitSelectedWorker(
    serviceId: string,
    pool: ServicePool,
  ): RuntimeWorker {
    const worker = this.selectServiceWorker(serviceId);
    this.#admitWorker(pool, worker);
    return worker;
  }

  #admitWorker(pool: ServicePool, worker: RuntimeWorker): void {
    const workerId = worker.metadata.workerId;
    pool.admissions.set(workerId, (pool.admissions.get(workerId) ?? 0) + 1);
  }

  #releaseWorkerAdmission(
    serviceId: string,
    lease: ServiceWorkerLease,
  ): void {
    if (!lease.admitted) return;
    lease.admitted = false;
    const pool = this.#servicePools.get(serviceId);
    if (pool === undefined) return;
    const workerId = lease.worker.metadata.workerId;
    const remaining = (pool.admissions.get(workerId) ?? 1) - 1;
    if (remaining > 0) pool.admissions.set(workerId, remaining);
    else pool.admissions.delete(workerId);
    this.#wakeCapacityWaiters();
  }

  #workerLoad(pool: ServicePool, worker: RuntimeWorker): number {
    return worker.inFlight +
      (pool.admissions.get(worker.metadata.workerId) ?? 0);
  }

  #connectServiceWorkerLease(
    serviceId: string,
    lease: ServiceWorkerLease,
  ): void {
    if (lease.executionId === undefined) return;
    const binding = this.#servicePools.get(serviceId)?.bindings.get(
      lease.executionId,
    );
    if (binding?.workerId === lease.worker.metadata.workerId) {
      binding.connections++;
    }
  }

  #disconnectServiceWorkerLease(
    serviceId: string,
    lease: ServiceWorkerLease,
  ): void {
    if (lease.executionId === undefined) return;
    const binding = this.#servicePools.get(serviceId)?.bindings.get(
      lease.executionId,
    );
    if (binding?.workerId === lease.worker.metadata.workerId) {
      binding.connections = Math.max(0, binding.connections - 1);
      binding.expiresAt = this.options.now() + lease.keepAliveMilliseconds;
    }
  }

  #sweepPersistentBindings(
    pool: {
      bindings: Map<string, PersistentBinding>;
    },
    now = this.options.now(),
    notify = true,
  ): boolean {
    let changed = false;
    for (const [executionId, binding] of pool.bindings) {
      if (binding.connections === 0 && binding.expiresAt <= now) {
        pool.bindings.delete(executionId);
        this.#lastReservationRelease.set(binding.workerId, now);
        changed = true;
      }
    }
    if (changed && notify) this.#notifyCapacity();
    return changed;
  }

  async #waitForCapacity(
    pool: Pick<ServicePool, "bindings">,
    signal: AbortSignal,
  ): Promise<void> {
    if (signal.aborted) return;
    const now = this.options.now();
    let delay: number | undefined;
    for (const binding of pool.bindings.values()) {
      if (binding.connections !== 0) continue;
      const remaining = Math.max(0, binding.expiresAt - now);
      delay = delay === undefined ? remaining : Math.min(delay, remaining);
    }
    await new Promise<void>((resolve) => {
      let timer: ReturnType<typeof setTimeout> | undefined;
      const wake = (): void => {
        this.#capacityWaiters.delete(wake);
        signal.removeEventListener("abort", wake);
        if (timer !== undefined) clearTimeout(timer);
        resolve();
      };
      this.#capacityWaiters.add(wake);
      signal.addEventListener("abort", wake, { once: true });
      if (delay !== undefined) timer = setTimeout(wake, delay);
    });
  }

  #notifyCapacity(): void {
    this.#wakeCapacityWaiters();
    this.#stateChanged();
  }

  #wakeCapacityWaiters(): void {
    const waiters = [...this.#capacityWaiters];
    this.#capacityWaiters.clear();
    for (const wake of waiters) wake();
  }

  #persistentReservationCounts(): Map<string, number> {
    const counts = new Map<string, number>();
    const now = this.options.now();
    let changed = false;
    for (const pool of this.#servicePools.values()) {
      changed = this.#sweepPersistentBindings(pool, now, false) || changed;
      for (const binding of pool.bindings.values()) {
        counts.set(binding.workerId, (counts.get(binding.workerId) ?? 0) + 1);
      }
    }
    if (changed) this.#notifyCapacity();
    return counts;
  }

  async drain(): Promise<void> {
    this.#draining = true;
    this.#stateChanged();
    await Promise.allSettled(
      [...this.#workerStarts.values()].map((start) => start.promise),
    );
    await Promise.allSettled(
      [...this.#workers.keys()].map((id) => this.stopWorker(id)),
    );
  }

  status(
    persistent = this.#persistentReservationCounts(),
  ): Record<string, unknown> {
    const workers = [...this.#workers.values()];
    const liveFailures = workers.filter((worker) =>
      worker.failure !== undefined
    ).slice(-20).map((worker) => ({
      worker_id: worker.metadata.workerId,
      execution_id: worker.metadata.executionId,
      reason: worker.failure,
    }));
    return {
      revision: this.#revision,
      supervisor_started_at_ms: this.options.startedAt,
      protocol_version: PROTOCOL_VERSION,
      supervisor_version: this.options.supervisorVersion,
      deno_version: this.options.denoVersion,
      runtime_group_id: this.options.runtimeGroupId,
      sandbox_id: this.options.sandboxId,
      workload_type: this.options.workloadType,
      worker_count: workers.length,
      ready_worker_count:
        workers.filter((worker) =>
          !worker.starting && !worker.closed && !worker.draining
        ).length,
      failed_worker_count: liveFailures.length,
      active_requests: workers.reduce(
        (total, worker) => total + worker.inFlight,
        0,
      ),
      active_execution_count: workers.reduce(
        (total, worker) => {
          const reserved = persistent.get(worker.metadata.workerId) ?? 0;
          return total + Math.max(reserved, worker.inFlight);
        },
        0,
      ),
      uptime_ms: this.options.now() - this.options.startedAt,
      draining: this.#draining,
      recent_failures: [...this.#recentFailures, ...liveFailures].slice(-20),
    };
  }

  workers(
    includeLogs = true,
    persistent = this.#persistentReservationCounts(),
  ): WorkerStatus[] {
    return [...this.#workers.values()].map<WorkerStatus>((worker) => {
      const reservations = persistent.get(worker.metadata.workerId) ?? 0;
      const inFlight = Math.max(reservations, worker.inFlight);
      const workerIdleSince = worker.idleSinceMilliseconds;
      const idleSince = inFlight === 0 && workerIdleSince !== undefined
        ? Math.max(
          workerIdleSince,
          this.#lastReservationRelease.get(worker.metadata.workerId) ?? 0,
        )
        : undefined;
      return {
        worker_id: worker.metadata.workerId,
        execution_id: worker.metadata.executionId,
        workload_id: worker.metadata.workloadId,
        owner_id: worker.metadata.ownerId,
        debugger_name: worker.metadata.debuggerName,
        entrypoint: worker.metadata.entrypoint,
        release_id: worker.metadata.releaseId,
        in_flight: inFlight,
        persistent_executions: reservations,
        idle_since_ms: idleSince,
        state: worker.failure !== undefined
          ? "failed"
          : worker.closed
          ? "stopped"
          : worker.draining
          ? "stopping"
          : worker.starting
          ? "starting"
          : "ready",
        failure: worker.failure,
        logs: includeLogs ? worker.logs : [],
      };
    }).sort((left, right) => left.worker_id.localeCompare(right.worker_id));
  }

  heartbeat(): Envelope<Record<string, unknown>> {
    return {
      protocol_version: PROTOCOL_VERSION,
      message_type: "heartbeat",
      runtime_group_id: this.options.runtimeGroupId,
      payload: {
        ...this.snapshot(),
        event_loop_timestamp: this.options.now(),
        memory_usage: Deno.memoryUsage(),
      },
    };
  }

  registration(): Envelope<Record<string, unknown>> {
    return { ...this.heartbeat(), message_type: "supervisor_registration" };
  }

  snapshot(): Record<string, unknown> {
    const persistent = this.#persistentReservationCounts();
    return {
      ...this.status(persistent),
      workers: this.workers(false, persistent),
    };
  }

  handler = async (request: Request): Promise<Response> => {
    if (!this.#authorized(request)) {
      return Response.json({ error: "unauthorized" }, { status: 401 });
    }
    const url = new URL(request.url);
    if (request.method === "GET" && url.pathname === "/v1/health") {
      return Response.json({
        healthy: !this.#draining,
        protocol_version: PROTOCOL_VERSION,
      });
    }
    if (request.method === "GET" && url.pathname === "/v1/status") {
      return Response.json(this.status());
    }
    if (request.method === "GET" && url.pathname === "/v1/snapshot") {
      return Response.json(this.snapshot());
    }
    if (request.method === "GET" && url.pathname === "/v1/workers") {
      return Response.json({ workers: this.workers() });
    }
    if (request.method === "POST" && url.pathname === "/v1/workers/start") {
      return await this.#handleControl(
        request,
        "start_worker",
        "worker_state_change",
        async (payload) => {
          const options = payload as unknown as StartWorkerOptions;
          const worker = await this.startWorker(options);
          return {
            worker: this.workers().find((item) =>
              item.worker_id === worker.metadata.workerId
            ),
          };
        },
        201,
      );
    }
    const workerStop = url.pathname.match(/^\/v1\/workers\/([^/]+)\/stop$/);
    if (request.method === "POST" && workerStop !== null) {
      return await this.#handleControl(
        request,
        "stop_worker",
        "worker_state_change",
        async (payload) => {
          await this.stopWorker(
            decodeURIComponent(workerStop[1]!),
            payload.immediate === true,
          );
          return { stopped: true };
        },
      );
    }
    const workerInvoke = url.pathname.match(
      /^\/v1\/workers\/([^/]+)\/invoke$/,
    );
    if (request.method === "POST" && workerInvoke !== null) {
      return await this.#handleControl(
        request,
        "worker_invoke",
        "worker_result",
        async (payload) => {
          if (
            typeof payload.function !== "string" ||
            payload.function.length === 0 || payload.function.length > 128
          ) throw new TypeError("registered Worker function is required");
          if (
            payload.persistent_execution_id !== undefined &&
            (typeof payload.persistent_execution_id !== "string" ||
              payload.persistent_execution_id.length === 0)
          ) throw new TypeError("persistent execution ID must be non-empty");
          return await this.invokeWorker(
            decodeURIComponent(workerInvoke[1]!),
            payload.function,
            payload.input,
            request.signal,
            payload.persistent_execution_id,
          );
        },
      );
    }
    const jobRun = url.pathname.match(/^\/v1\/jobs\/([^/]+)\/run$/);
    if (request.method === "POST" && jobRun !== null) {
      return await this.#handleControl(
        request,
        "job_start",
        "job_result",
        async (payload) => {
          const checkModules = Array.isArray(payload.check_modules) &&
              payload.check_modules.every((module) =>
                typeof module === "string"
              )
            ? payload.check_modules as string[]
            : [];
          if (checkModules.length > 0) {
            await this.#entrypointValidator(checkModules);
          }
          const moduleDependencies = checkModules.length === 0
            ? {}
            : await this.#moduleAnalyzer(checkModules);
          const worker = this.#requireWorker(
            decodeURIComponent(jobRun[1]!),
            "job",
          );
          if (!Array.isArray(payload.arguments)) {
            throw new TypeError("job arguments must be an array");
          }
          if (
            payload.secrets === null || typeof payload.secrets !== "object" ||
            Array.isArray(payload.secrets) ||
            !Object.values(payload.secrets).every((value) =>
              typeof value === "string"
            )
          ) throw new TypeError("job secrets must be a string map");
          const result = await worker.runJob(
            payload.arguments,
            payload.secrets as Record<string, string>,
          );
          return {
            result,
            logs: worker.logs,
            module_dependencies: moduleDependencies,
          };
        },
      );
    }
    const serviceConfigure = url.pathname.match(
      /^\/v1\/services\/([^/]+)\/configure$/,
    );
    if (request.method === "POST" && serviceConfigure !== null) {
      return await this.#handleControl(
        request,
        "service_pool_configuration",
        "service_pool_configuration",
        (payload) => {
          const body = payload as {
            worker_ids?: unknown;
            concurrency_per_worker?: unknown;
            queue_limit?: number;
          };
          if (
            !Array.isArray(body.worker_ids) ||
            !body.worker_ids.every((workerId) => typeof workerId === "string")
          ) {
            throw new TypeError("worker_ids must be an array of strings");
          }
          const serviceId = decodeURIComponent(serviceConfigure[1]!);
          this.configureService(
            serviceId,
            body.worker_ids,
            body.concurrency_per_worker as number,
            body.queue_limit,
          );
          return { configured: true };
        },
      );
    }
    const serviceWebSocket = url.pathname.match(
      /^\/v1\/services\/([^/]+)\/websocket$/,
    );
    if (request.method === "GET" && serviceWebSocket !== null) {
      const serviceId = decodeURIComponent(serviceWebSocket[1]!);
      return await this.#upgradeRequestWebSocket(request, serviceId);
    }
    const serviceDispatch = url.pathname.match(
      /^\/v1\/services\/([^/]+)\/dispatch$/,
    );
    if (request.method === "POST" && serviceDispatch !== null) {
      const serviceId = decodeURIComponent(serviceDispatch[1]!);
      let lease: ServiceWorkerLease | undefined;
      try {
        lease = await this.#acquireServiceWorkerLease(
          serviceId,
          request.headers,
          request.signal,
        );
        const worker = lease.worker;
        const method = request.headers.get("x-80-20-method") ?? "GET";
        const responsePromise = worker.dispatchService(
          new Request(
            request.headers.get("x-80-20-url") ?? "http://service/",
            {
              method,
              headers: forwardedHeaders(request.headers),
              signal: request.signal,
              body: method === "GET" || method === "HEAD"
                ? undefined
                : request.body,
            },
          ),
          trustedServiceMetadata(request.headers, worker.metadata),
        );
        this.#releaseWorkerAdmission(serviceId, lease);
        const response = await responsePromise;
        const headers = new Headers(response.headers);
        headers.set("x-80-20-runtime-worker-id", worker.metadata.workerId);
        headers.set("x-80-20-service-response", "true");
        this.#finishServiceWorkerLease(
          serviceId,
          lease,
          response.status >= 200 && response.status < 400,
        );
        return new Response(response.body, {
          status: response.status,
          statusText: response.statusText,
          headers,
        });
      } catch (error) {
        if (lease !== undefined) {
          this.#finishServiceWorkerLease(serviceId, lease, false);
        }
        if (error instanceof ServiceUnavailableError) {
          return Response.json(
            { error: "service_unavailable", message: error.message },
            {
              status: 503,
              headers: { "x-80-20-service-response": "true" },
            },
          );
        }
        return controlError(error);
      }
    }
    const serviceOpenAPI = url.pathname.match(
      /^\/v1\/services\/([^/]+)\/openapi$/,
    );
    if (request.method === "POST" && serviceOpenAPI !== null) {
      return await this.#handleControl(
        request,
        "service_openapi",
        "service_openapi",
        async () => {
          const worker = this.selectServiceWorker(
            decodeURIComponent(serviceOpenAPI[1]!),
          );
          return { document: await worker.serviceOpenAPI() };
        },
      );
    }
    if (request.method === "POST" && url.pathname === "/v1/drain") {
      return await this.#handleControl(
        request,
        "runtime_drain",
        "runtime_drain",
        async () => {
          await this.drain();
          return { draining: true };
        },
      );
    }
    return Response.json({ error: "not found" }, { status: 404 });
  };

  #authorized(request: Request): boolean {
    return request.headers.get("authorization") ===
      `Bearer ${this.options.token}`;
  }

  #stateChanged(): void {
    this.#revision++;
    this.#onStateChange?.();
  }

  #recordFailure(worker: RuntimeWorker, error: unknown): void {
    const reason = error instanceof Error ? error.message : String(error);
    this.#recentFailures.push({
      worker_id: worker.metadata.workerId,
      execution_id: worker.metadata.executionId,
      reason,
    });
    if (this.#recentFailures.length > 20) this.#recentFailures.shift();
  }

  async #upgradeRequestWebSocket(
    request: Request,
    serviceId: string,
  ): Promise<Response> {
    const pool = this.#servicePools.get(serviceId);
    if (pool === undefined) {
      return Response.json({ error: "service unavailable" }, {
        status: 503,
      });
    }
    let lease: ServiceWorkerLease;
    try {
      lease = await this.#acquireServiceWorkerLease(
        serviceId,
        request.headers,
        request.signal,
      );
    } catch (error) {
      return Response.json({
        error: "service_unavailable",
        message: errorMessage(error),
      }, { status: 503 });
    }
    const worker = lease.worker;
    const requestedProtocols = (request.headers.get("sec-websocket-protocol") ??
      "").split(",").map((item) => item.trim()).filter(Boolean);
    const protocol = requestedProtocols[0] ?? "";
    const socketRef: { current?: WebSocket } = {};
    const pending: Array<string | Uint8Array> = [];
    let pendingBytes = 0;
    let pendingClose: { code: number; reason: string } | undefined;
    let opened;
    try {
      const openedPromise = worker.openServiceWebSocket(
        new Request(
          request.headers.get("x-80-20-url") ?? "http://service/",
          {
            method: "GET",
            headers: forwardedHeaders(request.headers),
            signal: request.signal,
          },
        ),
        trustedServiceMetadata(request.headers, worker.metadata),
        protocol,
        {
          send(data) {
            if (socketRef.current?.readyState === WebSocket.OPEN) {
              sendWebSocket(socketRef.current, data);
              return;
            }
            const bytes = typeof data === "string"
              ? new TextEncoder().encode(data).byteLength
              : data.byteLength;
            if (pendingBytes + bytes > 1_048_576) {
              pendingClose = { code: 1009, reason: "outbound buffer exceeded" };
              return;
            }
            pending.push(data);
            pendingBytes += bytes;
          },
          close(code, reason) {
            pendingClose = { code, reason };
            if (socketRef.current?.readyState === WebSocket.OPEN) {
              socketRef.current.close(code, reason);
            }
          },
        },
      );
      this.#releaseWorkerAdmission(serviceId, lease);
      opened = await openedPromise;
    } catch (error) {
      this.#finishServiceWorkerLease(serviceId, lease, false);
      throw error;
    }
    if (!opened.accepted) {
      this.#finishServiceWorkerLease(serviceId, lease, false);
      return new Response(null, {
        status: opened.status,
        headers: opened.headers,
      });
    }
    this.#finishServiceWorkerLease(serviceId, lease, true);
    this.#connectServiceWorkerLease(serviceId, lease);
    const upgraded = Deno.upgradeWebSocket(request, {
      protocol: protocol || undefined,
    });
    const socket = upgraded.socket;
    socketRef.current = socket;
    socket.binaryType = "arraybuffer";
    let messages = Promise.resolve();
    socket.onopen = () => {
      for (const data of pending) sendWebSocket(socket, data);
      pending.length = 0;
      pendingBytes = 0;
      if (pendingClose !== undefined) {
        socket.close(pendingClose.code, pendingClose.reason);
      }
    };
    socket.onmessage = (event) => {
      messages = messages.then(async () => {
        const data = await websocketData(event.data);
        if (data.byteLength > 1_048_576) {
          opened.connection.close(1009, "message exceeds 1 MiB");
          socket.close(1009, "message exceeds 1 MiB");
          return;
        }
        opened.connection.send(
          typeof event.data === "string" ? event.data : data,
        );
      }).catch(() => {
        opened.connection.close(1003, "invalid WebSocket message");
        socket.close(1003, "invalid WebSocket message");
      });
    };
    socket.onclose = (event) => {
      opened.connection.close(event.code || 1000, event.reason);
      this.#disconnectServiceWorkerLease(serviceId, lease);
    };
    socket.onerror = () => {
      opened.connection.close(1011, "WebSocket transport failed");
    };
    return upgraded.response;
  }

  async #handleControl(
    request: Request,
    requestType: MessageType,
    responseType: MessageType,
    action: (payload: Record<string, unknown>) => unknown | Promise<unknown>,
    status = 200,
  ): Promise<Response> {
    let correlationId: string | undefined;
    try {
      const envelope = await request.json();
      assertEnvelope(envelope);
      correlationId = envelope.correlation_id;
      if (
        envelope.runtime_group_id !== this.options.runtimeGroupId ||
        envelope.message_type !== requestType
      ) throw new TypeError("runtime control envelope does not match request");
      if (correlationId === undefined || correlationId.length === 0) {
        throw new TypeError("runtime control correlation_id is required");
      }
      if (
        typeof envelope.payload !== "object" || envelope.payload === null ||
        Array.isArray(envelope.payload)
      ) throw new TypeError("runtime control payload must be an object");
      const payload = await action(envelope.payload);
      return this.#envelopeResponse(
        responseType,
        correlationId,
        payload,
        status,
      );
    } catch (error) {
      return this.#envelopeResponse(
        "error_response",
        correlationId,
        controlFailure(error),
        400,
      );
    }
  }

  #envelopeResponse(
    messageType: MessageType,
    correlationId: string | undefined,
    payload: unknown,
    status: number,
  ): Response {
    return Response.json(
      {
        protocol_version: PROTOCOL_VERSION,
        message_type: messageType,
        runtime_group_id: this.options.runtimeGroupId,
        correlation_id: correlationId,
        payload,
      } satisfies Envelope<unknown>,
      { status },
    );
  }

  #requireWorker(workerId: string, workloadType: WorkloadType): RuntimeWorker {
    if (this.options.workloadType !== workloadType) {
      throw new Error(`runtime group is not ${workloadType}`);
    }
    const worker = this.#workers.get(workerId);
    if (worker === undefined) throw new Error(`unknown Worker ${workerId}`);
    return worker;
  }
}

function controlFailure(error: unknown): Record<string, unknown> {
  if (error instanceof WorkerExecutionError) {
    return {
      error: error.message,
      ...(error.code === undefined ? {} : { code: error.code }),
      ...(error.details === undefined ? {} : { details: error.details }),
    };
  }
  return { error: error instanceof Error ? error.message : String(error) };
}

function controlError(error: unknown): Response {
  return Response.json(
    { error: error instanceof Error ? error.message : String(error) },
    { status: 400 },
  );
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

async function websocketData(value: unknown): Promise<Uint8Array> {
  if (typeof value === "string") return new TextEncoder().encode(value);
  if (value instanceof Uint8Array) return value;
  if (value instanceof ArrayBuffer) return new Uint8Array(value);
  if (value instanceof Blob) return new Uint8Array(await value.arrayBuffer());
  throw new TypeError("unsupported WebSocket message type");
}

function sendWebSocket(socket: WebSocket, data: string | Uint8Array): void {
  socket.send(typeof data === "string" ? data : new Uint8Array(data));
}

function forwardedHeaders(headers: Headers): Headers {
  const result = new Headers(headers);
  result.delete("authorization");
  result.delete("x-80-20-method");
  result.delete("x-80-20-url");
  result.delete("x-80-20-runtime-worker-id");
  result.delete("x-80-20-service-response");
  for (const name of [...result.keys()]) {
    if (name.toLowerCase().startsWith("x-80-20-internal-")) {
      result.delete(name);
    }
  }
  return result;
}

class ServiceUnavailableError extends Error {}

function trustedServiceMetadata(
  headers: Headers,
  worker: ExecutionMetadata,
): ServiceRequestMetadata {
  const generation = Number(
    headers.get("x-80-20-internal-service-generation") ??
      worker.service?.generation ?? 0,
  );
  return {
    requestId: headers.get("x-80-20-internal-request-id") ||
      crypto.randomUUID(),
    serviceId: headers.get("x-80-20-internal-service-id") ||
      worker.service?.serviceId || worker.workloadId,
    serviceGeneration: Number.isSafeInteger(generation) ? generation : 0,
    canonicalBasePath: headers.get("x-80-20-internal-canonical-base-path") ||
      worker.service?.canonicalBasePath || "/",
    originalUrl: headers.get("x-80-20-internal-original-url") ||
      headers.get("x-80-20-url") || "http://service/",
    client: {
      ipAddress: headers.get("x-80-20-internal-client-ip-address") || "",
      networkScope: clientNetworkScope(headers),
    },
    persistentExecutionId: headers.get(
      "x-80-20-internal-persistent-execution-id",
    ) || undefined,
    persistentKeepAliveMilliseconds: positiveIntegerHeader(
      headers,
      "x-80-20-internal-persistent-keep-alive-ms",
    ),
    execution: {
      nodeId: worker.nodeId,
      runtimeGroupId: worker.runtimeGroupId,
      sandboxId: worker.sandboxId,
      workerId: worker.workerId,
      workerExecutionId: worker.executionId,
      persistentExecutionId: headers.get(
        "x-80-20-internal-persistent-execution-id",
      ) || undefined,
    },
    auth: trustedAuthContext(headers),
    authenticatedUser: headers.get("x-80-20-internal-auth-username") ||
      undefined,
  };
}

function clientNetworkScope(
  headers: Headers,
): ServiceRequestMetadata["client"]["networkScope"] {
  const value = headers.get("x-80-20-internal-client-network-scope");
  return value === "loopback" || value === "private" ||
      value === "link_local" || value === "public" || value === "special"
    ? value
    : "special";
}

function positiveIntegerHeader(
  headers: Headers,
  name: string,
): number | undefined {
  const raw = headers.get(name);
  if (raw === null) return undefined;
  const value = Number(raw);
  return Number.isSafeInteger(value) && value > 0 ? value : undefined;
}

function trustedAuthContext(
  headers: Headers,
): import("../worker/contracts.ts").AuthContext {
  if (headers.get("x-80-20-internal-auth-authenticated") !== "true") {
    return { authenticated: false };
  }
  const authVersion = Number(
    headers.get("x-80-20-internal-auth-version") ?? "0",
  );
  return {
    authenticated: true,
    realm: headers.get("x-80-20-internal-auth-realm") === "user"
      ? "user"
      : undefined,
    userId: headers.get("x-80-20-internal-auth-user-id") || undefined,
    username: headers.get("x-80-20-internal-auth-username") || undefined,
    authVersion: Number.isSafeInteger(authVersion) && authVersion > 0
      ? authVersion
      : undefined,
  };
}

async function validateEntrypoints(entrypoints: string[]): Promise<void> {
  const command = new Deno.Command(Deno.execPath(), {
    args: serviceCheckArguments(
      entrypoints,
      Deno.env.get("DEPENDENCY_MODE") ?? "cached_only",
    ),
    stdout: "piped",
    stderr: "piped",
  });
  const output = await command.output();
  if (!output.success) {
    const detail = new TextDecoder().decode(output.stderr).trim().slice(
      0,
      8192,
    );
    throw new TypeError(`module type check failed: ${detail}`);
  }
}

interface DenoInfoModule {
  specifier?: string;
  dependencies?: Array<{
    code?: { specifier?: string };
    type?: { specifier?: string };
  }>;
}

async function analyzeModules(
  entrypoints: string[],
): Promise<ModuleDependencies> {
  const roots = entrypoints.map((entrypoint) =>
    new URL(entrypoint, "file:///").href
  );
  const aggregator = await Deno.makeTempFile({
    prefix: "the8020-table-graph-",
    suffix: ".ts",
  });
  try {
    await Deno.writeTextFile(
      aggregator,
      roots.map((root) => `import ${JSON.stringify(root)};`).join("\n"),
    );
    const output = await new Deno.Command(Deno.execPath(), {
      args: [
        "info",
        "--json",
        "--config=/opt/runtime/deno.json",
        aggregator,
      ],
      stdout: "piped",
      stderr: "piped",
    }).output();
    if (!output.success) {
      const detail = new TextDecoder().decode(output.stderr).trim().slice(
        0,
        8192,
      );
      throw new TypeError(`module graph failed: ${detail}`);
    }
    if (output.stdout.byteLength > 16 * 1024 * 1024) {
      throw new TypeError("module graph exceeds 16 MiB");
    }
    const graph = JSON.parse(new TextDecoder().decode(output.stdout)) as {
      modules?: DenoInfoModule[];
    };
    if (!Array.isArray(graph.modules)) {
      throw new TypeError("module graph result is invalid");
    }
    const modules = new Map(
      graph.modules.filter((item) => typeof item.specifier === "string").map(
        (item) => [item.specifier!, item],
      ),
    );
    return Object.fromEntries(entrypoints.map((entrypoint, index) => {
      const pending = [roots[index]!];
      const visited = new Set<string>();
      const dependencies = new Set<string>();
      while (pending.length > 0) {
        const specifier = pending.pop()!;
        if (visited.has(specifier)) continue;
        visited.add(specifier);
        if (specifier.startsWith("file:///workspace/packages/")) {
          dependencies.add(decodeURIComponent(new URL(specifier).pathname));
        }
        for (const dependency of modules.get(specifier)?.dependencies ?? []) {
          for (
            const resolved of [
              dependency.code?.specifier,
              dependency.type?.specifier,
            ]
          ) {
            if (typeof resolved === "string" && !visited.has(resolved)) {
              pending.push(resolved);
            }
          }
        }
      }
      return [entrypoint, [...dependencies].sort()];
    }));
  } finally {
    await Deno.remove(aggregator).catch(() => undefined);
  }
}

export function serviceCheckArguments(
  entrypoint: string | string[],
  _dependencyMode: string,
): string[] {
  const args = ["check", "--config=/opt/runtime/deno.json"];
  args.push(...(Array.isArray(entrypoint) ? entrypoint : [entrypoint]));
  return args;
}
