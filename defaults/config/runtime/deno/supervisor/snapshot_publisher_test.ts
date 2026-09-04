import { assertEquals } from "../test/assert.ts";
import { SnapshotPublisher } from "./snapshot_publisher.ts";

Deno.test("snapshot publisher coalesces changes and sends the newest absolute state", async () => {
  let revision = 1;
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => release = resolve);
  const sent: unknown[] = [];
  let active = 0;
  let maximumActive = 0;
  const publisher = new SnapshotPublisher(
    () => ({ revision }),
    async (snapshot) => {
      active++;
      maximumActive = Math.max(maximumActive, active);
      sent.push(snapshot);
      if (sent.length === 1) await blocked;
      active--;
    },
  );
  publisher.enable();
  publisher.markDirty();
  publisher.markDirty();
  await Promise.resolve();
  revision = 2;
  publisher.markDirty();
  revision = 3;
  publisher.markDirty();
  release();
  await publisher.flush();
  assertEquals(sent, [{ revision: 1 }, { revision: 3 }]);
  assertEquals(maximumActive, 1);
});

Deno.test("periodic dirty mark repairs a failed snapshot submission", async () => {
  let attempts = 0;
  const failures: unknown[] = [];
  const publisher = new SnapshotPublisher(
    () => ({ revision: 4 }),
    () => {
      attempts++;
      return attempts === 1
        ? Promise.reject(new Error("temporarily unavailable"))
        : Promise.resolve();
    },
    (error) => failures.push(error),
  );
  publisher.enable();
  publisher.markDirty();
  await publisher.flush();
  assertEquals(attempts, 1);
  publisher.markDirty();
  await publisher.flush();
  assertEquals(attempts, 2);
  assertEquals(failures.length, 1);
});
