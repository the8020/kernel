import { assertEquals, assertRejects } from "../test/assert.ts";
import { OMITTED } from "../logging/protocol.ts";
import { captureOutput, diagnosticText } from "./subprocess.ts";

function chunks(values: Uint8Array[]): ReadableStream<Uint8Array> {
  return new ReadableStream({
    pull(controller) {
      const value = values.shift();
      if (value) controller.enqueue(value);
      else controller.close();
    },
  });
}

Deno.test("subprocess graph is exact and rejects overflow while reading", async () => {
  const bytes = new TextEncoder().encode('{"modules":[]}');
  const output = await captureOutput(
    chunks([bytes.subarray(0, 5), bytes.subarray(5)]),
    bytes.length,
  );
  assertEquals(output.bytes, bytes);
  assertEquals(output.truncated, false);
  let cancelled = false;
  await assertRejects(
    () =>
      captureOutput(
        new ReadableStream({
          pull(controller) {
            controller.enqueue(bytes);
          },
          cancel() {
            cancelled = true;
          },
        }),
        16,
      ),
    RangeError,
    "exceeds 16 bytes",
  );
  assertEquals(cancelled, true);
});

Deno.test("subprocess diagnostics stay bounded and forward every byte", async () => {
  const encoder = new TextEncoder();
  const input = encoder.encode("BEGIN" + "日本語💡".repeat(8000) + "END");
  for (const chunkSize of [1, 13, 65536]) {
    let offset = 0;
    let forwarded = 0;
    const stream = new ReadableStream<Uint8Array>({
      pull(controller) {
        if (offset === input.length) return controller.close();
        const chunk = input.subarray(offset, offset + chunkSize);
        offset += chunk.length;
        controller.enqueue(chunk);
      },
    });
    const output = await captureOutput(stream, 64, true, (chunk) => {
      assertEquals(chunk, input.subarray(forwarded, forwarded + chunk.length));
      forwarded += chunk.length;
      return Promise.resolve();
    });
    assertEquals(output.bytes.length, 64);
    assertEquals(output.truncated, true);
    assertEquals(forwarded, input.length);
    const text = diagnosticText(output);
    assertEquals(text.startsWith("BEGIN"), true);
    assertEquals(text.endsWith("END"), true);
    assertEquals(text.includes(OMITTED), true);
    assertEquals(text.includes("�"), false);
  }
});

Deno.test("subprocess diagnostics preserve ordinary fragments and partial EOF", async () => {
  const text = "TypeError: 日本語💡\n cause: failure";
  const bytes = new TextEncoder().encode(text);
  const output = await captureOutput(
    chunks(Array.from(bytes, (value) => new Uint8Array([value]))),
    128,
    true,
  );
  assertEquals(diagnosticText(output), text);
  assertEquals(output.truncated, false);
});
