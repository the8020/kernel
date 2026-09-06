import { assertEquals } from "../test/assert.ts";
import { LogProducer } from "./producer.ts";
import {
  decodePacket,
  encodePacket,
  INITIAL_POLICY,
  type LogRecord,
  MAX_FRAME,
} from "./protocol.ts";

// Exercise real byte framing and partial asynchronous I/O without opening an
// unsandboxed service runtime or granting the unit suite filesystem writes.
class TestConnection {
  readonly received: Array<Record<string, unknown>> = [];
  closed = false;
  failNextWrite = false;
  stall = false;
  #input: Uint8Array[] = [];
  #read?: () => void;
  #stalled?: (error: Error) => void;
  #written = new Uint8Array(MAX_FRAME + 4);
  #length = 0;
  constructor() {
    this.push(INITIAL_POLICY);
  }
  push(value: unknown): void {
    this.#input.push(encodePacket(value));
    this.#read?.();
    this.#read = undefined;
  }
  async read(buffer: Uint8Array): Promise<number | null> {
    while (this.#input.length === 0 && !this.closed) {
      await new Promise<void>((resolve) => this.#read = resolve);
    }
    if (this.closed) return null;
    const current = this.#input[0]!;
    const count = Math.min(buffer.length, current.length, 7);
    buffer.set(current.subarray(0, count));
    if (count === current.length) this.#input.shift();
    else this.#input[0] = current.subarray(count);
    return count;
  }
  async write(buffer: Uint8Array): Promise<number> {
    if (this.closed) throw new Error("closed");
    if (this.stall) {
      await new Promise<void>((_resolve, reject) => this.#stalled = reject);
    }
    if (this.failNextWrite) {
      this.failNextWrite = false;
      throw new Error("write failed");
    }
    const count = Math.min(buffer.length, 31);
    this.#written.set(buffer.subarray(0, count), this.#length);
    this.#length += count;
    if (this.#length >= 4) {
      const length = new DataView(this.#written.buffer).getUint32(0) + 4;
      if (this.#length >= length) {
        this.received.push(
          decodePacket(this.#written.subarray(0, length)) as Record<
            string,
            unknown
          >,
        );
        this.#written.copyWithin(0, length, this.#length);
        this.#length -= length;
      }
    }
    return count;
  }
  close(): void {
    this.closed = true;
    this.#read?.();
    this.#read = undefined;
    this.#stalled?.(new Error("closed"));
    this.#stalled = undefined;
  }
  connection(): Deno.Conn {
    return this as unknown as Deno.Conn;
  }
}

const record = (message: string): LogRecord => ({
  time: new Date().toISOString(),
  level: "INFO",
  source: "deno",
  component: "worker",
  node_id: "nod-0123456789",
  sandbox_id: "sbx-0123456789",
  worker_id: "wrk-0123456789",
  context_id: "ctx-0123456789",
  job_id: "job-0123456789",
  username: "alice",
  message,
});
const options = {
  socket: "/not-opened-by-this-test",
  sandboxId: "sbx-0123456789",
  nodeId: "nod-0123456789",
  token: "test-token-value",
};

async function until(predicate: () => boolean): Promise<void> {
  const deadline = Date.now() + 2000;
  while (!predicate()) {
    if (Date.now() > deadline) throw new Error("logging condition timed out");
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
}

Deno.test("producer preserves startup prints on one authenticated framed connection and applies pushed policy", async () => {
  const connection = new TestConnection();
  let connects = 0;
  const producer = new LogProducer({
    ...options,
    connect: () => {
      connects++;
      return Promise.resolve(connection.connection());
    },
  });
  try {
    producer.emit(record("startup"));
    await until(() => connection.received.length === 2);
    for (let index = 0; index < 20; index++) {
      producer.emit(record("line " + index));
    }
    await until(() => connection.received.length === 22);
    assertEquals(connects, 1);
    assertEquals(connection.received[0], {
      version: 1,
      id: options.sandboxId,
      token: options.token,
    });
    assertEquals(
      (connection.received[1]!.record as LogRecord).username,
      "alice",
    );
    assertEquals(
      (connection.received.at(-1)!.record as LogRecord).message,
      "line 19",
    );
    connection.push({ revision: 2, enabled: false, level: "info" });
    await until(() => !producer.policy.enabled);
    producer.emit(record("filtered"));
    connection.push({ revision: 3, enabled: true, level: "warn" });
    await until(() => producer.policy.level === "warn");
    producer.emit(record("also filtered"));
    producer.emit({ ...record("retained"), level: "WARN" });
    await until(() => connection.received.length === 23);
    assertEquals(
      (connection.received.at(-1)!.record as LogRecord).message,
      "retained",
    );
  } finally {
    await producer.close();
  }
  assertEquals(connection.closed, true);
  assertEquals(producer.status.pendingBytes, 0);
});

Deno.test("producer accounts uncertain writes and reconnects without replaying their records", async () => {
  const first = new TestConnection(), second = new TestConnection();
  let connects = 0;
  const producer = new LogProducer({
    ...options,
    connect: () =>
      Promise.resolve((connects++ === 0 ? first : second).connection()),
  });
  try {
    await until(() => producer.status.connected);
    first.failNextWrite = true;
    producer.emit(record("uncertain"));
    await until(() => connects === 2 && producer.status.connected);
    producer.emit(record("after reconnect"));
    await until(() =>
      second.received.some((packet) =>
        (packet.record as LogRecord | undefined)?.message === "after reconnect"
      )
    );
    assertEquals(producer.status.dropped[1], 1);
    assertEquals(
      second.received.some((packet) =>
        (packet.record as LogRecord | undefined)?.message === "uncertain"
      ),
      false,
    );
    assertEquals(
      second.received.some((packet) =>
        Array.isArray(packet.dropped) && packet.dropped[1] === 1
      ),
      true,
    );
  } finally {
    await producer.close();
  }
});

Deno.test("stalled logging never grows past its byte budget and shutdown is bounded", async () => {
  const connection = new TestConnection();
  const producer = new LogProducer({
    ...options,
    connect: () => Promise.resolve(connection.connection()),
  });
  await until(() => producer.status.connected);
  connection.stall = true;
  producer.emit(record("in flight"));
  await new Promise((resolve) => setTimeout(resolve, 0));
  for (let index = 0; index < 10_000; index++) {
    producer.emit(record("x".repeat(500)));
  }
  producer.emit({ ...record("priority"), level: "ERROR" });
  assertEquals(producer.status.pendingBytes <= 128 * 1024, true);
  assertEquals(producer.status.pendingRecords <= 512, true);
  assertEquals(producer.status.dropped[1] > 9000, true);
  const started = Date.now();
  await producer.close();
  assertEquals(Date.now() - started < 1000, true);
  assertEquals(connection.closed, true);
  assertEquals(producer.status.pendingBytes, 0);
});

Deno.test("one pending connect is closed if it completes after producer shutdown", async () => {
  const connection = new TestConnection();
  let resolve!: (connection: Deno.Conn) => void;
  let connects = 0;
  const producer = new LogProducer({
    ...options,
    connect: () => {
      connects++;
      return new Promise((done) => resolve = done);
    },
  });
  await producer.close();
  resolve(connection.connection());
  await until(() => connection.closed);
  assertEquals(connects, 1);
});
