export type ExecutionContextType = "service" | "module" | "program";

export interface ExecutionContext {
  readonly type: ExecutionContextType;
  readonly id: string;
  readonly userId: string;
  readonly username: string;
  readonly authenticated: boolean;
  readonly nodeId: string;

  readonly sandboxId: string;
  readonly workerId: string;
  readonly contextId: string;
  readonly parentContextId?: string;
  readonly jobRunId?: string;
  readonly serviceInstanceId?: string;
  readonly persistentExecutionId?: string;
}
