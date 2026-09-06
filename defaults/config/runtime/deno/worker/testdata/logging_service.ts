import { context } from "@the8020/context";

export async function fetch(request: Request): Promise<Response> {
  const delay = Number(new URL(request.url).searchParams.get("delay") ?? 0);
  console.log("begin", context.username);
  await new Promise((resolve) => setTimeout(resolve, delay));
  console.warn("end", context.username);
  return Response.json({
    username: context.username,
    contextId: context.contextId,
  });
}
