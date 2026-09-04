export async function fetch(request: Request): Promise<Response> {
  const target = new URL(request.url).searchParams.get("target");
  if (target === null) {
    return new Response("target is required", { status: 400 });
  }
  return new Response(await (await globalThis.fetch(target)).text());
}
