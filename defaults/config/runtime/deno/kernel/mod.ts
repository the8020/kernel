export interface BootstrapLoginInput {
  username: string;
  password: string;
}

export interface BootstrapUser {
  id: string;
  username: string;
  realm: "bootstrap-admin";
}

export interface BootstrapLoginResult {
  authenticated: boolean;
  user?: BootstrapUser;
  setCookie?: string;
  error?: "invalid_credentials" | "disabled" | "internal_error";
}

export interface LogoutResult {
  setCookie: string;
}

export interface SecretSummary {
  name: string;
  updated_at: string;
}

export interface Secret extends SecretSummary {
  value: string;
}

export interface SetSecretInput {
  name: string;
  value: string;
}

export interface PackageIndex {
  schema: number;
  author: string;
  repository: string;
  source?: string;
  commit?: string;
  tag?: string;
  secret?: string;
  local: boolean;
  package_id: string;
  path: string;
  valid: boolean;
  validation_error?: string;
}

export interface PackageSourceReference {
  kind: "branch" | "tag";
  name: string;
  commit: string;
}

export interface PackageSourceInspection {
  source: string;
  author: string;
  repository: string;
  package_id: string;
  default_branch?: string;
  references: PackageSourceReference[];
}

export interface PackageVersion {
  commit: string;
  short_commit: string;
  authored_at: string;
  author: string;
  subject: string;
  tags: string[];
  current: boolean;
  selected: boolean;
}

export interface PackageVersions {
  package_id: string;
  source?: string;
  current_commit?: string;
  selected_commit?: string;
  versions: PackageVersion[];
}

export interface PackageSynchronization {
  package_id: string;
  commit: string;
  success: boolean;
}

export interface SetPackageIndexInput {
  author: string;
  repository: string;
  source?: string;
  commit?: string;
  tag?: string;
  secret?: string;
  local?: boolean;
}

export interface PackageRepositoryBranch {
  name: string;
  commit: string;
  current: boolean;
  remote: boolean;
}

export interface PackageRepositoryCommit {
  commit: string;
  short_commit: string;
  authored_at: string;
  author: string;
  subject: string;
  current: boolean;
}

export interface PackageRepository {
  package_id: string;
  path: string;
  activation_ready: boolean;
  branch?: string;
  head?: string;
  remote_name?: string;
  remote_url?: string;
  clean: boolean;
  status: string;
  branches: PackageRepositoryBranch[];
  commits: PackageRepositoryCommit[];
}

export interface CheckoutPackageRepositoryInput {
  packageId: string;
  branch?: string;
  commit?: string;
}

export interface CreateLocalPackageInput {
  author: string;
  repository: string;
  description?: string;
}

export interface LocalPackageResult {
  index: PackageIndex;
  package: Record<string, unknown>;
  commit: string;
  repository_path: string;
}

export interface AdminCommandErrorValue {
  code: string;
  message: string;
  details?: Record<string, unknown>;
}

interface AdminCommandResponse<Result> {
  protocol_version: number;
  success: boolean;
  request_id?: string;
  result?: Result;
  error?: AdminCommandErrorValue;
}

export class AdminCommandError extends Error {
  readonly code: string;
  readonly details?: Record<string, unknown>;
  readonly requestId?: string;

  constructor(error: AdminCommandErrorValue, requestId?: string) {
    super(error.message);
    this.name = "AdminCommandError";
    this.code = error.code;
    this.details = error.details;
    this.requestId = requestId;
  }
}

export type TaggedDatabaseValue =
  | { type: "bigint"; value: string }
  | { type: "decimal"; value: string; precision: number; scale: number }
  | { type: "datetime"; value: string }
  | { type: "bytes"; value: string }
  | { type: "json"; value: unknown };

export type DatabaseValue =
  | null
  | boolean
  | number
  | string
  | TaggedDatabaseValue;

export interface DatabaseInfo {
  backend: "sqlite" | "postgresql";
  location: string;
  state:
    | "UNAVAILABLE"
    | "CONNECTED"
    | "INITIALIZING"
    | "READY"
    | "DEGRADED";
  initialized: boolean;
  catalog_version: number;
}

export interface DatabaseExecuteResult {
  columns: string[];
  rows: DatabaseValue[][];
  affected_rows?: DatabaseValue;
  insert_id?: DatabaseValue;
}

export interface DatabaseTableSummary {
  table_id: string;
  source_package: string;
  source_commit: string;
  source_module: string;
  state: string;
  synchronization_state: string;
  descriptor_hash: string;
  synchronized_at?: string;
  active_columns: number;
  retired_columns: number;
  error?: string;
  definition_state?: string;
  current_source_commit?: string;
}

export interface DatabaseDefinitionSummary {
  table_id: string;
  source_package: string;
  source_commit: string;
  source_module: string;
  descriptor_hash: string;
  catalog_state: string;
  catalog_hash?: string;
  synchronization_state: string;
  error?: string;
}

export interface DatabaseSynchronizationResult {
  table_id: string;
  state: string;
  error?: string;
}

export type KernelOperation =
  | "auth.currentUser"
  | "auth.bootstrapLogin"
  | "auth.logoutCurrent"
  | "admin.execute"
  | "database.info"
  | "database.execute"
  | "database.scope.close"
  | "database.transaction.begin"
  | "database.transaction.commit"
  | "database.transaction.rollback"
  | "worker.invoke"
  | "execution.completePersistent";

export type KernelInvoke = (
  operation: KernelOperation,
  input: Record<string, unknown>,
) => Promise<unknown>;

export const kernelInvokeSymbol = Symbol.for("the8020.kernel.invoke");
export const kernelDatabaseBackendSymbol = Symbol.for(
  "the8020.kernel.databaseBackend",
);

export type DatabaseBackend = "sqlite" | "postgresql";

export function kernelDatabaseBackend(): DatabaseBackend {
  const backend = (globalThis as unknown as Record<symbol, unknown>)[
    kernelDatabaseBackendSymbol
  ];
  if (backend !== "sqlite" && backend !== "postgresql") {
    throw new Error("kernel database backend metadata is unavailable");
  }
  return backend;
}

function invoke<Result>(
  operation: KernelOperation,
  input: Record<string, unknown>,
): Promise<Result> {
  const bridge = (globalThis as unknown as Record<symbol, unknown>)[
    kernelInvokeSymbol
  ];
  if (typeof bridge !== "function") {
    return Promise.reject(new Error("kernel API is unavailable"));
  }
  return (bridge as KernelInvoke)(operation, input) as Promise<Result>;
}

export interface WorkerInvokeInput {
  nodeId: string;
  sandboxId: string;
  workerId: string;
  persistentExecutionId?: string;
  function: string;
  input: unknown;
}

export interface WorkerInvokeErrorValue {
  code:
    | "target_not_found"
    | "target_mismatch"
    | "function_not_found"
    | "timeout"
    | "application_error"
    | "invalid_request"
    | "unavailable";
  message: string;
}

interface WorkerInvokeResponse<Result> {
  ok: boolean;
  output?: Result;
  error?: WorkerInvokeErrorValue;
}

export class WorkerInvokeError extends Error {
  readonly code: WorkerInvokeErrorValue["code"];

  constructor(error: WorkerInvokeErrorValue) {
    super(error.message);
    this.name = "WorkerInvokeError";
    this.code = error.code;
  }
}

async function executeAdminCommand<Result extends Record<string, unknown>>(
  commandId: string,
  arguments_: Record<string, unknown> = {},
): Promise<Result> {
  if (typeof commandId !== "string" || commandId.length === 0) {
    throw new TypeError("command ID is required");
  }
  if (
    arguments_ === null || typeof arguments_ !== "object" ||
    Array.isArray(arguments_)
  ) {
    throw new TypeError("command arguments must be an object");
  }
  const response = await invoke<AdminCommandResponse<Result>>(
    "admin.execute",
    { command_id: commandId, arguments: arguments_ },
  );
  if (
    response === null || typeof response !== "object" ||
    typeof response.success !== "boolean"
  ) {
    throw new Error("invalid kernel admin response");
  }
  if (!response.success) {
    if (
      response.error === undefined ||
      typeof response.error.code !== "string" ||
      typeof response.error.message !== "string"
    ) throw new Error("kernel admin command failed");
    throw new AdminCommandError(response.error, response.request_id);
  }
  if (
    response.result === undefined || response.result === null ||
    typeof response.result !== "object" || Array.isArray(response.result)
  ) throw new Error("kernel admin command returned no result");
  return response.result;
}

function optionalArguments(
  values: Record<string, unknown>,
): Record<string, unknown> {
  return Object.fromEntries(
    Object.entries(values).filter(([, value]) => value !== undefined),
  );
}

function databaseArguments(
  statement: string,
  parameters: DatabaseValue[],
  options: { returnRows?: boolean; transaction?: string } = {},
): Record<string, unknown> {
  if (typeof statement !== "string" || statement.trim().length === 0) {
    throw new TypeError("SQL statement is required");
  }
  if (new TextEncoder().encode(statement).byteLength > 1_048_576) {
    throw new TypeError("SQL statement exceeds 1 MiB");
  }
  if (
    !Array.isArray(parameters) ||
    parameters.some((value) => !validDatabaseValue(value))
  ) {
    throw new TypeError("SQL parameters must be an array");
  }
  return {
    statement,
    parameters,
    ...(options.returnRows === undefined
      ? {}
      : { return_rows: options.returnRows }),
    ...(options.transaction === undefined
      ? {}
      : { transaction: options.transaction }),
  };
}

function validDatabaseValue(value: unknown): value is DatabaseValue {
  if (
    value === null || typeof value === "boolean" || typeof value === "string"
  ) return true;
  if (typeof value === "number") return Number.isFinite(value);
  if (typeof value !== "object" || Array.isArray(value)) return false;
  const type = (value as { type?: unknown }).type;
  return type === "bigint" || type === "decimal" || type === "datetime" ||
    type === "bytes" || type === "json";
}

export const kernel = Object.freeze({
  auth: Object.freeze({
    currentUser(): Promise<BootstrapUser | undefined> {
      return invoke<BootstrapUser | undefined>("auth.currentUser", {});
    },
    bootstrapLogin(input: BootstrapLoginInput): Promise<BootstrapLoginResult> {
      if (
        input === null || typeof input !== "object" ||
        typeof input.username !== "string" || typeof input.password !== "string"
      ) {
        return Promise.reject(
          new TypeError("username and password are required"),
        );
      }
      return invoke<BootstrapLoginResult>("auth.bootstrapLogin", {
        username: input.username,
        password: input.password,
      });
    },
    logoutCurrent(): Promise<LogoutResult> {
      return invoke<LogoutResult>("auth.logoutCurrent", {});
    },
  }),
  worker: Object.freeze({
    async invoke<Result = unknown>(input: WorkerInvokeInput): Promise<Result> {
      assertWorkerInvokeInput(input);
      const encoded = JSON.stringify(input.input);
      if (new TextEncoder().encode(encoded).byteLength > 1_048_576) {
        throw new TypeError("Worker invocation input exceeds 1 MiB");
      }
      const response = await invoke<WorkerInvokeResponse<Result>>(
        "worker.invoke",
        input as unknown as Record<string, unknown>,
      );
      if (response.ok) return response.output as Result;
      if (response.error === undefined) {
        throw new Error("Worker invocation returned an invalid result");
      }
      throw new WorkerInvokeError(response.error);
    },
  }),
  execution: Object.freeze({
    completePersistent(): Promise<void> {
      return invoke<void>("execution.completePersistent", {});
    },
  }),
  database: Object.freeze({
    info(): Promise<DatabaseInfo> {
      return invoke<DatabaseInfo>("database.info", {});
    },
    execute(
      statement: string,
      parameters: DatabaseValue[] = [],
      options: { returnRows?: boolean; transaction?: string } = {},
    ): Promise<DatabaseExecuteResult> {
      return invoke<DatabaseExecuteResult>(
        "database.execute",
        databaseArguments(statement, parameters, options),
      );
    },
    transaction: Object.freeze({
      begin(
        settings: { isolationLevel?: string; readOnly?: boolean } = {},
      ): Promise<{ transaction: string }> {
        return invoke("database.transaction.begin", { settings });
      },
      commit(transaction: string): Promise<void> {
        return invoke("database.transaction.commit", { transaction });
      },
      rollback(transaction: string): Promise<void> {
        return invoke("database.transaction.rollback", { transaction });
      },
    }),
    tables: Object.freeze({
      async list(): Promise<DatabaseTableSummary[]> {
        const result = await executeAdminCommand<
          { tables: DatabaseTableSummary[] }
        >("database.table.list");
        return result.tables;
      },
      async definitions(): Promise<DatabaseDefinitionSummary[]> {
        const result = await executeAdminCommand<
          { definitions: DatabaseDefinitionSummary[] }
        >("database.table.definitions");
        return result.definitions;
      },
      async inspect(tableId: string): Promise<Record<string, unknown>> {
        const result = await executeAdminCommand<
          { table: Record<string, unknown> }
        >("database.table.inspect", { table_id: tableId });
        return result.table;
      },
      async synchronize(
        tableId: string,
        sourcePackage?: string,
      ): Promise<DatabaseSynchronizationResult> {
        const result = await executeAdminCommand<
          { table: DatabaseSynchronizationResult }
        >(
          "database.table.sync",
          optionalArguments({
            table_id: tableId,
            source_package: sourcePackage,
          }),
        );
        return result.table;
      },
      async synchronizeAll(): Promise<DatabaseSynchronizationResult[]> {
        const result = await executeAdminCommand<
          { tables: DatabaseSynchronizationResult[] }
        >("database.table.sync_all");
        return result.tables;
      },
      async trim(input: {
        tableId: string;
        columns?: string[];
        dropTable?: boolean;
        confirm: true;
      }): Promise<void> {
        await executeAdminCommand(
          "database.table.trim",
          optionalArguments({
            table_id: input.tableId,
            columns: input.columns?.join(","),
            drop_table: input.dropTable,
            confirm: input.confirm,
          }),
        );
      },
    }),
  }),
  secrets: Object.freeze({
    async list(): Promise<SecretSummary[]> {
      const result = await executeAdminCommand<{ secrets: SecretSummary[] }>(
        "secret.list",
      );
      return result.secrets;
    },
    async get(name: string): Promise<Secret> {
      const result = await executeAdminCommand<{ secret: Secret }>(
        "secret.get",
        { name },
      );
      return result.secret;
    },
    async set(input: SetSecretInput): Promise<SecretSummary> {
      const result = await executeAdminCommand<{ secret: SecretSummary }>(
        "secret.set",
        { name: input.name, value: input.value },
      );
      return result.secret;
    },
  }),
  packages: Object.freeze({
    index: Object.freeze({
      async list(): Promise<PackageIndex[]> {
        const result = await executeAdminCommand<{ packages: PackageIndex[] }>(
          "package.index.list",
        );
        return result.packages;
      },
      async inspect(packageId: string): Promise<PackageIndex> {
        const result = await executeAdminCommand<{ package: PackageIndex }>(
          "package.index.inspect",
          { package_id: packageId },
        );
        return result.package;
      },
      async set(input: SetPackageIndexInput): Promise<PackageIndex> {
        const result = await executeAdminCommand<{ package: PackageIndex }>(
          "package.index.set",
          optionalArguments(input as unknown as Record<string, unknown>),
        );
        return result.package;
      },
    }),
    source: Object.freeze({
      async inspect(source: string): Promise<PackageSourceInspection> {
        const result = await executeAdminCommand<
          { source: PackageSourceInspection }
        >("package.source.inspect", { source });
        return result.source;
      },
    }),
    versions: Object.freeze({
      async list(packageId: string, limit?: number): Promise<PackageVersions> {
        const result = await executeAdminCommand<
          { package: PackageVersions }
        >(
          "package.version.list",
          optionalArguments({ package_id: packageId, limit }),
        );
        return result.package;
      },
    }),
    async synchronize(
      packageIds: string[] = [],
    ): Promise<PackageSynchronization[]> {
      const result = await executeAdminCommand<
        { packages: PackageSynchronization[] }
      >(
        "package.synchronize",
        packageIds.length === 0 ? {} : { packages: packageIds.join(",") },
      );
      return result.packages;
    },
    local: Object.freeze({
      async create(
        input: CreateLocalPackageInput,
      ): Promise<LocalPackageResult> {
        const result = await executeAdminCommand<
          { package: LocalPackageResult }
        >(
          "package.local.create",
          optionalArguments(input as unknown as Record<string, unknown>),
        );
        return result.package;
      },
    }),
    repository: Object.freeze({
      async inspect(packageId: string): Promise<PackageRepository> {
        const result = await executeAdminCommand<
          { repository: PackageRepository }
        >("package.repository.inspect", { package_id: packageId });
        return result.repository;
      },
      async pull(packageId: string): Promise<PackageRepository> {
        const result = await executeAdminCommand<
          { repository: PackageRepository }
        >("package.repository.pull", { package_id: packageId });
        return result.repository;
      },
      async push(packageId: string): Promise<PackageRepository> {
        const result = await executeAdminCommand<
          { repository: PackageRepository }
        >("package.repository.push", { package_id: packageId });
        return result.repository;
      },
      async checkout(
        input: CheckoutPackageRepositoryInput,
      ): Promise<PackageRepository> {
        const result = await executeAdminCommand<
          { repository: PackageRepository }
        >(
          "package.repository.checkout",
          optionalArguments({
            package_id: input.packageId,
            branch: input.branch,
            commit: input.commit,
          }),
        );
        return result.repository;
      },
    }),
  }),
  admin: Object.freeze({
    execute: executeAdminCommand,
  }),
});

function assertWorkerInvokeInput(
  input: WorkerInvokeInput,
): asserts input is WorkerInvokeInput {
  if (
    input === null || typeof input !== "object" ||
    typeof input.nodeId !== "string" || input.nodeId.length === 0 ||
    typeof input.sandboxId !== "string" || input.sandboxId.length === 0 ||
    typeof input.workerId !== "string" || input.workerId.length === 0 ||
    (input.persistentExecutionId !== undefined &&
      (typeof input.persistentExecutionId !== "string" ||
        input.persistentExecutionId.length === 0)) ||
    typeof input.function !== "string" || input.function.length === 0 ||
    input.function.length > 128
  ) throw new TypeError("exact Worker target and function are required");
}

export default kernel;
