import type { ServiceEntrypoint } from "../worker/contracts.ts";

export const fetch: ServiceEntrypoint = (request, context) =>
  Response.json({
    auth: context.meta.auth,
    user: context.meta.user,
    execution: context.meta.execution,
    internalHeaderVisible: [...request.headers.keys()].some((name) =>
      name.startsWith("the8020-internal-")
    ),
  });
