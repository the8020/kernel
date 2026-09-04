const encoder = new TextEncoder();
const decoder = new TextDecoder();

async function writeAll(
  connection: Deno.Conn,
  value: Uint8Array,
): Promise<void> {
  let offset = 0;
  while (offset < value.length) {
    offset += await connection.write(value.subarray(offset));
  }
}

interface Reader {
  read(buffer: Uint8Array): Promise<number | null>;
}

function concatenate(chunks: Uint8Array[], length: number): Uint8Array {
  const result = new Uint8Array(length);
  let offset = 0;
  for (const chunk of chunks) {
    result.set(chunk, offset);
    offset += chunk.length;
  }
  return result;
}

function declaredBodyLength(
  value: Uint8Array,
  boundary: number,
): number | undefined {
  const lines = decoder.decode(value.subarray(0, boundary)).split("\r\n");
  lines.shift();
  for (const line of lines) {
    const separator = line.indexOf(":");
    if (
      separator > 0 &&
      line.slice(0, separator).trim().toLowerCase() === "content-length"
    ) {
      const length = Number(line.slice(separator + 1).trim());
      if (!Number.isSafeInteger(length) || length < 0) {
        throw new Error("kernel returned an invalid HTTP content length");
      }
      return length;
    }
  }
  return undefined;
}

export async function readHTTPResponse(
  connection: Reader,
): Promise<Uint8Array> {
  const chunks: Uint8Array[] = [];
  let length = 0;
  let expectedLength: number | undefined;
  const buffer = new Uint8Array(16 * 1024);
  while (true) {
    const count = await connection.read(buffer);
    if (count === null) break;
    const chunk = buffer.slice(0, count);
    chunks.push(chunk);
    length += count;
    if (expectedLength === undefined) {
      const prefix = concatenate(chunks, length);
      const boundary = headerBoundary(prefix);
      if (boundary >= 0) {
        const bodyLength = declaredBodyLength(prefix, boundary);
        if (bodyLength !== undefined) {
          expectedLength = boundary + 4 + bodyLength;
        }
      } else if (length > 64 * 1024) {
        throw new Error("kernel HTTP response headers are too large");
      }
    }
    if (expectedLength !== undefined && length >= expectedLength) break;
  }
  return concatenate(chunks, length);
}

function headerBoundary(value: Uint8Array): number {
  for (let index = 0; index + 3 < value.length; index++) {
    if (
      value[index] === 13 && value[index + 1] === 10 &&
      value[index + 2] === 13 && value[index + 3] === 10
    ) return index;
  }
  return -1;
}

/** Send one bounded HTTP request over a fresh Unix-socket connection. */
export async function postUnixHTTP(
  socketPath: string,
  path: string,
  token: string,
  body: unknown,
  signal?: AbortSignal,
): Promise<Response> {
  if (!socketPath.startsWith("/") || !path.startsWith("/")) {
    throw new TypeError(
      "absolute kernel socket and request paths are required",
    );
  }
  const encodedBody = encoder.encode(JSON.stringify(body));
  const head = encoder.encode(
    `POST ${path} HTTP/1.0\r\nHost: kernel\r\nAuthorization: Bearer ${token}\r\nContent-Type: application/json\r\nContent-Length: ${encodedBody.length}\r\nConnection: close\r\n\r\n`,
  );
  const request = new Uint8Array(head.length + encodedBody.length);
  request.set(head);
  request.set(encodedBody, head.length);
  if (signal?.aborted) {
    throw signal.reason ?? new DOMException("Aborted", "AbortError");
  }
  const connection = await Deno.connect({
    transport: "unix",
    path: socketPath,
  });
  const abort = (): void => {
    try {
      connection.close();
    } catch {
      // Closing an already completed request is harmless.
    }
  };
  signal?.addEventListener("abort", abort, { once: true });
  try {
    if (signal?.aborted) {
      throw signal.reason ?? new DOMException("Aborted", "AbortError");
    }
    await writeAll(connection, request);
    const raw = await readHTTPResponse(connection);
    const boundary = headerBoundary(raw);
    if (boundary < 0) {
      throw new Error("kernel returned an invalid HTTP response");
    }
    const lines = decoder.decode(raw.subarray(0, boundary)).split("\r\n");
    const match = /^HTTP\/1\.[01] ([0-9]{3})(?: |$)/.exec(lines.shift() ?? "");
    if (match === null) {
      throw new Error("kernel returned an invalid HTTP status");
    }
    const headers = new Headers();
    for (const line of lines) {
      const separator = line.indexOf(":");
      if (separator <= 0) {
        throw new Error("kernel returned an invalid HTTP header");
      }
      headers.append(
        line.slice(0, separator).trim(),
        line.slice(separator + 1).trim(),
      );
    }
    const responseBody = raw.slice(boundary + 4);
    const declaredLength = headers.get("content-length");
    if (
      declaredLength !== null && Number(declaredLength) !== responseBody.length
    ) {
      throw new Error("kernel returned an incomplete HTTP response");
    }
    return new Response(responseBody, { status: Number(match[1]), headers });
  } catch (error) {
    if (signal?.aborted) {
      throw signal.reason ?? new DOMException("Aborted", "AbortError");
    }
    throw error;
  } finally {
    signal?.removeEventListener("abort", abort);
    abort();
  }
}
