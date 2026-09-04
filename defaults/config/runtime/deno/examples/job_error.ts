import { AdminCommandError } from "@the8020/kernel";

export default function fail(): never {
  throw new AdminCommandError({
    code: "invalid_arguments",
    message: "structured job failure",
    details: { field: "example" },
  });
}
