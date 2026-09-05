export type WorkloadType = "service" | "job";

export interface ExecutionUserMetadata {
  readonly userId: string;
  readonly username: string;
}

export interface ExecutionOriginMetadata {
  readonly type: "service" | "job" | "program";
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
    : origin.type === "job" || origin.type === "program";
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
  runtimeGroupId: string;
  sandboxId: string;
  workerId: string;
  executionId: string;
  workloadType: WorkloadType;
  ownerId: string;
  workloadId: string;
  releaseId: string;
  entrypoint: string;
  debuggerName: string;
  validateEntrypoint?: boolean;
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
  requestId: string;
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
  runtimeGroupId: string;
  sandboxId: string;
  workerId: string;
  workerExecutionId: string;
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
  | "execution.completePersistent";

export interface KernelCallRequest {
  operation: KernelOperation;
  arguments: Record<string, unknown>;
  requestId?: string;
  serviceId?: string;
  executionId: string;
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
  readonly requestId: string;
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
