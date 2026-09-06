import { assertEquals, assertRejects } from "../test/assert.ts";
import {
  followLogs,
  formatLogRecord,
  type LogPage,
  type LogQuery,
} from "./logs.ts";

function page(overrides: Partial<LogPage> = {}): LogPage {
  return {
    state: "ok",
    records: [],
    scanned_bytes: 0,
    more: false,
    cursor: "next",
    ...overrides,
  };
}

Deno.test("log follow advances empty pages and replaces the starting position", async () => {
  const calls: LogQuery[] = [];
  const abort = new AbortController();
  const pages = [
    page({ more: true }),
    page({ state: "expired", reason: "retention" }),
  ];
  const follow = followLogs(
    (query, signal) => {
      assertEquals(signal, abort.signal);
      calls.push(query);
      return Promise.resolve(pages.shift()!);
    },
    { position: "before-job", node_id: "nod-0123456789", username: "alice" },
    { signal: abort.signal },
  );
  const first = await follow.next();
  assertEquals(first.value!.more, true);
  const second = await follow.next();
  assertEquals(second.value!.state, "expired");
  assertEquals((await follow.next()).done, true);
  assertEquals(calls.length, 2);
  assertEquals(calls[0]!.position, "before-job");
  assertEquals(calls[1]!.cursor, "next");
  assertEquals(Object.hasOwn(calls[1]!, "position"), false);
  for (const call of calls) {
    assertEquals(call.node_id, "nod-0123456789");
    assertEquals(call.username, "alice");
  }
});

Deno.test("log follow defaults to tail and cancels its idle wait", async () => {
  const abort = new AbortController();
  let calls = 0;
  const follow = followLogs(
    (query) => {
      calls++;
      assertEquals(query, { tail: true });
      return Promise.resolve(page());
    },
    {},
    { signal: abort.signal, intervalMs: 60000 },
  );
  await follow.next();
  const waiting = follow.next();
  abort.abort();
  assertEquals((await waiting).done, true);
  assertEquals(calls, 1);
});

Deno.test("log follow retains one request and propagates cancellation", async () => {
  const abort = new AbortController();
  let calls = 0;
  const follow = followLogs(
    (_query, signal) => {
      calls++;
      return new Promise((_, reject) =>
        signal!.addEventListener("abort", () => reject(signal!.reason), {
          once: true,
        })
      );
    },
    {},
    { signal: abort.signal },
  );
  const rejected = assertRejects(
    () => follow.next(),
    Error,
    "view closed",
  );
  abort.abort(new DOMException("view closed", "AbortError"));
  await rejected;
  assertEquals(calls, 1);
});

Deno.test("readable log formatting preserves typed identities and error stacks", () => {
  const text = formatLogRecord({
    time: "2026-09-06T12:34:56.789Z",
    level: "ERROR",
    source: "deno",
    component: "worker",
    node_id: "nod-0123456789",
    sandbox_id: "sbx-0123456789",
    worker_id: "wrk-0123456789",
    context_id: "ctx-0123456789",
    parent_context_id: "ctx-abcdefghij",
    username: "alice",
    object: "program:acme/invoices/create",
    message: "Error: rejected\n  at create.ts:3",
    segment: "segment",
    offset: 0,
  });
  assertEquals(
    text,
    "2026-09-06T12:34:56.789Z ERROR deno/worker [nod-0123456789 sbx-0123456789 wrk-0123456789 ctx-0123456789 parent:ctx-abcdefghij program:acme/invoices/create user:alice] Error: rejected\n  at create.ts:3",
  );
});
