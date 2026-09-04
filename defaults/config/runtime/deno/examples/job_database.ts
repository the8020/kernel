import { kernel } from "@the8020/kernel";

export default async function job(value: number): Promise<unknown> {
  return await kernel.database.execute(
    "SELECT $1",
    [value],
    { returnRows: true },
  );
}
