import { context } from "@the8020/context";
import { type State, states } from "./hook_build.ts";

export default function filter(state: State): void {
  if (!states.has(state)) throw new Error("hook state was copied");
  console.log("filter ran");
  state.trace.push("filter");
  state.workers.push(context.workerId);
  state.value -= 1;
}
