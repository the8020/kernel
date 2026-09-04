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
): KernelCallbackRequest {
  const identity = {
    execution_id: call.executionId,
    worker_id: call.workerId,
    request_id: call.requestId,
  };
  switch (call.operation) {
    case "database.info":
      return {
        path: "/v1/runtime/database/info",
        messageType: "database_execute",
        responseMessageType: "database_result",
        payload: identity,
      };
    case "database.transaction.begin":
    case "database.transaction.commit":
    case "database.transaction.rollback":
      return {
        path: "/v1/runtime/database/transaction",
        messageType: "database_execute",
        responseMessageType: "database_result",
        payload: { operation: call.operation, ...call.arguments, ...identity },
      };
    case "database.scope.close":
      return {
        path: "/v1/runtime/database/scope",
        messageType: "database_execute",
        responseMessageType: "database_result",
        payload: identity,
      };
    case "worker.invoke":
      return {
        path: "/v1/runtime/worker/invoke",
        messageType: "worker_invoke",
        responseMessageType: "worker_result",
        payload: {
          target_node_id: call.arguments.nodeId,
          target_sandbox_id: call.arguments.sandboxId,
          target_worker_id: call.arguments.workerId,
          ...(call.arguments.persistentExecutionId === undefined ? {} : {
            target_persistent_execution_id:
              call.arguments.persistentExecutionId,
          }),
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
          service_id: call.serviceId,
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
    case "runtime.operation":
      return {
        path: "/v1/runtime/operation/execute",
        messageType: "admin_command",
        responseMessageType: "admin_result",
        payload: { ...call.arguments, ...identity },
      };
    case "database.execute":
      return {
        path: "/v1/runtime/database/execute",
        messageType: "database_execute",
        responseMessageType: "database_result",
        payload: { ...call.arguments, ...identity },
      };
    case "auth.login":
      return {
        path: "/v1/runtime/auth/login",
        messageType: "auth_login",
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
