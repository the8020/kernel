import type {
  ExecutionMetadata,
  RuntimeLogEvent,
} from "../worker/contracts.ts";
import { formatAttributes, formatValues } from "./format.ts";
import {
  allows,
  type Level,
  type LogPolicy,
  type LogRecord,
} from "./protocol.ts";

// This is a structural view of the existing bridge ALS store, never another
// mutable current-request slot or a retained map of execution contexts.
export interface LogExecution {
  readonly contextId: string;
  readonly parentContextId?: string;
  readonly jobRunId?: string;
  readonly persistentExecutionId?: string;
  readonly user: { readonly username: string };
  readonly secrets?: Readonly<Record<string, string>>;
}

export function captureRecord(
  metadata: ExecutionMetadata,
  execution: LogExecution | undefined,
  level: Level,
  values: readonly unknown[],
  fields?: Record<string, unknown>,
): LogRecord {
  const time = new Date().toISOString();
  return {
    time,
    level,
    source: "deno",
    component: "worker",
    node_id: metadata.nodeId,
    sandbox_id: metadata.sandboxId,
    worker_id: metadata.workerId,
    service_id: metadata.workloadType === "service"
      ? metadata.workloadId
      : undefined,
    object: `${metadata.origin.type}:${metadata.origin.id}`,
    context_id: execution?.contextId,
    parent_context_id: execution?.parentContextId,
    job_id: execution?.jobRunId,
    persistent_id: execution?.persistentExecutionId,
    username: execution?.user.username,
    message: formatValues(values, 12 * 1024, execution?.secrets),
    attributes: formatAttributes(fields, execution?.secrets),
  };
}

export interface ConsoleCapture {
  enabled(level: Level): boolean;
  print(
    level: Level,
    values: readonly unknown[],
    fields?: Record<string, unknown>,
  ): void;
}

// Extend the existing console path. Never call the original console for a
// structured print: that would duplicate it as anonymous native output.
export function installConsoleCapture(capture: ConsoleCapture): () => void {
  const original = {
    debug: console.debug,
    info: console.info,
    log: console.log,
    warn: console.warn,
    error: console.error,
  };
  for (const level of ["debug", "info", "warn", "error"] as const) {
    console[level] = (...values: unknown[]): void => {
      const severity = level.toUpperCase() as Level;
      if (capture.enabled(severity)) capture.print(severity, values);
    };
  }
  console.log = console.info;
  return () => Object.assign(console, original);
}

export function runtimeLog(
  capture: ConsoleCapture,
  event: RuntimeLogEvent,
): void {
  const level = event.level.toUpperCase() as Level;
  if (capture.enabled(level)) {
    capture.print(level, [event.message], event.fields);
  }
}

export function policyCapture(
  policy: () => Readonly<LogPolicy>,
  emit: ConsoleCapture["print"],
): ConsoleCapture {
  return { enabled: (level) => allows(policy(), level), print: emit };
}
