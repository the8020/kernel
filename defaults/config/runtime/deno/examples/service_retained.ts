import { kernel } from "@the8020/kernel";
import type {
  ServiceEntrypoint,
  WorkerControlFunctions,
} from "../worker/contracts.ts";

const retained = Promise.withResolvers<void>();
const ended = Promise.withResolvers<void>();
const release = Promise.withResolvers<void>();
const response = Promise.withResolvers<Response>();

export const fetch: ServiceEntrypoint = () => {
  void kernel.execution.runPersistent(async () => {
    retained.resolve();
    await release.promise;
  }).then(() => ended.resolve(), (error) => ended.reject(error));
  return response.promise;
};

export const workerFunctions: WorkerControlFunctions = {
  "example.when-retained": async () => {
    await retained.promise;
    return true;
  },
  "example.release": async () => {
    release.resolve();
    response.resolve(new Response(null, { status: 204 }));
    await ended.promise;
    return true;
  },
};
