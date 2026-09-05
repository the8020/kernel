import { currentExecutionContext } from "./runtime.ts";
import type { ExecutionContext, ExecutionContextType } from "./types.ts";

export type { ExecutionContext, ExecutionContextType } from "./types.ts";

export interface ContextAPI {
  readonly current: ExecutionContext;
  readonly type: ExecutionContextType;
  readonly id: string;
  readonly userId: string;
  readonly username: string;
  readonly authenticated: boolean;
  readonly nodeId: string;
  readonly runtimeGroupId: string;
  readonly sandboxId: string;
  readonly workerId: string;
  readonly executionId: string;
  readonly requestId: string;
  readonly persistentExecutionId: string | undefined;
}

export const context: ContextAPI = Object.freeze({
  get current() {
    return currentExecutionContext();
  },
  get type() {
    return currentExecutionContext().type;
  },
  get id() {
    return currentExecutionContext().id;
  },
  get userId() {
    return currentExecutionContext().userId;
  },
  get authenticated() {
    return currentExecutionContext().authenticated;
  },
  get username() {
    return currentExecutionContext().username;
  },
  get nodeId() {
    return currentExecutionContext().nodeId;
  },
  get runtimeGroupId() {
    return currentExecutionContext().runtimeGroupId;
  },
  get sandboxId() {
    return currentExecutionContext().sandboxId;
  },
  get workerId() {
    return currentExecutionContext().workerId;
  },
  get executionId() {
    return currentExecutionContext().executionId;
  },
  get requestId() {
    return currentExecutionContext().requestId;
  },
  get persistentExecutionId() {
    return currentExecutionContext().persistentExecutionId;
  },
});
