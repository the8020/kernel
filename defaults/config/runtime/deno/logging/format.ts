import { OMITTED } from "./protocol.ts";

type Secrets = Readonly<Record<string, string>> | undefined;
const REDACTED = "[redacted]";
const nativeStackGetter = Object.getOwnPropertyDescriptor(new Error(), "stack")
  ?.get;
const nativeStackFormatter = Object.getOwnPropertyDescriptor(
  Error,
  "prepareStackTrace",
);
const sensitive =
  /password|passwd|secret|token|credential|authorization|cookie|private.?key|access.?key/i;

function runeCost(code: number): number {
  if (
    code < 32 || (code >= 127 && code <= 159) || code === 0x2028 ||
    code === 0x2029 || (code >= 0xd800 && code <= 0xdfff)
  ) return 6;
  if (code === 34 || code === 92) return 2;
  return code < 128 ? 1 : code < 2048 ? 2 : code < 65536 ? 3 : 4;
}

function headEnd(value: string, budget: number): number {
  let end = 0;
  while (end < value.length) {
    const code = value.codePointAt(end)!;
    budget -= runeCost(code);
    if (budget < 0) break;
    end += code > 65535 ? 2 : 1;
  }
  return end;
}

function tailStart(value: string, budget: number): number {
  let start = value.length;
  while (start > 0) {
    let next = start - 1;
    const code = value.charCodeAt(next);
    if (code >= 0xdc00 && code <= 0xdfff && next > 0) {
      const high = value.charCodeAt(next - 1);
      if (high >= 0xd800 && high <= 0xdbff) next--;
    }
    budget -= runeCost(value.codePointAt(next)!);
    if (budget < 0) break;
    start = next;
  }
  return start;
}

// Select endpoints before allocating. Escaped-byte accounting includes UTF-8,
// controls and stack newlines, matching the file owner's stricter text codec.
export function truncateText(value: string, budget: number): string {
  const end = headEnd(value, budget);
  if (end === value.length) return value;
  if (budget < 24) return ".".repeat(Math.max(0, budget));
  const half = Math.floor((budget - 24) / 2);
  return value.slice(0, headEnd(value, half)) + OMITTED +
    value.slice(tailStart(value, budget - 24 - half));
}

// Mark only the selected window. Find matches in the original string so a
// truncation boundary through a long secret cannot expose its retained fragment.
// No split/replace/stringify of the unbounded original value is performed.
function redactedWindow(
  value: string,
  start: number,
  end: number,
  secrets: Secrets,
): string {
  if (secrets === undefined || end === start) return value.slice(start, end);
  const mask = new Uint8Array(end - start);
  for (const name in secrets) {
    const secret = secrets[name];
    if (secret === undefined || secret.length === 0) continue;
    let at = value.indexOf(secret, Math.max(0, start - secret.length + 1));
    while (at >= 0 && at < end) {
      mask.fill(
        1,
        Math.max(0, at - start),
        Math.min(end, at + secret.length) - start,
      );
      at = value.indexOf(secret, at + 1);
    }
  }
  const parts: string[] = [];
  for (let at = 0; at < mask.length;) {
    const marked = mask[at];
    let until = at + 1;
    while (until < mask.length && mask[until] === marked) until++;
    parts.push(
      marked === 1 ? REDACTED : value.slice(start + at, start + until),
    );
    at = until;
  }
  return parts.join("");
}

export function safeText(
  value: string,
  budget: number,
  secrets?: Secrets,
): string {
  const end = headEnd(value, budget);
  if (end === value.length) {
    return truncateText(redactedWindow(value, 0, end, secrets), budget);
  }
  if (budget < 24) return ".".repeat(Math.max(0, budget));
  const half = Math.floor((budget - 24) / 2);
  const head = redactedWindow(value, 0, headEnd(value, half), secrets);
  const tail = redactedWindow(
    value,
    tailStart(value, budget - 24 - half),
    value.length,
    secrets,
  );
  return truncateText(head + OMITTED + tail, budget);
}

function dataProperty(value: object, key: string): unknown {
  let owner: object | null = value;
  for (let depth = 0; owner !== null && depth < 4; depth++) {
    const descriptor = Object.getOwnPropertyDescriptor(owner, key);
    if (descriptor !== undefined) {
      return "value" in descriptor ? descriptor.value : "[accessor]";
    }
    owner = Object.getPrototypeOf(owner);
  }
  return undefined;
}

function selectedIndices(length: number): number[] {
  if (length <= 8) return Array.from({ length }, (_, index) => index);
  return [0, 1, 2, 3, length - 4, length - 3, length - 2, length - 1];
}

function errorStack(value: Error): unknown {
  const stack = Object.getOwnPropertyDescriptor(value, "stack");
  if (stack === undefined || "value" in stack) return stack?.value;
  const formatter = Object.getOwnPropertyDescriptor(Error, "prepareStackTrace");
  if (
    stack.get !== nativeStackGetter ||
    formatter?.value !== nativeStackFormatter?.value ||
    formatter?.get !== nativeStackFormatter?.get ||
    dataProperty(value, "name") === "[accessor]" ||
    dataProperty(value, "message") === "[accessor]"
  ) {
    return "[custom stack formatter omitted]";
  }
  // V8 exposes native Error.stack lazily as an accessor. Read that native
  // diagnostic once; never call a package-supplied stack getter/formatter.
  return nativeStackGetter?.call(value);
}

function valueText(
  value: unknown,
  budget: number,
  secrets: Secrets,
  seen: Set<object>,
  depth: number,
  quoted = false,
): string {
  if (budget < 24) return truncateText("[omitted]", budget);
  try {
    if (typeof value === "string") {
      const text = safeText(value, budget - (quoted ? 2 : 0), secrets);
      return quoted ? JSON.stringify(text) : text;
    }
    if (value === null) return "null";
    if (typeof value === "bigint") {
      return value > 10n ** 100n || value < -(10n ** 100n)
        ? "[large bigint]"
        : safeText(`${value}n`, budget, secrets);
    }
    if (typeof value === "symbol") return "[symbol]";
    if (typeof value === "function") return "[function]";
    if (typeof value !== "object") {
      return safeText(String(value), budget, secrets);
    }
    if (seen.has(value)) return "[circular]";
    if (depth >= 4) return "[depth omitted]";
    seen.add(value);
    try {
      if (value instanceof Error) {
        const name = dataProperty(value, "name");
        const message = dataProperty(value, "message");
        const stack = errorStack(value);
        const cause = dataProperty(value, "cause");
        const hasCause = cause !== undefined;
        const primaryBudget = hasCause ? Math.floor(budget * 2 / 3) : budget;
        const titleBudget = Math.floor(primaryBudget / 3);
        const title =
          safeText(typeof name === "string" ? name : "Error", 64, secrets) +
          ": " +
          safeText(
            typeof message === "string" ? message : "",
            titleBudget,
            secrets,
          );
        const trace = typeof stack === "string"
          ? "\n" + safeText(stack, primaryBudget - titleBudget - 80, secrets)
          : "";
        const caused = hasCause
          ? "\nCaused by: " +
            valueText(
              cause,
              budget - primaryBudget - 20,
              secrets,
              seen,
              depth + 1,
            )
          : "";
        return truncateText(title + trace + caused, budget);
      }
      if (Array.isArray(value)) {
        const indices = selectedIndices(value.length);
        const each = Math.max(
          0,
          Math.floor((budget - 48) / Math.max(1, indices.length)),
        );
        const parts = indices.map((index) =>
          valueText(
            dataProperty(value, String(index)),
            each,
            secrets,
            seen,
            depth + 1,
            true,
          )
        );
        if (value.length > 8) parts.splice(4, 0, JSON.stringify(OMITTED));
        return truncateText("[" + parts.join(",") + "]", budget);
      }
      const keys: string[] = [];
      let omitted = false;
      for (const key in value) {
        if (!Object.hasOwn(value, key)) continue;
        if (keys.length === 8) {
          omitted = true;
          break;
        }
        keys.push(key);
      }
      const each = Math.max(
        0,
        Math.floor((budget - 48) / Math.max(1, keys.length)),
      );
      const parts = keys.map((key) => {
        const safeKey = safeText(
          key,
          Math.min(64, Math.floor(each / 3)),
          secrets,
        );
        const text = key.length > 256 || sensitive.test(key)
          ? JSON.stringify(REDACTED)
          : valueText(
            dataProperty(value, key),
            each - safeKey.length - 4,
            secrets,
            seen,
            depth + 1,
            true,
          );
        return JSON.stringify(safeKey) + ":" + text;
      });
      if (omitted) parts.push('"fields_omitted":true');
      return truncateText("{" + parts.join(",") + "}", budget);
    } finally {
      seen.delete(value);
    }
  } catch {
    return "[format failed]";
  }
}

export function formatValues(
  values: readonly unknown[],
  budget = 12 * 1024,
  secrets?: Secrets,
): string {
  const indices = selectedIndices(values.length);
  const each = Math.max(
    0,
    Math.floor((budget - 32) / Math.max(1, indices.length)),
  );
  const seen = new Set<object>();
  const parts = indices.map((index) =>
    valueText(values[index], each, secrets, seen, 0)
  );
  if (values.length > 8) parts.splice(4, 0, OMITTED);
  return truncateText(parts.join(" "), budget);
}

export function formatAttributes(
  fields: Record<string, unknown> | undefined,
  secrets?: Secrets,
): Record<string, string> | undefined {
  if (fields === undefined) return undefined;
  const result: Record<string, string> = Object.create(null);
  let count = 0;
  try {
    for (const key in fields) {
      if (!Object.hasOwn(fields, key)) continue;
      if (count++ === 8) {
        result.attributes_omitted = "true";
        break;
      }
      result[safeText(key, 64, secrets)] =
        key.length > 256 || sensitive.test(key)
          ? REDACTED
          : valueText(dataProperty(fields, key), 160, secrets, new Set(), 0);
    }
  } catch {
    result.format_error = "[format failed]";
  }
  return result;
}
