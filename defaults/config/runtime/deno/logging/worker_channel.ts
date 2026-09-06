import { isId } from "../identity/mod.ts";
import type { ExecutionMetadata } from "../worker/contracts.ts";
import { captureRecord, type LogExecution } from "./capture.ts";
import {
  addDrops,
  allows,
  decodePacket,
  type DropCounts,
  encodePacket,
  type Level,
  LEVELS,
  type LogPolicy,
  type LogRecord,
  MAX_FRAME,
  validPolicy,
} from "./protocol.ts";

export const WORKER_LOG_BYTES = 64 * 1024;
export const WORKER_LOG_SLOTS = 64;
export const REPORT_WEIGHT = 256;

type Port = Pick<MessagePort, "postMessage">;

export class WorkerLogSender {
  #port: Port;
  #metadata: ExecutionMetadata;
  #execution: () => LogExecution | undefined;
  #policy: Readonly<LogPolicy>;
  #bytes = 0;
  #records = 0;
  #drops: DropCounts = [0, 0, 0, 0];

  constructor(
    port: Port,
    metadata: ExecutionMetadata,
    execution: () => LogExecution | undefined,
    policy: Readonly<LogPolicy>,
  ) {
    this.#port = port;
    this.#metadata = Object.freeze({
      ...metadata,
      user: Object.freeze({ ...metadata.user }),
      origin: Object.freeze({ ...metadata.origin }),
    });
    this.#execution = execution;
    this.#policy = policy;
  }

  get pendingBytes(): number {
    return this.#bytes;
  }
  get pendingRecords(): number {
    return this.#records;
  }

  #hasCredit(level: Level, weight: number): boolean {
    const high = LEVELS.indexOf(level) >= 2;
    return this.#bytes + weight <= WORKER_LOG_BYTES - (high ? 0 : 16 * 1024) &&
      this.#records < WORKER_LOG_SLOTS - (high ? 0 : 16);
  }

  #drop(level: Level): void {
    const counts: DropCounts = [0, 0, 0, 0];
    counts[LEVELS.indexOf(level)] = 1;
    addDrops(this.#drops, counts);
  }

  enabled(level: Level): boolean {
    if (!LEVELS.includes(level) || !allows(this.#policy, level)) return false;
    if (this.#hasCredit(level, REPORT_WEIGHT)) return true;
    this.#drop(level);
    return false;
  }

  print(
    level: Level,
    values: readonly unknown[],
    fields?: Record<string, unknown>,
  ): void {
    if (!this.enabled(level)) return;
    let frame: Uint8Array;
    try {
      frame = encodePacket({
        record: captureRecord(
          this.#metadata,
          this.#execution(),
          level,
          values,
          fields,
        ),
      });
    } catch {
      this.#drop(level);
      return;
    }
    const weight = frame.length + 128;
    if (!this.#hasCredit(level, weight)) {
      this.#drop(level);
      return;
    }
    const dropped = this.#drops;
    try {
      this.#port.postMessage({ type: "log_record", frame, dropped }, [
        frame.buffer as ArrayBuffer,
      ]);
    } catch {
      this.#drop(level);
      return;
    }
    this.#bytes += weight;
    this.#records++;
    this.#drops = [0, 0, 0, 0];
  }

  handle(message: { type: string; payload?: unknown }): boolean {
    if (message.type === "log_policy") {
      if (validPolicy(message.payload)) {
        this.#policy = Object.freeze({ ...message.payload });
      }
      try {
        this.#port.postMessage({ type: "log_policy_ack" });
      } catch {
        // A detached logging channel must not terminate its application Worker.
      }
      return true;
    }
    if (message.type !== "log_credit") return false;
    const credit = message.payload as { bytes?: unknown } | undefined;
    if (
      credit !== undefined && Number.isSafeInteger(credit.bytes) &&
      Number(credit.bytes) > 0 && Number(credit.bytes) <= this.#bytes &&
      this.#records > 0
    ) {
      this.#bytes -= Number(credit.bytes);
      this.#records--;
      if (
        this.#drops.some((count) => count > 0) &&
        this.#hasCredit("ERROR", REPORT_WEIGHT)
      ) {
        const dropped = this.#drops;
        try {
          this.#port.postMessage({ type: "log_dropped", dropped });
        } catch {
          return true;
        }
        this.#bytes += REPORT_WEIGHT;
        this.#records++;
        this.#drops = [0, 0, 0, 0];
      }
    }
    return true;
  }
}

// The private port binds the Worker. Rebuild the small record from allowed
// fields; application-originated data cannot relabel its fixed identities.
export function bindWorkerRecord(
  frame: Uint8Array,
  metadata: ExecutionMetadata,
): LogRecord {
  if (frame.length > MAX_FRAME + 4) {
    throw new Error("oversized Worker log frame");
  }
  const packet = decodePacket(frame) as { record?: Partial<LogRecord> };
  const record = packet?.record;
  if (
    record === undefined || record === null ||
    !LEVELS.includes(record.level as Level) ||
    typeof record.time !== "string" || record.time.length > 35 ||
    !Number.isFinite(Date.parse(record.time)) ||
    typeof record.message !== "string" || record.message.length > 16 * 1024
  ) throw new Error("invalid Worker log record");
  for (
    const [field, prefix] of [
      ["context_id", "ctx"],
      ["parent_context_id", "ctx"],
      ["job_id", "job"],
      ["persistent_id", "pex"],
    ] as const
  ) {
    if (record[field] !== undefined && !isId(record[field], prefix)) {
      throw new Error("invalid log invocation identity");
    }
  }
  if (
    record.username !== undefined && !/^[a-z0-9]{3,32}$/.test(record.username)
  ) throw new Error("invalid log username");
  const attributes: Record<string, string> = Object.create(null);
  if (record.attributes !== undefined) {
    if (
      record.attributes === null || typeof record.attributes !== "object" ||
      Array.isArray(record.attributes)
    ) throw new Error("invalid log attributes");
    let count = 0;
    let length = 0;
    for (const [key, value] of Object.entries(record.attributes)) {
      if (
        ++count > 16 || typeof value !== "string" || key.length > 64 ||
        value.length > 512 || (length += key.length + value.length) > 2048
      ) throw new Error("invalid log attributes");
      attributes[key] = value;
    }
  }
  return {
    time: record.time,
    level: record.level as Level,
    source: "deno",
    component: "worker",
    node_id: metadata.nodeId,
    sandbox_id: metadata.sandboxId,
    worker_id: metadata.workerId,
    service_id: metadata.workloadType === "service"
      ? metadata.workloadId
      : undefined,
    object: `${metadata.origin.type}:${metadata.origin.id}`,
    context_id: record.context_id,
    parent_context_id: record.parent_context_id,
    job_id: record.job_id,
    persistent_id: record.persistent_id,
    username: record.username,
    message: record.message,
    attributes: Object.keys(attributes).length === 0 ? undefined : attributes,
  };
}
