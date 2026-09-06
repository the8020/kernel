import type {
  Level,
  LogRecord as ProducerRecord,
} from "../logging/protocol.ts";

/** Filters use capture time (from inclusive, until exclusive) and exact IDs. */
export interface LogQuery {
  node_id?: string;
  sandbox_id?: string;
  worker_id?: string;
  context_id?: string;
  parent_context_id?: string;
  job_id?: string;
  service_id?: string;
  persistent_id?: string;
  object?: string;
  username?: string;
  from?: string;
  until?: string;
  level?: Level | Lowercase<Level>;
  source?: "kernel" | "deno" | "logd";
  limit?: number;
  position?: string;
  cursor?: string;
  tail?: boolean;
}

export type LogRecord = Omit<ProducerRecord, "source" | "sandbox_id"> & {
  source: "kernel" | "deno" | "logd";
  sandbox_id?: string;
  stream?: "stdout" | "stderr";
  segment: string;
  offset: number;
};

export interface LogPage {
  state: "ok" | "expired" | "unavailable";
  reason?: string;
  records: LogRecord[];
  cursor?: string;
  more: boolean;
  scanned_bytes: number;
  corrupt_records?: number;
  tail_limited?: boolean;
}

export interface LogFollowOptions {
  signal: AbortSignal;
  intervalMs?: number;
}

export type LogQueryFunction = (
  input: LogQuery,
  signal?: AbortSignal,
) => Promise<LogPage>;

/** One page at a time; no retained history or overlapping requests. */
export async function* followLogs(
  query: LogQueryFunction,
  input: LogQuery,
  options: LogFollowOptions,
): AsyncGenerator<LogPage> {
  const interval = options.intervalMs ?? 500;
  if (!Number.isInteger(interval) || interval < 100 || interval > 60000) {
    throw new TypeError(
      "Log polling interval must be between 100 and 60000 ms",
    );
  }
  let next: LogQuery = { ...input };
  if (!next.position && !next.cursor && next.tail === undefined) {
    next.tail = true;
  }
  while (!options.signal.aborted) {
    const page = await query(next, options.signal);
    yield page;
    if (page.state !== "ok" || !page.cursor) return;
    next = { ...input, cursor: page.cursor };
    delete next.position;
    delete next.tail;
    if (!page.more) await pause(interval, options.signal);
  }
}

function pause(interval: number, signal: AbortSignal): Promise<void> {
  if (signal.aborted) return Promise.resolve();
  return new Promise((resolve) => {
    const finish = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", finish);
      resolve();
    };
    const timer = setTimeout(finish, interval);
    signal.addEventListener("abort", finish, { once: true });
    if (signal.aborted) finish();
  });
}

/** Readable counterpart of the compact file format; stacks retain newlines. */
export function formatLogRecord(record: LogRecord): string {
  const identifiers = [
    record.node_id,
    record.sandbox_id,
    record.worker_id,
    record.context_id,
    record.job_id,
    record.service_id,
    record.persistent_id,
    record.parent_context_id && `parent:${record.parent_context_id}`,
    record.object,
    record.username && `user:${record.username}`,
    record.stream,
  ].filter(Boolean).join(" ");
  const attributes = record.attributes && Object.keys(record.attributes).length
    ? ` ${JSON.stringify(record.attributes)}`
    : "";
  return `${record.time} ${record.level} ${record.source}/${record.component} [${identifiers}] ${record.message}${attributes}`;
}
