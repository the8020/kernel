import { assertEquals } from "../test/assert.ts";
import { TestLogSink } from "../test/logs.ts";
import type { ExecutionMetadata } from "../worker/contracts.ts";
import { decodePacket, encodePacket, INITIAL_POLICY } from "./protocol.ts";
import { LogQueue } from "./queue.ts";
import {
  bindWorkerRecord,
  REPORT_WEIGHT,
  WORKER_LOG_BYTES,
  WORKER_LOG_SLOTS,
  WorkerLogSender,
} from "./worker_channel.ts";

const metadata: ExecutionMetadata = {
  nodeId: "nod-0123456789",
  sandboxId: "sbx-0123456789",
  workerId: "wrk-0123456789",
  workloadType: "job",
  workloadId: "job-0123456789",
  ownerId: "module:acme/example/run",
  releaseId: "commit",
  entrypoint: "file:///job.ts",
  debuggerName: "job",
  databaseBackend: "sqlite",
  user: { userId: "user:alice", username: "alice" },
  origin: { type: "module", id: "acme/example/run" },
};

Deno.test("failed logging sends preserve credit and losses without throwing into the Worker", () => {
  let fail = true;
  const messages: Array<
    { type: string; frame?: Uint8Array; dropped?: number[] }
  > = [];
  const sender = new WorkerLogSender(
    {
      postMessage(message: unknown) {
        if (fail) throw new DOMException("detached port", "DataCloneError");
        messages.push(message as typeof messages[number]);
      },
    },
    metadata,
    () => undefined,
    INITIAL_POLICY,
  );
  sender.print("INFO", ["lost"]);
  sender.handle({ type: "log_policy", payload: INITIAL_POLICY });
  assertEquals(sender.pendingBytes, 0);
  assertEquals(sender.pendingRecords, 0);
  fail = false;
  sender.print("WARN", ["accepted"]);
  assertEquals(messages[0]!.dropped, [0, 1, 0, 0]);
  const firstWeight = messages[0]!.frame!.length + 128;
  assertEquals(sender.pendingBytes, firstWeight);
  fail = true;
  sender.print("ERROR", ["also lost"]);
  sender.handle({ type: "log_credit", payload: { bytes: firstWeight } });
  assertEquals(sender.pendingBytes, 0);
  assertEquals(sender.pendingRecords, 0);
  fail = false;
  sender.print("INFO", ["next"]);
  assertEquals(messages.at(-1)!.dropped, [0, 0, 0, 1]);
  const nextWeight = messages.at(-1)!.frame!.length + 128;
  fail = true;
  sender.print("WARN", ["lost before credit"]);
  fail = false;
  sender.handle({ type: "log_credit", payload: { bytes: nextWeight } });
  assertEquals(messages.at(-1)!.type, "log_dropped");
  assertEquals(messages.at(-1)!.dropped, [0, 0, 1, 0]);
  assertEquals(sender.pendingBytes, REPORT_WEIGHT);
  sender.handle({ type: "log_credit", payload: { bytes: REPORT_WEIGHT } });
  assertEquals(sender.pendingRecords, 0);
});

Deno.test("MessagePort credit bounds unprocessed prints and reserves warnings", () => {
  const messages: Array<
    { type: string; frame?: Uint8Array; dropped?: number[] }
  > = [];
  const sender = new WorkerLogSender(
    {
      postMessage: (message: unknown) =>
        messages.push(message as typeof messages[number]),
    },
    metadata,
    () => ({
      contextId: "ctx-0123456789",
      jobRunId: "job-0123456789",
      user: { username: "alice" },
    }),
    INITIAL_POLICY,
  );
  for (let i = 0; i < 10_000; i++) sender.print("INFO", ["flood " + i]);
  const lowCount = messages.length;
  assertEquals(lowCount <= WORKER_LOG_SLOTS - 16, true);
  sender.print("ERROR", ["priority retained"]);
  assertEquals(messages.length, lowCount + 1);
  assertEquals(sender.pendingBytes <= WORKER_LOG_BYTES, true);
  assertEquals(sender.pendingRecords <= WORKER_LOG_SLOTS, true);
  const last = messages.at(-1)!;
  const record = bindWorkerRecord(last.frame!, metadata);
  assertEquals(record.message, "priority retained");
  assertEquals(record.username, "alice");
  assertEquals(record.job_id, "job-0123456789");
  assertEquals(last.dropped![1]! > 9000, true);
  sender.handle({
    type: "log_credit",
    payload: { bytes: messages[0]!.frame!.length + 128 },
  });
  assertEquals(sender.pendingRecords, lowCount);
});

Deno.test("queue retains in-flight credit, sheds low priority and preserves accepted order", () => {
  const queue = new LogQueue(64 * 1024, 16 * 1024, 8);
  const frame = encodePacket({ text: "record" });
  for (let i = 0; i < 6; i++) assertEquals(queue.add(frame, "INFO"), true);
  const batch = queue.take(128 * 1024);
  assertEquals(queue.waiting, 0);
  assertEquals(queue.records, 6);
  assertEquals(queue.add(frame, "INFO"), false);
  assertEquals(queue.add(frame, "WARN"), true);
  assertEquals(queue.add(frame, "ERROR"), true);
  assertEquals(queue.add(frame, "ERROR"), false);
  batch.release();
  batch.release();
  assertEquals(queue.records, 2);
  const rest = queue.take(128 * 1024);
  assertEquals(rest.entries.map((entry) => entry.level), ["WARN", "ERROR"]);
  rest.release();
  assertEquals(queue.bytes, 0);
  assertEquals(queue.dropped, [0, 1, 0, 1]);
});

Deno.test("supervisor binds fixed identities and policies skip formatting", () => {
  const messages: Array<{ type: string; frame?: Uint8Array }> = [];
  const sender = new WorkerLogSender(
    {
      postMessage: (message: unknown) =>
        messages.push(message as typeof messages[number]),
    },
    metadata,
    () => undefined,
    INITIAL_POLICY,
  );
  sender.handle({
    type: "log_policy",
    payload: { revision: 2, enabled: false, level: "info" },
  });
  let accessed = false;
  sender.print("ERROR", [
    new Proxy({}, {
      ownKeys() {
        accessed = true;
        return [];
      },
    }),
  ]);
  assertEquals(accessed, false);
  assertEquals(messages.length, 1);
  sender.handle({
    type: "log_policy",
    payload: { revision: 3, enabled: true, level: "warn" },
  });
  sender.print("INFO", ["filtered"]);
  sender.print("WARN", ["record"]);
  const packet = decodePacket(messages.at(-1)!.frame!) as {
    record: Record<string, unknown>;
  };
  packet.record.worker_id = "wrk-other00000";
  packet.record.object = "job:other/run";
  packet.record.node_id = "nod-other00000";
  const bound = bindWorkerRecord(encodePacket(packet), metadata);
  assertEquals(bound.worker_id, metadata.workerId);
  assertEquals(bound.object, "module:acme/example/run");
  assertEquals(bound.node_id, metadata.nodeId);
  assertEquals(bound.username, undefined);
  const sink = new TestLogSink();
  sink.emit(bound);
  assertEquals(sink.records.length, 1);
});
