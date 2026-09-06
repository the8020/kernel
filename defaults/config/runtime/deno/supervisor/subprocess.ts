import { OMITTED } from "../logging/protocol.ts";

const diagnosticLimit = 16 * 1024;

// Consume one chunk at a time. Keep graph JSON exact, but retain both ends of
// diagnostics while continuing to drain and forward their original raw bytes.
export async function captureOutput(
  stream: ReadableStream<Uint8Array>,
  limit: number,
  keepEnds = false,
  forward?: (chunk: Uint8Array) => Promise<void>,
): Promise<{ bytes: Uint8Array; truncated: boolean }> {
  let buffer = new Uint8Array(Math.min(limit, 4096));
  let used = 0;
  let truncated = false;
  for await (const chunk of stream) {
    if (forward) await forward(chunk);
    if (chunk.length > limit - used && !keepEnds) {
      throw new RangeError(`subprocess output exceeds ${limit} bytes`);
    }
    const copied = Math.min(chunk.length, limit - used);
    if (used + copied > buffer.length) {
      const grown = new Uint8Array(
        Math.min(limit, Math.max(used + copied, buffer.length * 2)),
      );
      grown.set(buffer);
      buffer = grown;
    }
    buffer.set(chunk.subarray(0, copied), used);
    used += copied;
    const rest = chunk.subarray(copied);
    if (rest.length > 0) {
      truncated = true;
      const half = Math.floor(limit / 2);
      const tailLength = limit - half;
      if (rest.length >= tailLength) {
        buffer.set(rest.subarray(rest.length - tailLength), half);
      } else {
        buffer.copyWithin(half, half + rest.length);
        buffer.set(rest, limit - rest.length);
      }
    }
  }
  return { bytes: buffer.subarray(0, used), truncated };
}

export function diagnosticText(
  output: { bytes: Uint8Array; truncated: boolean },
): string {
  if (!output.truncated) return new TextDecoder().decode(output.bytes).trim();
  const middle = Math.floor(output.bytes.length / 2);
  // Do not turn the two deliberately cut UTF-8 boundaries into replacement
  // characters. Invalid original bytes still use the decoder's replacement.
  const head = new TextDecoder().decode(output.bytes.subarray(0, middle), {
    stream: true,
  });
  let tailStart = middle;
  while (
    tailStart < output.bytes.length &&
    (output.bytes[tailStart]! & 0xc0) === 0x80
  ) tailStart++;
  return (head + OMITTED +
    new TextDecoder().decode(output.bytes.subarray(tailStart))).trim();
}

export async function runDeno(
  args: string[],
  stdoutLimit: number,
): Promise<{ success: boolean; stdout: Uint8Array; stderr: string }> {
  const child = new Deno.Command(Deno.execPath(), {
    args,
    stdout: stdoutLimit === 0 ? "null" : "piped",
    stderr: "piped",
  }).spawn();
  let rawAvailable = true;
  const stderr = captureOutput(
    child.stderr,
    diagnosticLimit,
    true,
    async (chunk) => {
      if (!rawAvailable) return;
      try {
        let offset = 0;
        while (offset < chunk.length) {
          const written = await Deno.stderr.write(chunk.subarray(offset));
          if (written === 0) throw new Error("closed native diagnostic stream");
          offset += written;
        }
      } catch {
        // A missing raw reader must not wedge module validation or graph reads.
        rawAvailable = false;
      }
    },
  );
  const stdout = stdoutLimit === 0
    ? Promise.resolve({ bytes: new Uint8Array(), truncated: false })
    : captureOutput(child.stdout, stdoutLimit);
  const pending = [child.status, stdout, stderr] as const;
  try {
    const [status, out, err] = await Promise.all(pending);
    return {
      success: status.success,
      stdout: out.bytes,
      stderr: diagnosticText(err),
    };
  } catch (error) {
    try {
      child.kill("SIGKILL");
    } catch {
      // The failed reader may already have caused this exact child to exit.
    }
    await Promise.allSettled(pending);
    throw error;
  }
}
