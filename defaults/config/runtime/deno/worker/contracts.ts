export type WorkloadType = "service" | "job";

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

export type KernelOperation =
  | "auth.bootstrapLogin"
  | "auth.logoutCurrent"
  | "admin.execute"
  | "database.query"
  | "database.execute"
  | "worker.invoke"
  | "execution.completePersistent";

export interface KernelCallRequest {
  operation: KernelOperation;
  arguments: Record<string, unknown>;
  requestId: string;
  serviceId: string;
  executionId: string;
  workerId: string;
  persistentExecutionId?: string;
}

export type KernelCall = (request: KernelCallRequest) => Promise<unknown>;

export interface WorkerPermissionSet {
  read?: string[];
  write?: string[];
  net?: string[];
  import?: string[];
  env?: string[];
  sys?: string[];
}

export interface RuntimeLogEvent {
  level: "debug" | "info" | "warn" | "error";
  message: string;
  fields?: Record<string, unknown>;
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

export interface JobContext extends BaseContext {
  readonly executionCount: number;
}

export type ServiceEntrypoint = (
  request: Request,
  context: ServiceContext,
) => Promise<Response>;
export type JobEntrypoint = (
  input: unknown,
  context: JobContext,
) => Promise<unknown>;

export type WorkerControlContext = BaseContext;

export type WorkerControlFunction = (
  input: unknown,
  context: WorkerControlContext,
) => unknown | Promise<unknown>;

export type WorkerControlFunctions = Readonly<
  Record<string, WorkerControlFunction>
>;
