import { kernel } from "@the8020/kernel";

export default function run(fail = false): void {
  const secret = kernel.execution.optionalSecret("password");
  console.log("job value", { value: secret });
  if (fail) {
    throw new Error(`outer ${secret}`, { cause: new Error(`inner ${secret}`) });
  }
}
