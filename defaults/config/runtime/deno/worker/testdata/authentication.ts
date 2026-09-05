import { kernel } from "@the8020/kernel";
import { context } from "@the8020/context";

// Runtime regression fixture. The real composition uses the users package.
export async function authenticate(): Promise<Response | undefined> {
  if (context.authenticated) {
    throw new Error("identity published before policy approval");
  }
  const result = await kernel.database.execute("SELECT authentication", [], {
    returnRows: true,
  });
  if (result.rows[0]?.[0] !== true) {
    return new Response("Denied", { status: 401 });
  }
}
