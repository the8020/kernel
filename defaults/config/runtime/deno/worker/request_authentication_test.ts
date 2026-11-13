import { assertEquals } from "../test/assert.ts";
import type { KernelBridge } from "../kernel/bridge.ts";
import type { ServiceRequestMetadata } from "./contracts.ts";
import { authenticateRequest } from "./request_authentication.ts";

const bridge = {
  withRequest: (_meta: unknown, run: () => Promise<unknown>) => run(),
  authenticate: () => Promise.resolve(true),
} as unknown as KernelBridge;

const meta: ServiceRequestMetadata = {
  contextId: "ctx-0000000001",
  serviceId: "example/auth/service",
  serviceGeneration: 1,
  canonicalBasePath: "/example/auth/service",
  originalUrl: "https://example.test/",
  client: { ipAddress: "127.0.0.1", networkScope: "loopback" },
  execution: {
    nodeId: "nod-0000000001",
    sandboxId: "sbx-0000000001",
    workerId: "wrk-0000000001",
  },
  user: { userId: "user:system", username: "system" },
  auth: { authenticated: false },
};

Deno.test("services see how the kernel authenticated the request", async () => {
  const module = new URL(
    "./testdata/authentication_identity.ts",
    import.meta.url,
  ).href;
  const unauthenticated = { action: "reject", status: 401 };
  const cases: Array<[unknown, boolean, Record<string, string>]> = [
    [{ method: "password", username: "alice", sub: "user:alice" }, false, {
      method: "password",
    }],
    [{ method: "certificate", sub: "user:alice" }, false, {
      method: "certificate",
    }],
    [{ method: "token", fromCookie: true, sub: "user:alice" }, false, {
      method: "token",
      tokenSource: "cookie",
    }],
    [{ method: "token", fromCookie: false, sub: "user:alice" }, false, {
      method: "token",
      tokenSource: "header",
    }],
    [{ sub: "user:alice" }, false, {}],
    [undefined, true, { method: "native" }],
  ];
  for (const [input, approved, expected] of cases) {
    const result = await authenticateRequest(
      new Request("https://service/"),
      {
        ...meta,
        user: approved
          ? { userId: "user:alice", username: "alice" }
          : meta.user,
        authentication: {
          module,
          input,
          approved,
          principal: "user:alice",
          unauthenticated,
        },
      },
      bridge,
      new AbortController().signal,
    );
    assertEquals(result.response, undefined);
    assertEquals(result.meta.auth, {
      authenticated: true,
      realm: "user",
      userId: "user:alice",
      username: "alice",
      ...expected,
    });
    assertEquals("authentication" in result.meta, false);
  }
});
