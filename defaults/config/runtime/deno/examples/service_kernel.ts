import { kernel } from "@the8020/kernel";
import type { ServiceEntrypoint } from "../worker/contracts.ts";

export const fetch: ServiceEntrypoint = async (request) => {
  const path = new URL(request.url).pathname;
  if (path === "/logout") {
    return Response.json(await kernel.auth.logoutCurrent());
  }
  if (path === "/admin") {
    return Response.json(await kernel.admin.execute("kernel.status"));
  }
  if (path === "/database-query") {
    return Response.json(
      await kernel.database.execute("SELECT $1", [7], { returnRows: true }),
    );
  }
  if (path === "/database-execute") {
    return Response.json(
      await kernel.database.execute("DELETE FROM example WHERE id = $1", [7]),
    );
  }
  if (path === "/database-stream") {
    let complete = false;
    return new Response(
      new ReadableStream({
        async pull(controller) {
          if (complete) {
            controller.close();
            return;
          }
          complete = true;
          const result = await kernel.database.execute(
            "SELECT $1",
            [9],
            { returnRows: true },
          );
          controller.enqueue(new TextEncoder().encode(JSON.stringify(result)));
        },
      }),
    );
  }
  const credentials = await request.json() as {
    username: string;
    password: string;
  };
  return Response.json(await kernel.auth.login(credentials));
};
