import { kernel } from "@the8020/kernel";
import type { ServiceEntrypoint } from "../worker/contracts.ts";

const database = await kernel.database.info();

export const fetch: ServiceEntrypoint = () => Response.json(database);
