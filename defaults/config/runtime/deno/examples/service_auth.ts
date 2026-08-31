import type { ServiceEntrypoint } from "../worker/contracts.ts";

export const fetch: ServiceEntrypoint = (request, context) =>
  Response.json({
    auth: context.meta.auth,
    execution: context.meta.execution,
    internalHeaderVisible: [...request.headers.keys()].some((name) =>
      name.startsWith("x-80-20-internal-")
    ),
  });
