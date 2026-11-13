import type { KernelBridge } from "../kernel/bridge.ts";
import {
  type AuthContext,
  canonicalExecutionUser,
  type ServiceRequestMetadata,
} from "./contracts.ts";

type Authentication = NonNullable<ServiceRequestMetadata["authentication"]>;

/** The kernel already chose the method; services see it without credentials. */
function authenticationMethod(
  authentication: Authentication,
): Pick<AuthContext, "method" | "tokenSource"> {
  if (authentication.approved === true) return { method: "native" };
  const input = authentication.input as
    | { method?: unknown; fromCookie?: unknown }
    | undefined;
  if (input?.method === "password" || input?.method === "certificate") {
    return { method: input.method };
  }
  if (input?.method === "token") {
    return {
      method: "token",
      tokenSource: input.fromCookie === true ? "cookie" : "header",
    };
  }
  return {};
}

// Only the composition-selected hook sees credentials; the handler gets its approved scope.
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
  try {
    let user = meta.user;
    if (authentication.approved !== true) {
      const result = await bridge.withRequest(
        { ...meta, authentication },
        async () => {
          const hook = await import(authentication.module);
          return await hook.authenticate(
            request,
            authentication.input,
            authentication.unauthenticated,
          );
        },
        signal,
      );
      if (result instanceof Response) return { meta, response: result };
      user = canonicalExecutionUser(result);
    }
    if (
      authentication.principal !== undefined &&
      authentication.principal !== user.userId
    ) {
      throw new TypeError("authentication principal mismatch");
    }
    const approved: ServiceRequestMetadata = {
      ...meta,
      user,
      auth: {
        authenticated: true,
        realm: "user",
        ...user,
        ...authenticationMethod(authentication),
      },
    };
    signal.throwIfAborted();
    if (
      authentication.approved !== true &&
      !await bridge.authenticate(approved, signal)
    ) {
      return {
        meta,
        response: new Response("Persistent execution lost", {
          status: 409,
          headers: { "cache-control": "no-store" },
        }),
      };
    }
    return { meta: approved };
  } catch {
    return {
      meta,
      response: new Response("Authentication unavailable", {
        status: 503,
        headers: { "cache-control": "no-store" },
      }),
    };
  }
}
