import type { MessageType } from "@the8020/protocol";
import type { KernelCallRequest } from "../worker/contracts.ts";

export interface KernelCallbackRequest {
  path: string;
  messageType: MessageType;
  responseMessageType: MessageType;
  payload: Record<string, unknown>;
}

export function kernelCallbackRequest(
  call: KernelCallRequest,
  sandboxId: string,
): KernelCallbackRequest {
  const identity = {
    execution_id: call.executionId,
    worker_id: call.workerId,
    service_id: call.serviceId,
    request_id: call.requestId,
    sandbox_id: sandboxId,
  };
  switch (call.operation) {
    case "worker.invoke":
      return {
        path: "/v1/runtime/worker/invoke",
        messageType: "worker_invoke",
        responseMessageType: "worker_result",
        payload: {
          target_node_id: call.arguments.nodeId,
          target_sandbox_id: call.arguments.sandboxId,
          target_worker_id: call.arguments.workerId,
          function: call.arguments.function,
          input: call.arguments.input,
          ...identity,
        },
      };
    case "execution.completePersistent":
      return {
        path: "/v1/runtime/execution/complete",
        messageType: "persistent_execution_complete",
        responseMessageType: "persistent_execution_completed",
        payload: {
          ...identity,
          persistent_execution_id: call.persistentExecutionId,
        },
      };
    case "admin.execute":
      return {
        path: "/v1/runtime/admin/execute",
        messageType: "admin_command",
        responseMessageType: "admin_result",
        payload: { ...call.arguments, ...identity },
      };
    case "auth.bootstrapLogin":
      return {
        path: "/v1/runtime/auth/bootstrap-login",
        messageType: "auth_bootstrap_login",
        responseMessageType: "auth_result",
        payload: { ...call.arguments, ...identity },
      };
    case "auth.logoutCurrent":
      return {
        path: "/v1/runtime/auth/logout-current",
        messageType: "auth_logout_current",
        responseMessageType: "auth_result",
        payload: { ...call.arguments, ...identity },
      };
  }
}
