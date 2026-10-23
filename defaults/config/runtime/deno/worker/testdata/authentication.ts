import { kernel } from "@the8020/kernel";
import { context } from "@the8020/context";

// Runtime regression fixture. The real composition uses the users package.
export async function authenticate(
  _request: Request,
  input: { sub: string },
): Promise<Response | { userId: string; username: string }> {
  if (context.authenticated) {
    throw new Error("identity published before policy approval");
  }
  const result = await kernel.database.execute("SELECT authentication", [], {
    returnRows: true,
  });
  if (result.rows[0]?.[0] !== true) {
    return new Response("Denied", { status: 401 });
  }
  return { userId: input.sub, username: input.sub.slice(5) };
}
