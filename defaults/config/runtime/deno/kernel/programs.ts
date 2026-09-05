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
  packageCommit: string;
  result: unknown;
  logs:
    | { level: string; message: string; fields?: Record<string, unknown> }[]
    | null;
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
