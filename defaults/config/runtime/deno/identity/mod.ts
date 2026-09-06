const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789";
const prefixPattern = /^[a-z]{3}$/;

/** Generate an operational identity. Registration must still reject collisions. */
export function newId(prefix: string): string {
  if (!prefixPattern.test(prefix)) {
    throw new TypeError("identity prefix must contain three lowercase letters");
  }
  let suffix = "";
  const random = new Uint8Array(32);
  while (suffix.length < 10) {
    crypto.getRandomValues(random);
    for (const value of random) {
      if (value >= 252) continue;
      suffix += alphabet[value % alphabet.length];
      if (suffix.length === 10) break;
    }
  }
  return `${prefix}-${suffix}`;
}

/** Check the entire canonical encoding and the expected resource type. */
export function isId(value: unknown, prefix: string): value is string {
  return prefixPattern.test(prefix) && typeof value === "string" &&
    value.length === 14 && value.startsWith(`${prefix}-`) &&
    /^[a-z0-9]{10}$/.test(value.slice(4));
}
