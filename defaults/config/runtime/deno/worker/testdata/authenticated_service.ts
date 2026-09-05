import { defineService } from "@the8020/http";
import { kernel } from "@the8020/kernel";
import { context } from "@the8020/context";

export default defineService()
  .get("/", {}, () => Response.json(context.current))
  .websocket("/", async ({ socket }) => {
    socket.send(JSON.stringify(context.current));
    const event = await socket.receive();
    if (event.type === "message") {
      await kernel.database.execute("SELECT handler", [], { returnRows: true });
      socket.send(JSON.stringify(context.current));
    }
  });
