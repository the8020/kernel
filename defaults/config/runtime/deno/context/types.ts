export type ExecutionContextType = "service" | "module" | "program";

export interface ExecutionContext {
  readonly type: ExecutionContextType;
  readonly id: string;
  readonly userId: string;
  readonly username: string;
  /** Application-selected presentation language; absent in unlocalized executions. */
  readonly language?: string;
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
