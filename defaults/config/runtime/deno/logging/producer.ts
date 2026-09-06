import {
  addDrops,
  allows,
  BATCH_BYTES,
  decodePacket,
  type DropCounts,
  encodePacket,
  INITIAL_POLICY,
  LOG_VERSION,
  type LogPolicy,
  type LogRecord,
  type LogSink,
  MAX_FRAME,
  validPolicy,
} from "./protocol.ts";
import { LogQueue } from "./queue.ts";

class Pulse {
  #pending = false;
  #resolve?: () => void;
  signal(): void {
    this.#pending = true;
    this.#resolve?.();
    this.#resolve = undefined;
  }
  async wait(): Promise<void> {
    if (!this.#pending) {
      await new Promise<void>((resolve) => this.#resolve = resolve);
    }
    this.#pending = false;
  }
}

function close(connection: Deno.Conn): void {
  try {
    connection.close();
  } catch { /* Already closed. */ }
}

export async function writeAll(
  connection: Pick<Deno.Conn, "write">,
  value: Uint8Array,
): Promise<void> {
  let offset = 0;
  while (offset < value.length) {
    const count = await connection.write(value.subarray(offset));
    if (count <= 0 || count > value.length - offset) {
      throw new Error("incomplete log socket write");
    }
    offset += count;
  }
}

async function readExact(
  connection: Pick<Deno.Conn, "read">,
  value: Uint8Array,
): Promise<void> {
  let offset = 0;
  while (offset < value.length) {
    const count = await connection.read(value.subarray(offset));
    if (count === null || count <= 0 || count > value.length - offset) {
      throw new Error("log socket closed");
    }
    offset += count;
  }
}

export async function readPacket(
  connection: Pick<Deno.Conn, "read">,
): Promise<unknown> {
  const header = new Uint8Array(4);
  await readExact(connection, header);
  const length = new DataView(header.buffer).getUint32(0);
  if (length === 0 || length > MAX_FRAME) {
    throw new Error("invalid log frame length");
  }
  const frame = new Uint8Array(length + 4);
  frame.set(header);
  await readExact(connection, frame.subarray(4));
  return decodePacket(frame);
}

interface ProducerOptions {
  socket: string;
  sandboxId: string;
  nodeId: string;
  token: string;
  connect?: () => Promise<Deno.Conn>;
}

export class LogProducer implements LogSink {
  #options: ProducerOptions;
  #queue = new LogQueue();
  #policy: Readonly<LogPolicy> = INITIAL_POLICY;
  #listeners = new Set<(policy: Readonly<LogPolicy>) => void>();
  #pulse = new Pulse();
  #connection?: Deno.Conn;
  #accepting = true;
  #closing = false;
  #stopped = false;
  #done: Promise<void>;
  #close?: Promise<void>;
  #connected = false;
  #failures = 0;
  #ticker: ReturnType<typeof setInterval>;

  constructor(options: ProducerOptions) {
    this.#options = options;
    this.#ticker = setInterval(() => this.#pulse.signal(), 1000);
    this.#done = this.#run();
  }

  get policy(): Readonly<LogPolicy> {
    return this.#policy;
  }

  get status(): {
    connected: boolean;
    failures: number;
    pendingBytes: number;
    pendingRecords: number;
    dropped: DropCounts;
  } {
    return {
      connected: this.#connected,
      failures: this.#failures,
      pendingBytes: this.#queue.bytes,
      pendingRecords: this.#queue.records,
      dropped: [...this.#queue.dropped],
    };
  }

  subscribe(listener: (policy: Readonly<LogPolicy>) => void): () => void {
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  }

  #publish(value: unknown): void {
    if (!validPolicy(value)) throw new Error("invalid log policy");
    this.#policy = Object.freeze({
      revision: value.revision,
      enabled: value.enabled,
      level: value.level,
    });
    for (const listener of this.#listeners) listener(this.#policy);
  }

  emit(record: LogRecord): void {
    if (!this.#accepting || !allows(this.#policy, record.level)) return;
    try {
      const frame = encodePacket({
        record: {
          ...record,
          source: "deno",
          node_id: this.#options.nodeId,
          sandbox_id: this.#options.sandboxId,
        },
      });
      this.#queue.add(frame, record.level);
    } catch {
      this.#queue.drop(record.level);
    }
    this.#pulse.signal();
  }

  dropped(counts: DropCounts): void {
    addDrops(this.#queue.dropped, counts);
    // Fixed cumulative counters are sent at most once per second, and on close.
  }

  async #timed<T>(
    connection: Deno.Conn,
    operation: () => Promise<T>,
  ): Promise<T> {
    const timer = setTimeout(() => close(connection), 250);
    try {
      return await operation();
    } finally {
      clearTimeout(timer);
    }
  }

  async #run(): Promise<void> {
    try {
      while (!this.#stopped) {
        let connection: Deno.Conn | undefined;
        try {
          // Deno's Unix connect has no cancellation parameter. Keep exactly one
          // attempt in flight, and close a late result after bounded shutdown.
          connection = await (this.#options.connect?.() ??
            Deno.connect({ transport: "unix", path: this.#options.socket }));
          if (this.#stopped) {
            close(connection);
            break;
          }
          this.#connection = connection;
          await this.#timed(connection, async () => {
            await writeAll(
              connection!,
              encodePacket({
                version: LOG_VERSION,
                id: this.#options.sandboxId,
                token: this.#options.token,
              }),
            );
            this.#publish(await readPacket(connection!));
          });
          this.#connected = true;
          await this.#session(connection);
        } catch {
          this.#failures = Math.min(
            Number.MAX_SAFE_INTEGER,
            this.#failures + 1,
          );
        } finally {
          this.#connected = false;
          if (connection !== undefined) close(connection);
          this.#connection = undefined;
        }
        if (this.#closing || this.#stopped) break;
        await new Promise<void>((resolve) => setTimeout(resolve, 100));
      }
    } finally {
      clearInterval(this.#ticker);
      this.#discard();
    }
  }

  async #session(connection: Deno.Conn): Promise<void> {
    let live = true;
    const reader = (async () => {
      try {
        while (live) this.#publish(await readPacket(connection));
      } catch {
        /* Writer owns reconnect, never recursive console output. */
      } finally {
        live = false;
        close(connection);
        this.#pulse.signal();
      }
    })();
    const buffer = new Uint8Array(BATCH_BYTES);
    let reported: DropCounts = [0, 0, 0, 0];
    let reportAt = 0;
    try {
      while (live && !this.#stopped) {
        if (this.#queue.waiting > 0) {
          const batch = this.#queue.take(BATCH_BYTES);
          const policy = this.#policy;
          try {
            let length = 0;
            for (const entry of batch.entries) {
              if (!allows(policy, entry.level)) continue;
              buffer.set(entry.frame, length);
              length += entry.frame.length;
            }
            if (length > 0) {
              await this.#timed(
                connection,
                () => writeAll(connection, buffer.subarray(0, length)),
              );
            }
          } catch (error) {
            // A partial socket write has uncertain delivery. Do not replay it.
            for (const entry of batch.entries) {
              if (allows(policy, entry.level)) this.#queue.drop(entry.level);
            }
            throw error;
          } finally {
            batch.release();
          }
        }
        if (this.#closing || Date.now() >= reportAt) {
          const counts: DropCounts = [...this.#queue.dropped];
          if (counts.some((count, index) => count !== reported[index])) {
            await this.#timed(
              connection,
              () => writeAll(connection, encodePacket({ dropped: counts })),
            );
            reported = counts;
          }
          reportAt = Date.now() + 1000;
        }
        if (this.#closing && this.#queue.waiting === 0) break;
        if (this.#queue.waiting === 0) await this.#pulse.wait();
      }
    } finally {
      live = false;
      close(connection);
      await reader;
    }
  }

  #discard(): void {
    while (this.#queue.waiting > 0) {
      const batch = this.#queue.take(BATCH_BYTES);
      for (const entry of batch.entries) {
        if (allows(this.#policy, entry.level)) this.#queue.drop(entry.level);
      }
      batch.release();
    }
  }

  close(): Promise<void> {
    return this.#close ??= this.#finish();
  }

  async #finish(): Promise<void> {
    this.#accepting = false;
    this.#closing = true;
    this.#pulse.signal();
    let timer: ReturnType<typeof setTimeout> | undefined;
    try {
      await Promise.race([
        this.#done,
        new Promise<void>((resolve) => timer = setTimeout(resolve, 500)),
      ]);
    } finally {
      clearTimeout(timer);
      clearInterval(this.#ticker);
      this.#stopped = true;
      if (this.#connection !== undefined) close(this.#connection);
      this.#pulse.signal();
      this.#listeners.clear();
    }
  }
}
