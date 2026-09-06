import {
  addDrops,
  allows,
  type DropCounts,
  INITIAL_POLICY,
  type LogPolicy,
  type LogRecord,
  type LogSink,
} from "../logging/protocol.ts";

// Test-owned observation only. Production supervisors have no log history.
export class TestLogSink implements LogSink {
  records: LogRecord[] = [];
  losses: DropCounts = [0, 0, 0, 0];
  policy: Readonly<LogPolicy> = INITIAL_POLICY;
  #listeners = new Set<(policy: Readonly<LogPolicy>) => void>();
  emit(record: LogRecord): void {
    if (!allows(this.policy, record.level)) return;
    if (this.records.length >= 1000) {
      throw new Error("test log observation exceeded its bound");
    }
    this.records.push(record);
  }
  dropped(counts: DropCounts): void {
    addDrops(this.losses, counts);
  }
  subscribe(listener: (policy: Readonly<LogPolicy>) => void): () => void {
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  }
  setPolicy(policy: Readonly<LogPolicy>): void {
    this.policy = policy;
    for (const listener of this.#listeners) listener(policy);
  }
}
