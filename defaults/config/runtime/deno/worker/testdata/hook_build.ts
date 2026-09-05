import { context } from "@the8020/context";

export const states = new WeakSet<object>();

export interface State {
  trace: string[];
  workers: string[];
  value: number;
  fail?: boolean;
  packageId?: unknown;
  scopeFrozen?: boolean;
  user?: string;
}

export default function build(
  state: State,
  scope: Readonly<Record<string, unknown>>,
): void {
  states.add(state);
  state.trace.push("build");
  state.workers.push(context.workerId);
  state.value += 1;
  state.packageId = scope.package_id;
  state.scopeFrozen = Object.isFrozen(scope);
  state.user = context.userId;
}
