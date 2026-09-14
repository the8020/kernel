import type { ServiceHandler } from "../worker/contracts.ts";

export default {
  fetch() {
    return new Response("not found", { status: 404 });
  },
  connectWebSocket(request, { meta }, socket) {
    const match = new URL(request.url).pathname.match(/^\/echo\/([^/]+)$/);
    if (match === null) return new Response("not found", { status: 404 });
    void (async () => {
      socket.send(`ready:${match[1]}:${meta.contextId}:${socket.protocol}`);
      while (true) {
        const event = await socket.receive();
        if (event.type === "close") return;
        if (event.data === "close") {
          socket.close(1000, "server closed");
          return;
        }
        socket.send(
          typeof event.data === "string" ? `echo:${event.data}` : event.data,
        );
      }
    })().catch(() => socket.close(1011, "handler failed"));
    return new Response(null, {
      status: 204,
      headers: { "the8020-internal-websocket-accepted": "true" },
    });
  },
} satisfies ServiceHandler;
