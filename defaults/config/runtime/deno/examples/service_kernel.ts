import { kernel } from "@the8020/kernel";
import type { ServiceEntrypoint } from "../worker/contracts.ts";

export const fetch: ServiceEntrypoint = async (request) => {
  const path = new URL(request.url).pathname;
  if (path === "/logout") {
    return Response.json(await kernel.auth.logoutCurrent());
  }
  if (path === "/admin") {
    return Response.json(await kernel.admin.execute("service.list"));
  }
  if (path === "/database-query") {
    return Response.json(await kernel.database.query("SELECT $1", [7]));
  }
  if (path === "/database-execute") {
    return Response.json(
      await kernel.database.execute("DELETE FROM example WHERE id = $1", [7]),
    );
  }
  const credentials = await request.json() as {
    username: string;
    password: string;
  };
  return Response.json(await kernel.auth.bootstrapLogin(credentials));
};
