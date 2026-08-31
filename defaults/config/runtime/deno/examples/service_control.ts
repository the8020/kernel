import type {
  ServiceEntrypoint,
  WorkerControlFunctions,
} from "../worker/contracts.ts";

export const fetch: ServiceEntrypoint = () =>
  new Response(null, { status: 204 });

export const workerFunctions: WorkerControlFunctions = Object.freeze({
  "example.echo": (input: unknown) => input,
});
