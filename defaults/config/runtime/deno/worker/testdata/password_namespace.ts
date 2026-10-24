import { kernel } from "@the8020/kernel";

export default async function () {
  const digest = await kernel.crypto.passwordDigest(
    "alice",
    new TextEncoder().encode("password"),
    new Uint8Array(16),
  );
  return digest.length === 32;
}
