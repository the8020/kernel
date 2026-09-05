import { context } from "@the8020/context";
import { type State, states } from "./hook_build.ts";

export default async function enhance(state: State): Promise<void> {
  await Promise.resolve();
  if (!states.has(state)) throw new Error("hook state was copied");
  if (state.fail) throw new Error("enhancement failed");
  state.trace.push("enhance");
  state.workers.push(context.workerId);
  state.value *= 3;
}
