import { assertEquals } from "../test/assert.ts";
import { captureRecord } from "./capture.ts";
import {
  formatAttributes,
  formatValues,
  safeText,
  truncateText,
} from "./format.ts";
import { encodePacket, MAX_FRAME, OMITTED } from "./protocol.ts";

Deno.test("log formatting bounds UTF-8 and controls while preserving both ends", () => {
  const huge = "START😀\n" + "🙂\u0000".repeat(100_000) + "\nEND🎯";
  const text = formatValues([huge]);
  assertEquals(text.startsWith("START😀\n"), true);
  assertEquals(text.endsWith("\nEND🎯"), true);
  assertEquals(text.includes(OMITTED), true);
  assertEquals(
    new TextEncoder().encode(JSON.stringify(text)).length < 16 * 1024,
    true,
  );
  assertEquals(text.includes("�"), false);
  assertEquals(truncateText("hello", 40), "hello");
});

Deno.test("log values retain Error stacks and causes, skip getters and handle cycles", () => {
  let invoked = false;
  const input: Record<string, unknown> = {
    value: 1,
    accessToken: "never-store",
  };
  input.self = input;
  Object.defineProperty(input, "getter", {
    enumerable: true,
    get() {
      invoked = true;
      throw new Error("getter executed");
    },
  });
  input.toJSON = () => {
    invoked = true;
    throw new Error("toJSON executed");
  };
  const error = new Error("outer failure", {
    cause: new TypeError("inner failure"),
  });
  const text = formatValues([input, error]);
  assertEquals(invoked, false);
  assertEquals(text.includes("[circular]"), true);
  assertEquals(text.includes("[accessor]"), true);
  assertEquals(text.includes("never-store"), false);
  assertEquals(text.includes("outer failure"), true);
  assertEquals(text.includes("Caused by: TypeError: inner failure"), true);
  assertEquals(text.includes("format_test.ts"), true);
});

Deno.test("secure values are redacted before truncation including overlapping boundary fragments", () => {
  const long = "PRIVATE_START_" + "s".repeat(500_000) + "_PRIVATE_END";
  const secrets = { short: "hide-me", long };
  const text = safeText("head " + long + " tail hide-me", 2048, secrets);
  assertEquals(text.startsWith("head "), true);
  assertEquals(text.endsWith(" tail [redacted]"), true);
  assertEquals(text.includes("PRIVATE"), false);
  assertEquals(text.includes("ssss"), false);
  assertEquals(text.includes("hide-me"), false);
  assertEquals(text.includes(OMITTED), true);
  assertEquals(
    formatValues([{ value: "hide-me" }], 512, secrets),
    '{"value":"[redacted]"}',
  );
  assertEquals(
    formatAttributes({ value: "hide-me", password: "different" }, secrets),
    { value: "[redacted]", password: "[redacted]" },
  );
});

Deno.test("capture preserves the active username and invocation before forwarding", () => {
  const record = captureRecord(
    {
      nodeId: "nod-0123456789",
      sandboxId: "sbx-0123456789",
      workerId: "wrk-0123456789",
      workloadType: "service",
      workloadId: "srv-0123456789",
      ownerId: "service:acme/billing/invoice",
      releaseId: "sha256:unchanged",
      entrypoint: "file:///invoice.ts",
      debuggerName: "invoice",
      databaseBackend: "sqlite",
      user: { username: "system", userId: "user:system" },
      origin: { type: "service", id: "acme/billing/invoice" },
    },
    {
      contextId: "ctx-0123456789",
      parentContextId: "ctx-abcdefghij",
      user: { username: "alice" },
      secrets: { password: "redact-this" },
    },
    "ERROR",
    [new Error("redact-this\n" + "abc".repeat(100_000) + " last")],
    { password: "hidden" },
  );
  assertEquals(record.username, "alice");
  assertEquals(record.context_id, "ctx-0123456789");
  assertEquals(record.parent_context_id, "ctx-abcdefghij");
  assertEquals(record.object, "service:acme/billing/invoice");
  assertEquals(JSON.stringify(record).includes("redact-this"), false);
  assertEquals(encodePacket({ record }).length <= MAX_FRAME + 4, true);
});
