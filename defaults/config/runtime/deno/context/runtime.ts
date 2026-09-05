import type { ExecutionContext } from "./types.ts";

type ContextProvider = () => ExecutionContext | undefined;

let provider: ContextProvider | undefined;

// The bridge installs one provider for the lifetime of an application Worker.
// A second library cannot replace it while that Worker is active.
export function installContextProvider(
  next: ContextProvider,
): () => void {
  if (provider !== undefined) {
    throw new Error("execution context provider is already installed");
  }
  provider = next;
  return () => {
    if (provider === next) provider = undefined;
  };
}

export function currentExecutionContext(): ExecutionContext {
  const value = provider?.();
  if (value === undefined) {
    throw new Error("execution context is only available inside an invocation");
  }
  return value;
}
