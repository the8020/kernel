import { newId } from "../identity/mod.ts";
import { Supervisor } from "./supervisor.ts";
import {
  assertEnvelope,
  type Envelope,
  PROTOCOL_VERSION,
} from "@the8020/protocol";
import type { KernelCall } from "../worker/contracts.ts";
import { kernelCallbackRequest } from "./callback_request.ts";
import { postUnixHTTP } from "./unix_http.ts";
import { SnapshotPublisher } from "./snapshot_publisher.ts";
import { LogProducer } from "../logging/producer.ts";
import { installConsoleCapture, policyCapture } from "../logging/capture.ts";
import { formatAttributes, formatValues } from "../logging/format.ts";

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

const sandboxId = required("SANDBOX_ID");
const workloadType = required("WORKLOAD_TYPE") as
  | "service"
  | "job";
const token = required("INTERNAL_API_TOKEN");
const kernelSocketPath = Deno.env.get("KERNEL_SOCKET_PATH");
const nodeId = required("NODE_ID");
const logs = new LogProducer({
  socket: "/run/the8020/logs.sock",
  sandboxId,
  nodeId,
  token,
});
installConsoleCapture(
  policyCapture(() => logs.policy, (level, values, fields) => {
    logs.emit({
      time: new Date().toISOString(),
      level,
      source: "deno",
      component: "supervisor",
      node_id: nodeId,
      sandbox_id: sandboxId,
      message: formatValues(values),
      attributes: formatAttributes(fields),
    });
  }),
);
console.info("Supervisor starting", {
  deno: Deno.version.deno,
  workload: workloadType,
});

const postCallback = async (
  path: string,
  body: unknown,
  signal?: AbortSignal,
): Promise<Response> => {
  if (kernelSocketPath === undefined || kernelSocketPath.length === 0) {
    throw new Error("kernel callback is unavailable");
  }
  const response = await postUnixHTTP(
    kernelSocketPath,
    path,
    token,
    body,
    signal,
  );
  if (!response.ok) {
    const detail = (await response.text()).trim();
    throw new Error(
      detail || `kernel callback returned ${response.status}`,
    );
  }
  return response;
};

const kernelCall: KernelCall | undefined = kernelSocketPath === undefined ||
    kernelSocketPath.length === 0
  ? undefined
  : async (call, signal) => {
    const callbackRequest = kernelCallbackRequest(call);
    const correlationId = newId("cor");
    const envelope: Envelope<Record<string, unknown>> = {
      protocol_version: PROTOCOL_VERSION,
      message_type: callbackRequest.messageType,
      sandbox_id: sandboxId,
      correlation_id: correlationId,
      payload: callbackRequest.payload,
    };
    const response = await postCallback(callbackRequest.path, envelope, signal);
    const result: unknown = await response.json();
    assertEnvelope(result);
    if (
      result.message_type !== callbackRequest.responseMessageType ||
      result.sandbox_id !== sandboxId ||
      result.correlation_id !== correlationId || result.payload === undefined
    ) {
      throw new Error("kernel response identity mismatch");
    }
    return result.payload;
  };

const snapshots = new SnapshotPublisher(
  () => supervisor.heartbeat(),
  async (snapshot) => {
    const response = await postCallback("/v1/runtime/heartbeat", snapshot);
    await response.body?.cancel();
  },
  (error) => console.error("Heartbeat failed", error),
);

const supervisor = new Supervisor({
  sandboxId,
  nodeId,
  workloadType,
  token,
  supervisorVersion: "1.0.0",
  kernelCall,
  logSink: logs,
  onStateChange: () => snapshots.markDirty(),
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

let heartbeat: ReturnType<typeof setInterval> | undefined;
let stopping: Promise<void> | undefined;
const stop = (): void => {
  stopping ??= (async () => {
    console.info("Supervisor stopping");
    await supervisor.drain();
    await server.shutdown();
  })().catch((error) => {
    console.error("Supervisor shutdown failed", error);
  });
};
Deno.addSignalListener("SIGTERM", stop);
Deno.addSignalListener("SIGINT", stop);
try {
  if (kernelSocketPath !== undefined && kernelSocketPath.length > 0) {
    const send = async (path: string, body: unknown): Promise<void> => {
      const response = await postCallback(path, body);
      await response.body?.cancel();
    };
    await send("/v1/runtime/register", supervisor.registration());
    snapshots.enable();
    snapshots.markDirty();
    heartbeat = setInterval(() => snapshots.markDirty(), heartbeatInterval);
    await snapshots.flush();
  }
  console.info("Supervisor ready");
  await server.finished;
} catch (error) {
  console.error("Supervisor failed", error);
  throw error;
} finally {
  clearInterval(heartbeat);
  Deno.removeSignalListener("SIGTERM", stop);
  Deno.removeSignalListener("SIGINT", stop);
  console.info("Supervisor exited");
  await logs.close();
}
