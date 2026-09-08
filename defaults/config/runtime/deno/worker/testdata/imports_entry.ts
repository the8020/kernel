import { value } from "./imports_direct.ts";

export default async function (dynamic = false): Promise<string> {
  if (dynamic) return (await import("./imports_dynamic.ts")).value;
  return value;
}
