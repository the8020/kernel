import type { KernelBridge } from "../kernel/bridge.ts";
import type { ServiceRequestMetadata } from "./contracts.ts";

// The composition-selected package owns authentication policy. This adapter
// supplies its ordinary request scope and publishes identity only on approval.
export async function authenticateRequest(
  request: Request,
  metadata: ServiceRequestMetadata,
  bridge: KernelBridge,
  signal: AbortSignal,
): Promise<{ meta: ServiceRequestMetadata; response?: Response }> {
  const { authentication, ...rest } = metadata;
  const meta: ServiceRequestMetadata = {
    ...rest,
    auth: { authenticated: false },
  };
  if (authentication === undefined) return { meta };
  if (authentication.claims.sub !== meta.user.userId) {
    throw new TypeError("verified token does not match execution principal");
  }
  const response = await bridge.withRequest(meta, async () => {
    const hook = await import(authentication.module);
    if (typeof hook.authenticate !== "function") {
      throw new TypeError("authentication module must export authenticate");
    }
    return await hook.authenticate(
      request,
      Object.freeze({ ...authentication.claims }),
      authentication.unauthenticated,
    );
  }, signal);
  if (response instanceof Response) return { meta, response };
  if (response !== undefined) {
    throw new TypeError(
      "authentication must return a rejection Response or undefined",
    );
  }
  return {
    meta: {
      ...meta,
      auth: {
        authenticated: true,
        realm: "user",
        userId: meta.user.userId,
        username: meta.user.username,
      },
    },
  };
}
