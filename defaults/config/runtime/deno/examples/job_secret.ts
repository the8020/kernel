import { kernel } from "@the8020/kernel";

export default function readSecret(name: string, fail = false): string {
  const value = kernel.execution.optionalSecret(name) ?? "missing";
  if (fail) throw new Error("deliberate job failure");
  return value;
}
