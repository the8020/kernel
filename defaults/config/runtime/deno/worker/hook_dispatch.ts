export interface HookReference {
  readonly id: string;
  readonly entrypoint: string;
}

// An ordinary job entrypoint. All handlers share this invocation's state and
// execution context; only the input and final output cross the Worker boundary.
export default async function dispatch(
  handlers: readonly HookReference[],
  scope: Readonly<Record<string, unknown>>,
  state: Record<string, unknown>,
): Promise<Record<string, unknown>> {
  Object.freeze(scope);
  for (const handler of handlers) {
    try {
      const module = await import(handler.entrypoint);
      if (typeof module.default !== "function") {
        throw new TypeError("hook program must default-export a function");
      }
      await module.default(state, scope);
    } catch (cause) {
      const message = cause instanceof Error ? cause.message : String(cause);
      throw new Error(`hook ${handler.id} failed: ${message}`, { cause });
    }
  }
  return state;
}
