import { currentExecutionContext } from "./runtime.ts";
import { AsyncLocalStorage } from "node:async_hooks";
import type { ExecutionContext, ExecutionContextType } from "./types.ts";

export type { ExecutionContext, ExecutionContextType } from "./types.ts";

export interface ContextAPI {
  readonly current: ExecutionContext;
  readonly type: ExecutionContextType;
  readonly id: string;
  readonly userId: string;
  readonly username: string;
  readonly language: string | undefined;
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

const languageScope = new AsyncLocalStorage<string>();

/** Scope application presentation data without changing execution authority. */
export function withLanguage<T>(language: string, run: () => T): T {
  if (
    typeof language !== "string" || !language.length || language.length > 128
  ) {
    throw new TypeError("invalid context language");
  }
  return languageScope.run(language, run);
}

export const context: ContextAPI = Object.freeze({
  get current() {
    const current = currentExecutionContext();
    const language = languageScope.getStore();
    return language === undefined
      ? current
      : Object.freeze({ ...current, language });
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
  get language() {
    return languageScope.getStore();
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
