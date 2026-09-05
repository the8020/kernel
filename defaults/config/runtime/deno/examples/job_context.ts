import { context } from "@the8020/context";

export default function currentExecution() {
  return context.current;
}
