import type { ServiceHandler } from "../contracts.ts";
import { kernel } from "@the8020/kernel";
import { context } from "@the8020/context";

export default {
  fetch: () => Response.json(context.current),
  connectWebSocket(_request, _context, socket) {
    void (async () => {
      socket.send(JSON.stringify(context.current));
      const event = await socket.receive();
      if (event.type === "message") {
        await kernel.database.execute("SELECT handler", [], {
          returnRows: true,
        });
        socket.send(JSON.stringify(context.current));
      }
    })().then(
      () => socket.close(1000, "handler completed"),
      () => socket.close(1011, "handler failed"),
    );
    return new Response(null, {
      status: 204,
      headers: { "the8020-internal-websocket-accepted": "true" },
    });
  },
} satisfies ServiceHandler;
