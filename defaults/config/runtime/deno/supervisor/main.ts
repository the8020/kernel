import { Supervisor } from "./supervisor.ts";
import {
  assertEnvelope,
  type Envelope,
  PROTOCOL_VERSION,
} from "@the8020/protocol";
import type { KernelCall } from "../worker/contracts.ts";
import { kernelCallbackRequest } from "./callback_request.ts";

const required = (name: string): string => {
  const value = Deno.env.get(name);
  if (value === undefined || value.length === 0) {
    throw new Error(`${name} is required`);
  }
  return value;
};

const requiredMilliseconds = (name: string, fallback: number): number => {
  const value = Number(Deno.env.get(name) ?? String(fallback));
  if (!Number.isSafeInteger(value) || value < 10) {
    throw new Error(`${name} must be an integer of at least 10`);
  }
  return value;
};

const runtimeGroupId = required("RUNTIME_GROUP_ID");
const sandboxId = required("SANDBOX_ID");
const workloadType = required("WORKLOAD_TYPE") as
  | "service"
  | "job";
const token = required("INTERNAL_API_TOKEN");
const callback = Deno.env.get("KERNEL_CALLBACK_ADDRESS");

const postCallback = async (path: string, body: unknown): Promise<Response> => {
  if (callback === undefined || callback.length === 0) {
    throw new Error("kernel callback is unavailable");
  }
  const response = await fetch(new URL(path, callback), {
    method: "POST",
    headers: {
      authorization: `Bearer ${token}`,
      "content-type": "application/json",
    },
    body: JSON.stringify(body),
  });
  if (!response.ok) {
    const detail = (await response.text()).trim();
    throw new Error(
      detail || `kernel callback returned ${response.status}`,
    );
  }
  return response;
};

const kernelCall: KernelCall | undefined = callback === undefined ||
    callback.length === 0
  ? undefined
  : async (call) => {
    const callbackRequest = kernelCallbackRequest(call, sandboxId);
    const correlationId = crypto.randomUUID();
    const envelope: Envelope<Record<string, unknown>> = {
      protocol_version: PROTOCOL_VERSION,
      message_type: callbackRequest.messageType,
      runtime_group_id: runtimeGroupId,
      correlation_id: correlationId,
      payload: callbackRequest.payload,
    };
    const response = await postCallback(callbackRequest.path, envelope);
    const result: unknown = await response.json();
    assertEnvelope(result);
    if (
      result.message_type !== callbackRequest.responseMessageType ||
      result.runtime_group_id !== runtimeGroupId ||
      result.correlation_id !== correlationId || result.payload === undefined
    ) {
      throw new Error("kernel response identity mismatch");
    }
    return result.payload;
  };

const supervisor = new Supervisor({
  runtimeGroupId,
  sandboxId,
  nodeId: Deno.env.get("NODE_ID") ?? runtimeGroupId,
  workloadType,
  token,
  supervisorVersion: "1.0.0",
  kernelCall,
  workerStopGraceMilliseconds: requiredMilliseconds(
    "WORKER_STOP_GRACE_MS",
    1_000,
  ),
});

const port = Number(Deno.env.get("SUPERVISOR_PORT") ?? "8000");
const host = Deno.env.get("SUPERVISOR_HOST") ?? "0.0.0.0";
const heartbeatInterval = Number(
  Deno.env.get("HEARTBEAT_INTERVAL_MS") ?? "5000",
);
if (!Number.isSafeInteger(heartbeatInterval) || heartbeatInterval < 100) {
  throw new Error("HEARTBEAT_INTERVAL_MS must be an integer of at least 100");
}
const server = Deno.serve({ hostname: host, port }, supervisor.handler);

if (callback !== undefined && callback.length > 0) {
  const send = async (path: string, body: unknown): Promise<void> => {
    const response = await postCallback(path, body);
    await response.body?.cancel();
  };
  await send("/v1/runtime/register", supervisor.registration());
  const heartbeat = async (): Promise<void> => {
    try {
      await send("/v1/runtime/heartbeat", supervisor.heartbeat());
    } catch (error) {
      console.error(
        JSON.stringify({
          level: "error",
          event: "heartbeat_failed",
          error: error instanceof Error ? error.message : String(error),
        }),
      );
    }
  };
  setInterval(heartbeat, heartbeatInterval);
  await heartbeat();
}

await server.finished;
