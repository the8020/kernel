# Purpose

- Emit local asynchronous package events and the minute-boundary clock event.

# Ownership

- Own bounded asynchronous dispatch, failure
  reporting, and minute timer. Packages own listener behavior and durable state.
- The package owner indexes flat `events/*.toml` and `hooks/*.toml`
  declarations by their required `event` or `hook` field. Dispatch uses a
  memory-only lookup in that shared handler index.
  Existing activation hooks retain synchronous ordering and failure behavior.

# Local Contracts

- Emit returns an event ID and accepted listener count without waiting for
  execution, Worker admission, or listener completion. Each listener invokes its
  declared full program ID through the ordinary program runner and validates its
  cached target commit; errors are independent and logged. Delivery is memory-only.
- Emission is local to this node. Kernel minute events use system identity;
  Deno emissions retain the emitting user. `kernel.events.emit` on the command
  bus uses the same dispatcher, with system identity for native commands.
  Payloads are copied bounded JSON inside one event argument to each program.
- Every kernel schedules the next UTC minute boundary (seconds 00), recalculates
  after each wake, and never replays elapsed minute events. Host clocks own time.
- The shared reindex entry point refreshes declarations at boot, explicit
  reindex, activation, and source convergence. Dispatch never reads declarations
  and caps outstanding listener jobs.
- Start follows complete runtime publication. Close stops emission, cancels
  outstanding jobs, and joins them before the runtime is torn down.

# Verification

- Go tests cover parallel nonblocking delivery, identity/payload isolation,
  failure isolation, capacity, shutdown, listener discovery, and clock alignment.

# Child DOX Index
