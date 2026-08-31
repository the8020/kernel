import { defineService } from "@the8020/http";

export default defineService()
  .websocket("/echo/:channel", async ({ params, meta, socket }) => {
    socket.send(`ready:${params.channel}:${meta.requestId}:${socket.protocol}`);
    while (true) {
      const event = await socket.receive();
      if (event.type === "close") return;
      if (typeof event.data === "string") {
        socket.send(`echo:${event.data}`);
      } else {
        socket.send(event.data);
      }
    }
  });
