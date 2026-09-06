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

  readonly sandboxId: string;
  readonly workerId: string;
  readonly contextId: string;
  readonly parentContextId: string | undefined;
  readonly jobRunId: string | undefined;
  readonly serviceInstanceId: string | undefined;
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
  get sandboxId() {
    return currentExecutionContext().sandboxId;
  },
  get workerId() {
    return currentExecutionContext().workerId;
  },
  get contextId() {
    return currentExecutionContext().contextId;
  },
  get parentContextId() {
    return currentExecutionContext().parentContextId;
  },
  get jobRunId() {
    return currentExecutionContext().jobRunId;
  },
  get serviceInstanceId() {
    return currentExecutionContext().serviceInstanceId;
  },
  get persistentExecutionId() {
    return currentExecutionContext().persistentExecutionId;
  },
});
