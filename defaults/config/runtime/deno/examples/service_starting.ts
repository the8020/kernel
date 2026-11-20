export { fetch } from "./service.ts";

const gate = new URL(import.meta.url).searchParams.get("gate");
if (gate) await (await fetch(gate)).body?.cancel();
