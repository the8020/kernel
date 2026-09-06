// This private producer protocol matches kernel/logging/records. It is separate
// from execution RPC: ordinary prints never become kernel calls.
export const LOG_VERSION = 1;
export const MAX_FRAME = 32 * 1024;
export const BATCH_BYTES = 64 * 1024;
export const OMITTED = "…[middle omitted]…";
export const LEVELS = ["DEBUG", "INFO", "WARN", "ERROR"] as const;
export type Level = typeof LEVELS[number];
export type DropCounts = [number, number, number, number];

export interface LogPolicy {
  revision: number;
  enabled: boolean;
  level: "debug" | "info" | "warn" | "error";
}

export const INITIAL_POLICY: Readonly<LogPolicy> = Object.freeze({
  revision: 1,
  enabled: true,
  level: "info",
});

export interface LogRecord {
  time: string;
  level: Level;
  source: "deno";
  component: string;
  node_id: string;
  sandbox_id: string;
  worker_id?: string;
  context_id?: string;
  parent_context_id?: string;
  job_id?: string;
  service_id?: string;
  persistent_id?: string;
  object?: string;
  username?: string;
  message: string;
  attributes?: Record<string, string>;
}

export function allows(policy: Readonly<LogPolicy>, level: Level): boolean {
  return policy.enabled &&
    LEVELS.indexOf(level) >=
      LEVELS.indexOf(policy.level.toUpperCase() as Level);
}

export function validPolicy(value: unknown): value is LogPolicy {
  if (value === null || typeof value !== "object") return false;
  const policy = value as LogPolicy;
  return Number.isSafeInteger(policy.revision) && policy.revision > 0 &&
    typeof policy.enabled === "boolean" &&
    ["debug", "info", "warn", "error"].includes(policy.level);
}

export function validDrops(value: unknown): value is DropCounts {
  return Array.isArray(value) && value.length === 4 &&
    value.every((count) => Number.isSafeInteger(count) && count >= 0);
}

export function addDrops(target: DropCounts, values: DropCounts): void {
  for (let i = 0; i < 4; i++) {
    target[i] = Math.min(Number.MAX_SAFE_INTEGER, target[i]! + values[i]!);
  }
}

const encoder = new TextEncoder();
const decoder = new TextDecoder("utf-8", { fatal: true });

// Only bounded, runtime-created records/control values reach serialization.
export function encodePacket(value: unknown): Uint8Array {
  const payload = encoder.encode(JSON.stringify(value));
  if (payload.length === 0 || payload.length > MAX_FRAME) {
    throw new Error("log frame exceeds its bound");
  }
  const frame = new Uint8Array(payload.length + 4);
  new DataView(frame.buffer).setUint32(0, payload.length);
  frame.set(payload, 4);
  return frame;
}

export function decodePacket(frame: Uint8Array): unknown {
  if (
    frame.length < 5 || frame.length > MAX_FRAME + 4 ||
    new DataView(frame.buffer, frame.byteOffset).getUint32(0) !==
      frame.length - 4
  ) throw new Error("invalid log frame");
  return JSON.parse(decoder.decode(frame.subarray(4)));
}

export interface LogSink {
  readonly policy: Readonly<LogPolicy>;
  emit(record: LogRecord): void;
  dropped(counts: DropCounts): void;
  subscribe(listener: (policy: Readonly<LogPolicy>) => void): () => void;
}
