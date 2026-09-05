import { context } from "@the8020/context";

export function fetch(): Response {
  return Response.json(context.current);
}
