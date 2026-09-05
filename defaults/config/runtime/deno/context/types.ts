export type ExecutionContextType = "service" | "job" | "program";

export interface ExecutionContext {
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
  readonly persistentExecutionId?: string;
}
