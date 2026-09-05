# Purpose

- Implement `kernel.events.emit` through the shared local event dispatcher.

# Local Contracts

- Parse the optional JSON data using shared command helpers. The event owner
  validates names, payload bounds, and asynchronous admission.
- Native commands use system identity; nested Deno commands inherit their user.
- Return the same event ID and listener count as the Deno event API. Never wait
  for a listener or broadcast to other nodes.

# Verification

- Handler tests cover payload delivery, asynchronous completion, identity,
  omitted data, invalid requests, and unavailable runtime state. Generator tests
  validate command metadata and examples.

# Child DOX Index
