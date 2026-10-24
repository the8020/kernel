import { kernel } from "@the8020/kernel";
import { context } from "@the8020/context";

export async function authenticate(
  _request: Request,
  input: { username: string; deny?: boolean; fail?: boolean },
) {
  if (context.authenticated) throw new Error("premature authentication");
  let restricted = false;
  try {
    await kernel.admin.execute("must-not-run");
  } catch {
    restricted = true;
  }
  if (!restricted) throw new Error("authentication gained native authority");
  const digest = await kernel.crypto.passwordDigest(
    input.username,
    new TextEncoder().encode(
      kernel.execution.optionalSecret("password") ?? "fixture",
    ),
    new Uint8Array(16),
  );
  if (digest.length !== 32) throw new Error("password digest unavailable");
  console.log("policy secret", kernel.execution.optionalSecret("password"));
  if (input.fail) throw new Error("policy unavailable");
  await kernel.database.execute("SELECT authentication", [input.username], {
    returnRows: true,
  });
  if (input.deny) return new Response("Denied", { status: 401 });
  return { userId: `user:${input.username}`, username: input.username };
}

export default {
  async fetch(_request: Request, options: { meta: Record<string, unknown> }) {
    if (
      options.meta.authentication !== undefined ||
      kernel.execution.optionalSecret("password") !== undefined
    ) throw new Error("credential leak");
    await kernel.admin.execute("handler");
    return Response.json(context.current);
  },
};
