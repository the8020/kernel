export type {
  ServiceConfiguration,
  ServiceIndexScope,
  ServiceIndexState,
  ServiceSpecification,
} from "./services.ts";
import type {
  EventReceipt,
  ProgramRun,
  ProgramRunInput,
  ProgramSummary,
} from "./programs.ts";
export type {
  EventReceipt,
  PackageEvent,
  ProgramRun,
  ProgramRunInput,
  ProgramSummary,
} from "./programs.ts";

// The signing key stays in the kernel. Claim meanings belong to callers.
export type TokenClaims = Readonly<Record<string, unknown>>;

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

export interface CommandArgumentSpec {
  values?: readonly string[];
  booleans?: readonly string[];
}

export interface ParsedCommandArguments {
  positionals: string[];
  options: Record<string, string | boolean>;
}

/** Parse package-owned command flags while preserving positional token text. */
export function parseCommandArguments(
  arguments_: readonly string[],
  spec: CommandArgumentSpec = {},
): ParsedCommandArguments {
  const valueOptions = new Set(spec.values ?? []);
  const booleanOptions = new Set(spec.booleans ?? []);
  const options: Record<string, string | boolean> = {};
  const positionals: string[] = [];
  let parseOptions = true;
  for (let index = 0; index < arguments_.length; index++) {
    const token = arguments_[index]!;
    if (parseOptions && token === "--") {
      parseOptions = false;
      continue;
    }
    if (!parseOptions || !token.startsWith("--") || token === "--") {
      positionals.push(token);
      continue;
    }
    const option = token.slice(2);
    const equals = option.indexOf("=");
    const name = equals < 0 ? option : option.slice(0, equals);
    const inline = equals < 0 ? undefined : option.slice(equals + 1);
    if (name.length === 0 || options[name] !== undefined) {
      throw invalidArguments(`invalid or repeated option --${name}`);
    }
    if (booleanOptions.has(name)) {
      if (inline !== undefined && inline !== "true" && inline !== "false") {
        throw invalidArguments(`--${name} must be true or false`);
      }
      options[name] = inline === undefined ? true : inline === "true";
      continue;
    }
    if (!valueOptions.has(name)) {
      throw invalidArguments(`unknown option --${name}`);
    }
    const value = inline ?? arguments_[++index];
    if (value === undefined) {
      throw invalidArguments(`--${name} requires a value`);
    }
    options[name] = value;
  }
  return { positionals, options };
}

export function requiredCommandArgument(
  values: readonly string[],
  index: number,
  name: string,
): string {
  const value = values[index];
  if (value === undefined || value.length === 0) {
    throw invalidArguments(`${name} is required`);
  }
  return value;
}

export interface PackageIndex {
  author: string;
  repository: string;
  source?: string;
  commit?: string;
  tag?: string;
  secret?: string;
  local: boolean;
  package_id: string;
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

function invalidArguments(message: string): AdminCommandError {
  return new AdminCommandError({ code: "invalid_arguments", message });
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
    | "INITIALIZATION_FAILED";
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

export type KernelInvoke = (
  operation: KernelOperation,
  input: Record<string, unknown>,
) => Promise<unknown>;

export const kernelInvokeSymbol = Symbol.for("the8020.kernel.invoke");
export const kernelSecretSymbol = Symbol.for("the8020.kernel.secret");
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

function executionSecret(name: string): string {
  if (typeof name !== "string" || name.length === 0) {
    throw new TypeError("secret name is required");
  }
  const resolve = (globalThis as unknown as Record<symbol, unknown>)[
    kernelSecretSymbol
  ];
  if (typeof resolve !== "function") {
    throw new Error("kernel execution context is unavailable");
  }
  const value = (resolve as (name: string) => string | undefined)(name);
  if (value === undefined) {
    throw new Error(`execution secret ${name} is unavailable`);
  }
  return value;
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

interface RuntimeOperationResponse<Result> {
  success: boolean;
  result?: Result;
  error?: AdminCommandErrorValue;
}

async function executeRuntimeOperation<Result>(
  operation: string,
  input: Record<string, unknown> = {},
): Promise<Result> {
  if (operation.length === 0) {
    return Promise.reject(new TypeError("runtime operation is required"));
  }
  const response = await invoke<RuntimeOperationResponse<Result>>(
    "runtime.operation",
    { operation, input },
  );
  if (
    response === null || typeof response !== "object" ||
    typeof response.success !== "boolean"
  ) throw new Error("invalid kernel runtime operation response");
  if (!response.success) {
    if (
      response.error === undefined || typeof response.error.code !== "string" ||
      typeof response.error.message !== "string"
    ) throw new Error("kernel runtime operation failed");
    throw new AdminCommandError(response.error);
  }
  return response.result as Result;
}

async function runtimeOperationField<Result>(
  operation: string,
  input: Record<string, unknown>,
  field: string,
): Promise<Result> {
  const result = await executeRuntimeOperation<Record<string, unknown>>(
    operation,
    input,
  );
  if (result === null || typeof result !== "object" || !(field in result)) {
    throw new Error(`runtime operation ${operation} returned no ${field}`);
  }
  return result[field] as Result;
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
  options: {
    returnRows?: boolean;
    returnInsertId?: boolean;
    transaction?: string;
  } = {},
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
    ...(options.returnInsertId === undefined
      ? {}
      : { return_insert_id: options.returnInsertId }),
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

function settingOperations(scope: "global" | "node") {
  const operation = (action: string) => `settings.${scope}.${action}`;
  return Object.freeze({
    list(): Promise<Record<string, unknown>[]> {
      return runtimeOperationField(operation("list"), {}, "settings");
    },
    get(key: string): Promise<Record<string, unknown>> {
      return runtimeOperationField(operation("get"), { key }, "setting");
    },
    set(key: string, value: string): Promise<Record<string, unknown>> {
      return runtimeOperationField(
        operation("set"),
        { key, value },
        "setting",
      );
    },
    unset(key: string): Promise<Record<string, unknown>> {
      return runtimeOperationField(operation("unset"), { key }, "setting");
    },
  });
}

export const kernel = Object.freeze({
  programs: Object.freeze({
    list(): Promise<ProgramSummary[]> {
      return executeRuntimeOperation("program.list");
    },
    run(input: ProgramRunInput): Promise<ProgramRun> {
      return executeRuntimeOperation("program.run", { ...input });
    },
  }),
  events: Object.freeze({
    emit(name: string, data: unknown = null): Promise<EventReceipt> {
      return executeRuntimeOperation("event.emit", { name, data });
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
    secret: executionSecret,
    optionalSecret(name: string): string | undefined {
      try {
        return executionSecret(name);
      } catch {
        return undefined;
      }
    },
    completePersistent(): Promise<void> {
      return invoke<void>("execution.completePersistent", {});
    },
  }),
  crypto: Object.freeze({
    sign(data: Uint8Array): Promise<string> {
      return runtimeOperationField(
        "crypto.sign",
        { data: data.toBase64() },
        "signature",
      );
    },
    verify(data: Uint8Array, signature: string): Promise<boolean> {
      return runtimeOperationField("crypto.verify", {
        data: data.toBase64(),
        signature,
      }, "valid");
    },
    token: Object.freeze({
      sign(claims: TokenClaims): Promise<string> {
        return runtimeOperationField("crypto.token.sign", { claims }, "token");
      },
      verify(token: string): Promise<TokenClaims | null> {
        return executeRuntimeOperation("crypto.token.verify", { token });
      },
    }),
  }),
  services: Object.freeze({
    list<Result = Record<string, unknown>>(): Promise<Result[]> {
      return runtimeOperationField("service.list", {}, "services");
    },
    inspect<Result = Record<string, unknown>>(
      serviceId: string,
    ): Promise<Result> {
      return runtimeOperationField(
        "service.inspect",
        { service_id: serviceId },
        "service",
      );
    },
    refresh<Result = Record<string, unknown>>(
      serviceId: string,
    ): Promise<Result> {
      return runtimeOperationField(
        "service.refresh",
        { service_id: serviceId },
        "service",
      );
    },
    validate(serviceId: string): Promise<Record<string, unknown>> {
      return executeRuntimeOperation("service.validate", {
        service_id: serviceId,
      });
    },
    openapi(serviceId: string): Promise<Record<string, unknown>> {
      return runtimeOperationField(
        "service.openapi",
        { service_id: serviceId },
        "openapi",
      );
    },
    request(input: Record<string, unknown>): Promise<Record<string, unknown>> {
      return runtimeOperationField("service.request", input, "response");
    },
  }),
  nodes: Object.freeze({
    list(): Promise<Record<string, unknown>> {
      return executeRuntimeOperation("node.list");
    },
    set(input: Record<string, unknown>): Promise<Record<string, unknown>> {
      return runtimeOperationField("node.set", input, "node");
    },
    remove(nodeId: string): Promise<Record<string, unknown>> {
      return executeRuntimeOperation("node.remove", { node_id: nodeId });
    },
  }),
  settings: Object.freeze({
    global: settingOperations("global"),
    node: settingOperations("node"),
  }),
  development: Object.freeze({
    imageStatus(): Promise<Record<string, unknown>> {
      return runtimeOperationField("development.image.status", {}, "image");
    },
    sandbox: Object.freeze({
      list(): Promise<unknown[]> {
        return runtimeOperationField(
          "development.sandbox.list",
          {},
          "sandboxes",
        );
      },
      run(
        action: string,
        userId: string,
        input: Record<string, unknown> = {},
      ): Promise<Record<string, unknown>> {
        return executeRuntimeOperation(
          `development.sandbox.${action.replaceAll("-", "_")}`,
          { user_id: userId, ...input },
        );
      },
    }),
    activate: Object.freeze({
      preview(
        input: Record<string, unknown>,
      ): Promise<Record<string, unknown>> {
        return runtimeOperationField(
          "development.activate.preview",
          input,
          "preview",
        );
      },
      run(input: Record<string, unknown>): Promise<Record<string, unknown>> {
        return runtimeOperationField(
          "development.activate.run",
          input,
          "activation",
        );
      },
    }),
  }),
  database: Object.freeze({
    check(): Promise<Record<string, unknown>> {
      return executeRuntimeOperation("database.check");
    },
    info(): Promise<DatabaseInfo> {
      return invoke<DatabaseInfo>("database.info", {});
    },
    execute(
      statement: string,
      parameters: DatabaseValue[] = [],
      options: {
        returnRows?: boolean;
        returnInsertId?: boolean;
        transaction?: string;
      } = {},
    ): Promise<DatabaseExecuteResult> {
      return invoke<DatabaseExecuteResult>(
        "database.execute",
        databaseArguments(statement, parameters, options),
      );
    },
    transaction: Object.freeze({
      begin(
        settings: {
          isolationLevel?: string;
          readOnly?: boolean;
          timeoutMs?: number;
          lockTimeoutMs?: number;
        } = {},
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
        const result = await executeRuntimeOperation<
          { tables: DatabaseTableSummary[] }
        >("database.table.list");
        return result.tables;
      },
      async definitions(): Promise<DatabaseDefinitionSummary[]> {
        const result = await executeRuntimeOperation<
          { definitions: DatabaseDefinitionSummary[] }
        >("database.table.definitions");
        return result.definitions;
      },
      async inspect(tableId: string): Promise<Record<string, unknown>> {
        const result = await executeRuntimeOperation<
          { table: Record<string, unknown> }
        >("database.table.inspect", { table_id: tableId });
        return result.table;
      },
      async compare(tableId: string): Promise<Record<string, unknown>> {
        const result = await executeRuntimeOperation<
          { table: Record<string, unknown> }
        >("database.table.compare", { table_id: tableId });
        return result.table;
      },
      async synchronize(
        tableId: string,
        sourcePackage?: string,
      ): Promise<DatabaseSynchronizationResult> {
        const result = await executeRuntimeOperation<
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
        const result = await executeRuntimeOperation<
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
        await executeRuntimeOperation(
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
      const result = await executeRuntimeOperation<
        { secrets: SecretSummary[] }
      >(
        "secret.list",
      );
      return result.secrets;
    },
    async get(name: string): Promise<Secret> {
      const result = await executeRuntimeOperation<{ secret: Secret }>(
        "secret.get",
        { name },
      );
      return result.secret;
    },
    async set(input: SetSecretInput): Promise<SecretSummary> {
      const result = await executeRuntimeOperation<{ secret: SecretSummary }>(
        "secret.set",
        { name: input.name, value: input.value },
      );
      return result.secret;
    },
  }),
  packages: Object.freeze({
    list<Result = Record<string, unknown>>(): Promise<Result[]> {
      return runtimeOperationField("package.list", {}, "packages");
    },
    inspect<Result = Record<string, unknown>>(
      packageId: string,
    ): Promise<Result> {
      return runtimeOperationField(
        "package.inspect",
        { package_id: packageId },
        "package",
      );
    },
    index: Object.freeze({
      async list(): Promise<PackageIndex[]> {
        const result = await executeRuntimeOperation<
          { packages: PackageIndex[] }
        >(
          "package.index.list",
        );
        return result.packages;
      },
      async inspect(packageId: string): Promise<PackageIndex> {
        const result = await executeRuntimeOperation<{ package: PackageIndex }>(
          "package.index.inspect",
          { package_id: packageId },
        );
        return result.package;
      },
      async set(input: SetPackageIndexInput): Promise<PackageIndex> {
        const result = await executeRuntimeOperation<{ package: PackageIndex }>(
          "package.index.set",
          optionalArguments(input as unknown as Record<string, unknown>),
        );
        return result.package;
      },
    }),
    source: Object.freeze({
      async inspect(source: string): Promise<PackageSourceInspection> {
        const result = await executeRuntimeOperation<
          { source: PackageSourceInspection }
        >("package.source.inspect", { source });
        return result.source;
      },
    }),
    versions: Object.freeze({
      async list(packageId: string, limit?: number): Promise<PackageVersions> {
        const result = await executeRuntimeOperation<
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
      gitToken?: string,
    ): Promise<PackageSynchronization[]> {
      const result = await executeRuntimeOperation<
        { packages: PackageSynchronization[] }
      >(
        "package.synchronize",
        optionalArguments({
          packages: packageIds.length === 0 ? undefined : packageIds.join(","),
          git_token: gitToken,
        }),
      );
      return result.packages;
    },
    local: Object.freeze({
      async create(
        input: CreateLocalPackageInput,
      ): Promise<LocalPackageResult> {
        const result = await executeRuntimeOperation<
          { package: LocalPackageResult }
        >(
          "package.local.create",
          optionalArguments(input as unknown as Record<string, unknown>),
        );
        return result.package;
      },
    }),
    repository: Object.freeze({
      list(): Promise<unknown[]> {
        return runtimeOperationField(
          "package.repository.list",
          {},
          "repositories",
        );
      },
      status(packageId: string): Promise<Record<string, unknown>> {
        return runtimeOperationField(
          "package.repository.status",
          { package_id: packageId },
          "repository",
        );
      },
      initialize(
        input: Record<string, unknown>,
      ): Promise<Record<string, unknown>> {
        return runtimeOperationField(
          "package.repository.init",
          input,
          "repository",
        );
      },
      remote(input: Record<string, unknown>): Promise<Record<string, unknown>> {
        return runtimeOperationField(
          "package.repository.remote",
          input,
          "repository",
        );
      },
      async inspect(packageId: string): Promise<PackageRepository> {
        const result = await executeRuntimeOperation<
          { repository: PackageRepository }
        >("package.repository.inspect", { package_id: packageId });
        return result.repository;
      },
      async pull(packageId: string): Promise<PackageRepository> {
        const result = await executeRuntimeOperation<
          { repository: PackageRepository }
        >("package.repository.pull", { package_id: packageId });
        return result.repository;
      },
      async push(packageId: string): Promise<PackageRepository> {
        const result = await executeRuntimeOperation<
          { repository: PackageRepository }
        >("package.repository.push", { package_id: packageId });
        return result.repository;
      },
      async checkout(
        input: CheckoutPackageRepositoryInput,
      ): Promise<PackageRepository> {
        const result = await executeRuntimeOperation<
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
