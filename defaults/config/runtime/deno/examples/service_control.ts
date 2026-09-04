import type {
  ServiceEntrypoint,
  WorkerControlFunctions,
} from "../worker/contracts.ts";
import { kernel } from "@the8020/kernel";

export const fetch: ServiceEntrypoint = () =>
  new Response(null, { status: 204 });

export const workerFunctions: WorkerControlFunctions = Object.freeze({
  "example.echo": (input: unknown) => input,
  "example.complete-persistent": async () => {
    await kernel.execution.completePersistent();
    return { completed: true };
  },
});
