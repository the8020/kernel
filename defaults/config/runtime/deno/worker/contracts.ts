import type { ExecutionContextType } from "../context/types.ts";
import { isId } from "../identity/mod.ts";

export type WorkloadType = "service" | "job";

export interface ExecutionUserMetadata {
  readonly userId: string;
  readonly username: string;
}

export interface ExecutionOriginMetadata {
  readonly type: ExecutionContextType;
  readonly id: string;
}

export function canonicalExecutionUser(
  value: unknown,
): ExecutionUserMetadata {
  if (value === null || typeof value !== "object") {
    throw new TypeError("execution user must be an object");
  }
  const user = value as Record<string, unknown>;
  if (
    typeof user.userId !== "string" ||
    typeof user.username !== "string" ||
    !/^[a-z0-9]{3,32}$/.test(user.username) ||
    user.userId !== `user:${user.username}`
  ) throw new TypeError("execution user is invalid");
  return Object.freeze({ userId: user.userId, username: user.username });
}

export function canonicalExecutionOrigin(
  value: unknown,
  workloadType: WorkloadType,
): ExecutionOriginMetadata {
  if (value === null || typeof value !== "object") {
    throw new TypeError("execution origin must be an object");
  }
  const origin = value as Record<string, unknown>;
  const validType = workloadType === "service"
    ? origin.type === "service"
    : origin.type === "module" || origin.type === "program";
  if (!validType || typeof origin.id !== "string" || origin.id.length === 0) {
    throw new TypeError("execution origin is invalid");
  }
  return Object.freeze({
    type: origin.type,
    id: origin.id,
  }) as ExecutionOriginMetadata;
}

export interface ExecutionMetadata {
  nodeId: string;

  sandboxId: string;
  workerId: string;
  workloadType: WorkloadType;
  ownerId: string;
  workloadId: string;
  releaseId: string;
  entrypoint: string;
  debuggerName: string;
  databaseBackend: "sqlite" | "postgresql";
  databaseAccess?: "full" | "none";
  user: ExecutionUserMetadata;
  origin: ExecutionOriginMetadata;
  service?: ServiceExecutionMetadata;
}

export interface ServiceExecutionMetadata {
  serviceId: string;
  generation: number;
  canonicalBasePath: string;
  executionMode?: "stateless" | "persistent";
  openapi?: {
    title?: string;
    version?: string;
    description?: string;
  };
}

export interface ServiceRequestMetadata {
  contextId: string;
  parentContextId?: string;
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
  authentication?: {
    approved?: boolean;
    module: string;
    claims: Record<string, unknown>;
    unauthenticated: {
      action: string;
      status: number;
      message?: string;
      redirect_url?: string;
    };
  };
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

export type KernelOperation =
  | "admin.execute"
  | "runtime.operation"
  | "database.info"
  | "database.execute"
  | "database.scope.close"
  | "database.transaction.begin"
  | "database.transaction.commit"
  | "database.transaction.rollback"
  | "worker.invoke"
  | "execution.releaseWorker"
  | "execution.retainPersistent"
  | "execution.completePersistent";

export interface KernelCallRequest {
  operation: KernelOperation;
  arguments: Record<string, unknown>;
  contextId?: string;
  parentContextId?: string;
  jobRunId?: string;
  serviceId?: string;
  workerId: string;
  persistentExecutionId?: string;
  user?: ExecutionUserMetadata;
}

export type KernelCall = (
  request: KernelCallRequest,
  signal?: AbortSignal,
) => Promise<unknown>;

export interface WorkerPermissionSet {
  read?: string[];
  write?: string[];
  net?: true | string[];
  import?: true | string[];
  env?: string[];
  sys?: string[];
}

export interface RuntimeLogEvent {
  level: "debug" | "info" | "warn" | "error";
  message: string;
  fields?: Record<string, unknown>;
}

export interface WorkerExecutionFailure {
  message: string;
  code?: string;
  details?: Record<string, unknown>;
}

export interface BaseContext {
  readonly metadata: ExecutionMetadata;
  readonly signal: AbortSignal;
  log(event: RuntimeLogEvent): void;
}

export interface ServiceContext extends BaseContext {
  readonly contextId: string;
  readonly meta: ServiceRequestMetadata;
}

export type ServiceEntrypoint = (
  request: Request,
  context: ServiceContext,
) => Promise<Response>;
export type JobEntrypoint = (
  ...arguments_: unknown[]
) => unknown | Promise<unknown>;

export type WorkerControlContext = BaseContext;

export type WorkerControlFunction = (
  input: unknown,
  context: WorkerControlContext,
) => unknown | Promise<unknown>;

export type WorkerControlFunctions = Readonly<
  Record<string, WorkerControlFunction>
>;

// One invocation, independent of the Worker that executes it.
export interface InvocationMetadata {
  readonly contextId: string;
  readonly parentContextId?: string;
  readonly jobRunId?: string;
}

export function canonicalInvocation(value: unknown): InvocationMetadata {
  if (value === null || typeof value !== "object") {
    throw new TypeError("invocation identity is required");
  }
  const input = value as Record<string, unknown>;
  if (
    !isId(input.contextId, "ctx") ||
    (input.parentContextId !== undefined &&
      !isId(input.parentContextId, "ctx")) ||
    (input.jobRunId !== undefined && !isId(input.jobRunId, "job"))
  ) {
    throw new TypeError("invalid invocation identity");
  }
  return Object.freeze({
    contextId: input.contextId,
    parentContextId: input.parentContextId,
    jobRunId: input.jobRunId,
  }) as InvocationMetadata;
}
