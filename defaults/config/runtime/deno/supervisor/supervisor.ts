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
import { RuntimeWorker } from "../worker/runtime_worker.ts";
import type { WorkerInvocationResult } from "../worker/runtime_worker.ts";

export interface SupervisorOptions {
  runtimeGroupId: string;
  sandboxId: string;
  workloadType: WorkloadType;
  token: string;
  supervisorVersion: string;
  denoVersion?: string;
  startedAt?: number;
  workerStopGraceMilliseconds?: number;
  kernelCall?: KernelCall;
  nodeId?: string;
}

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
  state: "ready" | "draining" | "failed";
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
}

export class Supervisor {
  readonly options:
    & Required<
      Omit<SupervisorOptions, "kernelCall" | "nodeId">
    >
    & {
      kernelCall?: KernelCall;
      nodeId: string;
    };
  #workers = new Map<string, RuntimeWorker>();
  #servicePools = new Map<
    string,
    {
      workers: Set<string>;
      maximumInFlight: number;
      queueLimit: number;
      queued: number;
      executionMode: "stateless" | "persistent";
      bindings: Map<string, PersistentBinding>;
    }
  >();
  #draining = false;

  constructor(options: SupervisorOptions) {
    if (
      options.runtimeGroupId.length === 0 || options.sandboxId.length === 0 ||
      options.token.length < 16
    ) {
      throw new TypeError(
        "runtime-group ID, sandbox ID, and high-entropy token are required",
      );
    }
    this.options = {
      ...options,
      denoVersion: options.denoVersion ?? Deno.version.deno,
      startedAt: options.startedAt ?? Date.now(),
      workerStopGraceMilliseconds: options.workerStopGraceMilliseconds ?? 1_000,
      nodeId: options.nodeId ?? options.runtimeGroupId,
    };
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
    if (this.#workers.has(options.metadata.workerId)) {
      throw new Error(`Worker ${options.metadata.workerId} already exists`);
    }
    if (options.metadata.validateEntrypoint === true) {
      await validateEntrypoint(options.metadata.entrypoint);
    }
    const metadata = {
      ...options.metadata,
      nodeId: this.options.nodeId,
      runtimeGroupId: this.options.runtimeGroupId,
      sandboxId: this.options.sandboxId,
    };
    const worker = new RuntimeWorker({
      ...options,
      metadata,
      kernelCall: this.options.kernelCall === undefined
        ? undefined
        : async (call) => {
          const result = await this.options.kernelCall!(call);
          if (
            call.operation === "execution.completePersistent" &&
            call.persistentExecutionId !== undefined
          ) {
            this.completePersistentExecution(
              call.serviceId,
              call.persistentExecutionId,
              call.workerId,
            );
          }
          return result;
        },
    });
    this.#workers.set(options.metadata.workerId, worker);
    try {
      await worker.ready;
      return worker;
    } catch (error) {
      this.#workers.delete(options.metadata.workerId);
      worker.kill();
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
  }

  async invokeWorker(
    workerId: string,
    functionName: string,
    input: unknown,
    signal: AbortSignal,
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
    return await worker.invoke(functionName, input, signal);
  }

  async stopWorker(workerId: string, immediate = false): Promise<void> {
    const worker = this.#workers.get(workerId);
    if (worker === undefined) throw new Error(`unknown Worker ${workerId}`);
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
    for (const pool of this.#servicePools.values()) {
      pool.workers.delete(workerId);
    }
  }

  configureService(
    serviceId: string,
    workerIds: string[],
    maximumInFlight = Number.MAX_SAFE_INTEGER,
    queueLimit = Math.max(
      1,
      Math.min(1_024, workerIds.length * Math.min(maximumInFlight, 1_024)),
    ),
  ): void {
    if (!Number.isSafeInteger(maximumInFlight) || maximumInFlight < 1) {
      throw new TypeError("maximum in-flight requests must be positive");
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
    const nextPool = {
      workers: pool,
      maximumInFlight,
      queueLimit,
      queued: this.#servicePools.get(serviceId)?.queued ?? 0,
      executionMode,
      bindings,
    };
    this.#sweepPersistentBindings(nextPool);
    for (const binding of bindings.values()) {
      const worker = this.#workers.get(binding.workerId);
      if (worker !== undefined && !worker.closed) pool.add(binding.workerId);
    }
    this.#servicePools.set(serviceId, nextPool);
  }

  selectServiceWorker(serviceId: string): RuntimeWorker {
    const pool = this.#servicePools.get(serviceId);
    if (pool === undefined || pool.workers.size === 0) {
      throw new Error(`service ${serviceId} has no ready Workers`);
    }
    const workers = [...pool.workers].map((id) => this.#workers.get(id)!)
      .filter((worker) =>
        !worker.closed && !worker.draining &&
        worker.inFlight < pool.maximumInFlight
      )
      .sort((left, right) =>
        left.inFlight - right.inFlight ||
        left.metadata.workerId.localeCompare(right.metadata.workerId)
      );
    if (workers.length === 0) {
      throw new Error(
        `service ${serviceId} has no Worker below its in-flight limit`,
      );
    }
    return workers[0]!;
  }

  async acquireServiceWorker(
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
      return this.selectServiceWorker(serviceId);
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
          return this.selectServiceWorker(serviceId);
        } catch {
          await new Promise((resolve) => setTimeout(resolve, 1));
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
          if (reservations >= pool.maximumInFlight) {
            throw new ServiceUnavailableError(
              `target Worker ${targetWorkerID} has no persistent execution slot`,
            );
          }
          pool.bindings.set(executionId, {
            workerId: targetWorkerID,
            expiresAt: Date.now() + keepAliveMilliseconds,
            connections: 0,
          });
        }
        return {
          worker: target,
          executionId,
          keepAliveMilliseconds,
          created: existing === undefined,
        };
      }
      return {
        worker: target,
        keepAliveMilliseconds: 0,
        created: false,
      };
    }
    if (pool.executionMode === "stateless") {
      return {
        worker: await this.acquireServiceWorker(serviceId, signal),
        keepAliveMilliseconds: 0,
        created: false,
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
          (reserved.get(worker.metadata.workerId) ?? 0) < pool.maximumInFlight
        ).sort((left, right) =>
          (reserved.get(left.metadata.workerId) ?? 0) -
            (reserved.get(right.metadata.workerId) ?? 0) ||
          left.metadata.workerId.localeCompare(right.metadata.workerId)
        );
      const worker = candidates[0];
      if (worker === undefined) return undefined;
      pool.bindings.set(executionId, {
        workerId: worker.metadata.workerId,
        expiresAt: Date.now() + keepAliveMilliseconds,
        connections: 0,
      });
      return { worker, executionId, keepAliveMilliseconds, created: true };
    };

    let lease = acquire();
    if (lease !== undefined) return lease;
    if (pool.queued >= pool.queueLimit) {
      throw new ServiceUnavailableError(
        `service ${serviceId} has no persistent execution slot`,
      );
    }
    pool.queued++;
    try {
      while (!signal.aborted) {
        lease = acquire();
        if (lease !== undefined) return lease;
        await new Promise((resolve) => setTimeout(resolve, 1));
      }
      throw signal.reason ??
        new DOMException("Request cancelled", "AbortError");
    } finally {
      pool.queued = Math.max(0, pool.queued - 1);
    }
  }

  #finishServiceWorkerLease(
    serviceId: string,
    lease: ServiceWorkerLease,
    successful: boolean,
  ): void {
    if (lease.executionId === undefined) return;
    const pool = this.#servicePools.get(serviceId);
    const binding = pool?.bindings.get(lease.executionId);
    if (binding?.workerId !== lease.worker.metadata.workerId) return;
    if (successful) {
      binding.expiresAt = Date.now() + lease.keepAliveMilliseconds;
    } else if (lease.created) {
      pool!.bindings.delete(lease.executionId);
    }
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
      binding.expiresAt = Date.now() + lease.keepAliveMilliseconds;
    }
  }

  #sweepPersistentBindings(
    pool: {
      bindings: Map<string, PersistentBinding>;
    },
    now = Date.now(),
  ): void {
    for (const [executionId, binding] of pool.bindings) {
      if (binding.connections === 0 && binding.expiresAt <= now) {
        pool.bindings.delete(executionId);
      }
    }
  }

  #persistentReservations(workerId: string): number {
    let total = 0;
    for (const pool of this.#servicePools.values()) {
      this.#sweepPersistentBindings(pool);
      for (const binding of pool.bindings.values()) {
        if (binding.workerId === workerId) total++;
      }
    }
    return total;
  }

  async drain(): Promise<void> {
    this.#draining = true;
    await Promise.allSettled(
      [...this.#workers.keys()].map((id) => this.stopWorker(id)),
    );
  }

  status(): Record<string, unknown> {
    const workers = [...this.#workers.values()];
    const recentFailures = workers.filter((worker) =>
      worker.failure !== undefined
    ).slice(-20).map((worker) => ({
      worker_id: worker.metadata.workerId,
      execution_id: worker.metadata.executionId,
      reason: worker.failure,
    }));
    return {
      protocol_version: PROTOCOL_VERSION,
      supervisor_version: this.options.supervisorVersion,
      deno_version: this.options.denoVersion,
      runtime_group_id: this.options.runtimeGroupId,
      sandbox_id: this.options.sandboxId,
      workload_type: this.options.workloadType,
      worker_count: workers.length,
      ready_worker_count:
        workers.filter((worker) => !worker.closed && !worker.draining).length,
      failed_worker_count: recentFailures.length,
      active_requests: workers.reduce(
        (total, worker) => total + worker.inFlight,
        0,
      ),
      active_execution_count: workers.reduce(
        (total, worker) => {
          const reserved = this.#persistentReservations(
            worker.metadata.workerId,
          );
          return total + (reserved > 0 ? reserved : worker.inFlight);
        },
        0,
      ),
      uptime_ms: Date.now() - this.options.startedAt,
      draining: this.#draining,
      recent_failures: recentFailures,
    };
  }

  workers(): WorkerStatus[] {
    return [...this.#workers.values()].map<WorkerStatus>((worker) => ({
      worker_id: worker.metadata.workerId,
      execution_id: worker.metadata.executionId,
      workload_id: worker.metadata.workloadId,
      owner_id: worker.metadata.ownerId,
      debugger_name: worker.metadata.debuggerName,
      entrypoint: worker.metadata.entrypoint,
      release_id: worker.metadata.releaseId,
      in_flight: this.#persistentReservations(worker.metadata.workerId) ||
        worker.inFlight,
      state: worker.failure !== undefined
        ? "failed"
        : worker.draining
        ? "draining"
        : "ready",
      failure: worker.failure,
      logs: worker.logs,
    })).sort((left, right) => left.worker_id.localeCompare(right.worker_id));
  }

  heartbeat(): Envelope<Record<string, unknown>> {
    return {
      protocol_version: PROTOCOL_VERSION,
      message_type: "heartbeat",
      runtime_group_id: this.options.runtimeGroupId,
      payload: {
        ...this.status(),
        event_loop_timestamp: Date.now(),
        memory_usage: Deno.memoryUsage(),
      },
    };
  }

  registration(): Envelope<Record<string, unknown>> {
    return { ...this.heartbeat(), message_type: "supervisor_registration" };
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
          return await this.invokeWorker(
            decodeURIComponent(workerInvoke[1]!),
            payload.function,
            payload.input,
            request.signal,
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
          const worker = this.#requireWorker(
            decodeURIComponent(jobRun[1]!),
            "job",
          );
          const result = await worker.runJob(payload.input);
          return { result, logs: worker.logs };
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
            worker_ids?: string[];
            maximum_in_flight?: number;
            queue_limit?: number;
          };
          const serviceId = decodeURIComponent(serviceConfigure[1]!);
          this.configureService(
            serviceId,
            body.worker_ids ?? [],
            body.maximum_in_flight ?? Number.MAX_SAFE_INTEGER,
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
        const response = await worker.dispatchService(
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
      opened = await worker.openServiceWebSocket(
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
        { error: error instanceof Error ? error.message : String(error) },
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
    realm: headers.get("x-80-20-internal-auth-realm") === "bootstrap-admin"
      ? "bootstrap-admin"
      : undefined,
    userId: headers.get("x-80-20-internal-auth-user-id") || undefined,
    username: headers.get("x-80-20-internal-auth-username") || undefined,
    authVersion: Number.isSafeInteger(authVersion) && authVersion > 0
      ? authVersion
      : undefined,
  };
}

async function validateEntrypoint(entrypoint: string): Promise<void> {
  const command = new Deno.Command(Deno.execPath(), {
    args: serviceCheckArguments(
      entrypoint,
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
    throw new TypeError(`service type check failed: ${detail}`);
  }
}

export function serviceCheckArguments(
  entrypoint: string,
  dependencyMode: string,
): string[] {
  const args = ["run", "--check", "--config=/opt/runtime/deno.json"];
  if (dependencyMode === "cached_only") args.push("--cached-only");
  args.push(entrypoint);
  return args;
}
