import type { PersistentServiceTarget } from "./services.ts";

/** Physical terminals have kernel lifetime. Labels and display state are package-owned. */
export interface TerminalSize {
  columns: number;
  rows: number;
}

export interface TerminalInfo {
  id: string;
  sessionId?: string;
  kind: "development" | "runtime";
  sandboxId: string;
  size: TerminalSize;
  sequence: number;
  exited: boolean;
  exitStatus?: number;
}

export interface TerminalCreateInput {
  kind: TerminalInfo["kind"];
  sandboxId: string;
  arguments: string[];
  environment?: string[];
  workingDir: string;
  size: TerminalSize;
}

export interface TerminalAttachment {
  terminal: TerminalInfo;
  attachmentId: string;
}

export interface TerminalOpenInput extends TerminalCreateInput {
  sessionId: string;
  owner: PersistentServiceTarget;
}

export type TerminalOpenResult =
  | (TerminalAttachment & { after: number; reset: boolean })
  | { terminal: TerminalInfo; owner: PersistentServiceTarget };

export interface TerminalEvent {
  sequence: number;
  data?: Uint8Array;
  size?: TerminalSize;
}

export interface TerminalBatch {
  events: TerminalEvent[];
  sequence: number;
  exited: boolean;
  exitStatus?: number;
}

export interface TerminalViewRequest {
  viewId: string;
  sequence: number;
  size: TerminalSize;
}

export class TerminalControlBusyError extends Error {
  constructor() {
    super("Terminal already has an input controller");
  }
}

export class TerminalClosedError extends Error {
  constructor() {
    super("Terminal closed");
  }
}

type Operation = <T>(
  operation: string,
  input: Record<string, unknown>,
  signal?: AbortSignal,
) => Promise<T>;

function encode(data: Uint8Array): string {
  if (data.length < 1 || data.length > 65_536) {
    throw new TypeError("terminal input must contain 1..65536 bytes");
  }
  return data.toBase64();
}

/** @internal Installed by the kernel SDK over its trusted Worker bridge. */
export function terminalAPI(callOperation: Operation) {
  const operation: Operation = async <T>(
    name: string,
    input: Record<string, unknown>,
    signal?: AbortSignal,
  ): Promise<T> => {
    const result = await callOperation<T | { closed: true }>(
      name,
      input,
      signal,
    );
    if (
      result && typeof result === "object" && "closed" in result &&
      result.closed === true
    ) {
      throw new TerminalClosedError();
    }
    return result as T;
  };
  return Object.freeze({
    /** Reuse or create one sandbox-scoped session; a live processor retains ownership. */
    open(
      input: TerminalOpenInput,
      signal?: AbortSignal,
    ): Promise<TerminalOpenResult> {
      return operation("terminal.open", { ...input }, signal);
    },
    create(
      input: TerminalCreateInput,
      signal?: AbortSignal,
    ): Promise<TerminalAttachment> {
      // The canonical processor exists before the first native output read.
      return operation("terminal.create", { ...input }, signal);
    },
    list(
      kind: TerminalInfo["kind"],
      sandboxId: string,
      signal?: AbortSignal,
    ): Promise<TerminalInfo[]> {
      return operation("terminal.list", { kind, sandboxId }, signal);
    },
    inspect(terminalId: string, signal?: AbortSignal): Promise<TerminalInfo> {
      return operation("terminal.inspect", { terminalId }, signal);
    },
    async attach(
      terminalId: string,
      mode: "control" | "take-control" | "observe" | "process",
      after = 0,
      signal?: AbortSignal,
    ): Promise<TerminalAttachment> {
      const result = await operation<TerminalAttachment | { busy: true }>(
        "terminal.attach",
        { terminalId, mode, after },
        signal,
      );
      if ("busy" in result) throw new TerminalControlBusyError();
      return result;
    },
    /** Wait for a native display stream; only the canonical processor may accept it. */
    nextView(
      attachmentId: string,
      signal?: AbortSignal,
    ): Promise<TerminalViewRequest> {
      return operation("terminal.view-next", { attachmentId }, signal);
    },
    /** Send outside the parser queue. Bound pending display data and slow transfers. */
    async writeView(
      attachmentId: string,
      viewId: string,
      data: Uint8Array,
      signal?: AbortSignal,
    ): Promise<void> {
      await operation("terminal.view-write", {
        attachmentId,
        viewId,
        data: encode(data),
      }, signal);
    },
    async finishView(
      attachmentId: string,
      viewId: string,
      signal?: AbortSignal,
    ): Promise<void> {
      await operation("terminal.view-finish", { attachmentId, viewId }, signal);
    },
    async read(
      attachmentId: string,
      after: number,
      signal?: AbortSignal,
    ): Promise<TerminalBatch> {
      if (!Number.isSafeInteger(after) || after < 0) {
        throw new TypeError("invalid terminal sequence");
      }
      type WireBatch = Omit<TerminalBatch, "events"> & {
        events: Array<Omit<TerminalEvent, "data"> & { data?: string }>;
      };
      const batch = await operation<WireBatch>("terminal.read", {
        attachmentId,
        after,
      }, signal);
      return {
        ...batch,
        events: batch.events.map(({ data, ...event }) => ({
          ...event,
          ...(data === undefined ? {} : { data: Uint8Array.fromBase64(data) }),
        })),
      };
    },
    /** Await native consumption before sending another frame. Never retry an uncertain write. */
    async write(
      attachmentId: string,
      data: Uint8Array,
      signal?: AbortSignal,
    ): Promise<void> {
      await operation(
        "terminal.write",
        { attachmentId, data: encode(data) },
        signal,
      );
    },
    /** Canonical query replies acknowledge bounded admission, so output can keep draining. */
    async respond(
      attachmentId: string,
      data: Uint8Array,
      signal?: AbortSignal,
    ): Promise<void> {
      await operation(
        "terminal.respond",
        { attachmentId, data: encode(data) },
        signal,
      );
    },
    async resize(
      attachmentId: string,
      size: TerminalSize,
      signal?: AbortSignal,
    ): Promise<void> {
      await operation("terminal.resize", { attachmentId, size }, signal);
    },
    async detach(attachmentId: string): Promise<void> {
      await operation("terminal.detach", { attachmentId });
    },
    async close(
      target: string | { terminalId: string; nodeId: string },
      signal?: AbortSignal,
    ): Promise<void> {
      await operation(
        "terminal.close",
        typeof target === "string" ? { terminalId: target } : { ...target },
        signal,
      );
    },
  });
}
