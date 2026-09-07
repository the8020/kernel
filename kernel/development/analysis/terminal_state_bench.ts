// Bounded Phase 1 component measurements; excludes Go, transport, and UI work.
// deno-lint-ignore-file no-explicit-any
import headless from "npm:@xterm/headless@5.5.0";
import { cpuUsage } from "node:process";
import { captureTerminal, installHeadlessColors, restoreTerminal } from "./xterm_state.ts";

const { Terminal } = headless;
const write = (t: any, data: string) => new Promise<void>((resolve) => t.write(data, resolve));
const collect = () => (globalThis as any).gc?.();
const fresh = (cols = 80, rows = 24) => {
  const terminal = new Terminal({ cols, rows, scrollback: 5000, allowProposedApi: true });
  installHeadlessColors(terminal);
  return terminal;
};

const warmup = fresh();
await write(warmup, "warm\r\n".repeat(1000));
warmup.dispose();
collect();
const baseline = Deno.memoryUsage();
const idle = Array.from({ length: 8 }, () => fresh());
collect();
const idleUsage = Deno.memoryUsage();
for (const terminal of idle) terminal.dispose();

const samples = [];
for (const [cols, rows] of [[80, 24], [160, 48]]) {
  const terminal = fresh(cols, rows);
  const line = "\x1b[38;2;45;170;92m" + "abcd汉字".repeat(Math.ceil(cols / 8)) + "\x1b[0m\r\n";
  const chunk = line.repeat(256);
  const bytes = new TextEncoder().encode(chunk).length * 32;
  const startCPU = cpuUsage();
  const started = performance.now();
  for (let i = 0; i < 32; i++) await write(terminal, chunk);
  const parseMs = performance.now() - started;
  const parseCPU = cpuUsage(startCPU);
  collect();
  const retained = Deno.memoryUsage();
  const snapshotSamples = [];
  for (let trial = 0; trial < 5; trial++) {
    const startCPU = cpuUsage();
    const started = performance.now();
    const snapshot = captureTerminal(terminal);
    const captureMs = performance.now() - started;
    const encodeStart = performance.now();
    const encoded = new TextEncoder().encode(JSON.stringify(snapshot));
    const gzip = await new Response(new Blob([encoded]).stream().pipeThrough(new CompressionStream("gzip"))).arrayBuffer();
    const encodeMs = performance.now() - encodeStart;
    const target = fresh();
    const restoreStart = performance.now();
    restoreTerminal(target, snapshot);
    const restoreMs = performance.now() - restoreStart;
    snapshotSamples.push({ captureMs, encodeMs, restoreMs, jsonBytes: encoded.length, gzipBytes: gzip.byteLength, cpu: cpuUsage(startCPU) });
    target.dispose();
    collect();
  }
  samples.push({ cols, rows, retainedLines: terminal.buffer.normal.length, bytes, parseMs,
    parseMiBPerSecond: bytes / (1 << 20) / (parseMs / 1000), parseCPU, retained, snapshotSamples });
  terminal.dispose();
  collect();
}

console.log(JSON.stringify({ runtime: Deno.version, engine: "@xterm/headless@5.5.0", baseline,
  idleTerminals: 8, idleUsage, idleHeapDeltaPerTerminal: (idleUsage.heapUsed - baseline.heapUsed) / 8,
  idleExternalDeltaPerTerminal: (idleUsage.external - baseline.external) / 8,
  note: "GC-assisted component measurements in one Deno process; RSS is process-wide, and later cases share allocator history. Prototype JSON snapshots are not the production wire format.", samples }, null, 2));
