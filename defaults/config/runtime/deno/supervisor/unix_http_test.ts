import { assertEquals } from "@std/assert";
import { readHTTPResponse } from "./unix_http.ts";

Deno.test("Unix HTTP response completes at the declared body length", async () => {
  const response = new TextEncoder().encode(
    'HTTP/1.0 200 OK\r\nContent-Type: application/json\r\nContent-Length: 11\r\n\r\n{"ok":true}',
  );
  let offset = 0;
  const reader = {
    async read(buffer: Uint8Array): Promise<number | null> {
      if (offset === response.length) return await new Promise(() => {});
      const count = Math.min(7, response.length - offset);
      buffer.set(response.subarray(offset, offset + count));
      offset += count;
      return count;
    },
  };

  const raw = await Promise.race([
    readHTTPResponse(reader),
    new Promise<never>((_, reject) =>
      setTimeout(() => reject(new Error("response waited for EOF")), 100)
    ),
  ]);
  assertEquals(raw, response);
});
