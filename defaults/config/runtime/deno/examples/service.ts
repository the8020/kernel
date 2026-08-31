export async function fetch(
  request: Request,
  context: { signal: AbortSignal },
): Promise<Response> {
  const received = request.body === null ? "" : await request.text();
  if (new URL(request.url).pathname === "/wait") {
    await new Promise<void>((resolve, reject) => {
      const timeout = setTimeout(resolve, 10_000);
      context.signal.addEventListener("abort", () => {
        clearTimeout(timeout);
        reject(context.signal.reason);
      }, { once: true });
    });
  }
  if (new URL(request.url).pathname === "/slow") {
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  const sse = new URL(request.url).pathname === "/sse";
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      if (sse) {
        controller.enqueue(new TextEncoder().encode("event: ready\n"));
        controller.enqueue(new TextEncoder().encode("data: streamed\n\n"));
        controller.close();
        return;
      }
      controller.enqueue(
        new TextEncoder().encode(
          `${request.method}:${new URL(request.url).pathname}:`,
        ),
      );
      controller.enqueue(new TextEncoder().encode(`streamed:${received}`));
      controller.close();
    },
  });
  return new Response(stream, {
    status: 201,
    headers: {
      "content-type": sse ? "text/event-stream" : "text/plain",
    },
  });
}
