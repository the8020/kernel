const text = "compressible service response ".repeat(1_024);
let pending: ReadableStreamDefaultController<Uint8Array> | undefined;

export function fetch(request: Request): Response {
  const path = new URL(request.url).pathname;
  if (path === "/finish") {
    pending?.close();
    pending = undefined;
    return new Response(null, { status: 204 });
  }
  const headers = new Headers({
    "content-type": "text/plain",
    "etag": '"example"',
    "vary": "Origin",
    "cache-control": "private",
  });
  if (path === "/no-transform") headers.append("cache-control", "no-transform");
  let body = new Response(text).body!;
  if (path === "/encoded") {
    body = body.pipeThrough(new CompressionStream("gzip"));
    headers.set("content-encoding", "gzip");
  } else if (path === "/stream") {
    body = new ReadableStream({
      start(controller) {
        pending = controller;
        controller.enqueue(new TextEncoder().encode(text));
      },
      cancel() {
        pending = undefined;
      },
    });
  } else {
    headers.set("content-length", String(text.length));
    if (path === "/range") {
      headers.set("content-range", `bytes 0-${text.length - 1}/${text.length}`);
    }
  }
  return new Response(body, { status: path === "/range" ? 206 : 200, headers });
}
