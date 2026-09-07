// Qualification of the exact xterm versions currently used by UUI. This probe
// characterizes the upstream serializer; failures are not acceptance results.
import headless from "npm:@xterm/headless@5.5.0";
import serialize from "npm:@xterm/addon-serialize@0.13.0";
import { captureTerminal, installHeadlessColors, restoreTerminal } from "./xterm_state.ts";

const { Terminal } = headless;
const { SerializeAddon } = serialize;
type Engine = InstanceType<typeof Terminal>;

async function write(t: Engine, data: string | Uint8Array): Promise<void> {
  await new Promise<void>((resolve) => t.write(data, resolve));
}

function visible(t: Engine) {
  return {
    active: t.buffer.active.type,
    cursor: [t.buffer.active.cursorX, t.buffer.active.cursorY],
    modes: t.modes,
    normal: Array.from({ length: t.buffer.normal.length }, (_, i) =>
      t.buffer.normal.getLine(i)?.translateToString(true)),
    alternate: Array.from({ length: t.buffer.alternate.length }, (_, i) =>
      t.buffer.alternate.getLine(i)?.translateToString(true)),
  };
}

const cases: { name: string; before: string | Uint8Array; after: string | Uint8Array; size?: [number, number] }[] = [
  { name: "plain text", before: "hello\r\nworld", after: "!" },
  { name: "UTF-8 split", before: new Uint8Array([0xf0, 0x9f]), after: new Uint8Array([0x98, 0x80]) },
  { name: "CSI split", before: "hello\x1b[2;", after: "3HX" },
  { name: "OSC split", before: "hello\x1b]0;pending", after: " title\x07world" },
  { name: "saved cursor", before: "abcdef\x1b7\x1b[2;1HZ", after: "\x1b8X" },
  { name: "scroll margins", before: "\x1b[2;4r\x1b[4;1Hbefore", after: "\r\nX" },
  { name: "custom tab stops", before: "\x1b[3g\x1b[6G\x1bH\r", after: "\tX" },
  { name: "DEC line drawing", before: "\x1b(0", after: "lqqk" },
  { name: "alternate buffer", before: "normal\x1b[?1049h\x1b[2;4Halternate", after: "\x1b[?1049lX" },
  { name: "modes", before: "\x1b[?1h\x1b[?2004h\x1b[?1000h\x1b[?7l", after: "abc" },
  { name: "wide and combining text", before: "汉字e\u0301😀", after: "\bX" },
  { name: "wrapped cursor", before: "12345678901234567890", after: "X" },
  { name: "DCS split", before: "\x1bP$q", after: "m\x1b\\" },
  { name: "extended attributes", before: "\x1b[4:3;58:2::10:20:30;38;2;200;100;50mX", after: "Y" },
  { name: "cursor appearance", before: "\x1b[5 q\x1b[?25l", after: "hidden" },
  { name: "hyperlink", before: "\x1b]8;id=example;https://example.com\x07link", after: "more\x1b]8;;\x07end" },
  { name: "detached palette query", before: "\x1b]4;1;#010203\x07\x1b]11;#102030\x07", after: "\x1b]4;1;?\x07\x1b]11;?\x07" },
  { name: "palette reset after reconnect", before: "\x1b]4;1;#010203\x07", after: "\x1b]104;1\x07\x1b]4;1;?\x07" },
  { name: "normal reflow wider", before: "abcdefghijklmnop汉字e\u0301😀\r\n".repeat(30), size: [32, 9], after: "after resize\r\n" },
  { name: "normal reflow narrower", before: "abcdefghijklmnop汉字e\u0301😀\r\n".repeat(30), size: [12, 4], after: "after resize\r\n" },
  { name: "alternate resize", before: "normal\x1b[?1049h12345678901234567890\x1b[4;4HX", size: [12, 4], after: "\x1b[?1049lafter" },
  { name: "UTF-16 surrogate split", before: "\ud83d", after: "\ude00" },
  { name: "protected cell erase", before: "\x1b[1\"qprotected\x1b[0\"qunprotected\r", after: "\x1b[?0KX" },
];

if (Deno.args.includes("--state")) {
  const sample = new TextEncoder().encode("A汉字e\u0301😀\x1b[38;2;1;2;3mB\x1b7\x1b[2;4HC\x1b8\x1b]0;title\x1b\\\x1bP$qm\x1b\\\x1b]8;;https://example.com\x07link\x1b]8;;\x07\r\nend");
  for (let split = 0; split <= sample.length; split++) {
    cases.push({ name: `mixed sequence byte split ${split}`, before: sample.slice(0, split), after: sample.slice(split) });
  }
}

const results = [];
const fixtures = [];
for (const [index, test] of cases.entries()) {
  if (index >= 12 && !Deno.args.includes("--state")) continue;
  const original = new Terminal({ cols: 20, rows: 6, scrollback: 20, allowProposedApi: true });
  const restored = new Terminal({ cols: 20, rows: 6, scrollback: 20, allowProposedApi: true });
  const addon = new SerializeAddon();
  if (Deno.args.includes("--state")) { installHeadlessColors(original); installHeadlessColors(restored); }
  original.loadAddon(addon);
  const expectedReplies: string[] = [];
  const actualReplies: string[] = [];
  original.onData((data) => expectedReplies.push(data));
  restored.onData((data) => actualReplies.push(data));
  await write(original, test.before);
  if (test.size) original.resize(...test.size);
  expectedReplies.length = 0;
  const beforeState = captureTerminal(original);
  if (Deno.args.includes("--state")) {
    restoreTerminal(restored, beforeState);
  } else {
    await write(restored, addon.serialize());
  }
  await write(original, test.after);
  await write(restored, test.after);
  const expected = Deno.args.includes("--state") ? captureTerminal(original) : visible(original);
  const actual = Deno.args.includes("--state") ? captureTerminal(restored) : visible(restored);
  fixtures.push({ name: test.name, before: beforeState,
    after: typeof test.after === "string" ? test.after : [...test.after],
    expected: captureTerminal(original), expectedReplies });
  const equal = JSON.stringify(expected) === JSON.stringify(actual);
  const repliesEqual = JSON.stringify(expectedReplies) === JSON.stringify(actualReplies);
  results.push({ name: test.name, equal: equal && repliesEqual, ...(!equal ? { expected, actual } : {}), ...(!repliesEqual ? { expectedReplies, actualReplies } : {}) });
  original.dispose();
  restored.dispose();
}

const fixturePath = Deno.args.find((argument) => argument.startsWith("--fixtures="))?.slice(11);
if (fixturePath) await Deno.writeTextFile(fixturePath, JSON.stringify(fixtures));

console.log(JSON.stringify({
  engine: "@xterm/headless@5.5.0",
  serializer: Deno.args.includes("--state") ? fixtures[0]?.before.version : "@xterm/addon-serialize@0.13.0",
  runtime: Deno.version.deno,
  equal: results.filter((result) => result.equal).length,
  total: results.length,
  results,
}, null, 2));
