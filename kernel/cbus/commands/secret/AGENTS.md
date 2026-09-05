Parent DOX: [kernel/kernel/cbus/commands DOX](../AGENTS.md).

# Purpose

- Adapt named-secret primitives for package runtime operations.

# Ownership

- Do not publish CBus metadata; `the8020/secrets` owns visible `secrets.*`
  command programs.
- Own private list/get/set delegation.
- Do not persist values, apply them to Git, or render administration screens.

# Local Contracts

- List and set return summaries without values. Get is the sole command that
  returns a stored value.
- The package command acquires set values through execution-scoped secure input;
  values are never ordinary command tokens.

# Work Guidance

- Keep all validation and persistence in `kernel/secrets`.

# Verification

- Generated catalog and handler tests cover all leaves; `kernel/secrets` owns
  storage behavior.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
