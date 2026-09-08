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
  const packages = scope.packages as readonly { package_id: string }[];
  state.packageId = packages[0]!.package_id;
  state.scopeFrozen = Object.isFrozen(scope) && Object.isFrozen(packages) &&
    packages.every(Object.isFrozen);
  state.user = context.userId;
}
