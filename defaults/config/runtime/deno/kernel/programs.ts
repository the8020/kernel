export interface ProgramSummary {
  program_id: string;
  package_id: string;
  name: string;
  commit: string;
  description?: string;
  discoverable: boolean;
  uui: boolean;
  entrypoint: string;
  entrypoint_url: string;
}

export interface ProgramRunInput {
  programId: string;
  arguments?: unknown[];
  username?: string;
  sandboxGroup?: string;
  timeoutMs?: number;
}
export interface ProgramRun {
  state: "succeeded" | "failed";
  failure: string;
  executionId: string;
  nodeId: string;
  sandboxId: string;
  workerId: string;
  contextId: string;
  parentContextId: string;
  logPosition: string;
  queuedAt: string;
  startedAt: string;
  finishedAt: string;
  packageCommit: string;
  result: unknown;
}
export interface PackageEvent<Data = unknown> {
  id: string;
  name: string;
  nodeId: string;
  occurredAt: string;
  data: Data;
}
export interface EventReceipt {
  id: string;
  listeners: number;
}
