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
  const credentials = await request.json() as {
    username: string;
    password: string;
  };
  return Response.json(await kernel.auth.bootstrapLogin(credentials));
};
