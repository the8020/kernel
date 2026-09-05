Parent DOX: [kernel/kernel/settings DOX](../AGENTS.md).

# Purpose

- Persist explicit system-wide setting values and the settings revision in the
  package-owned system tables.

# Local Contracts

- Missing settings receive their compiled recommended default exactly once.
- A later definition/default change updates metadata but never replaces the
  stored value.
- Value changes and revision increments commit in one transaction.
- This package contains no setting definitions and never handles node-local
  configuration.
- JSON parameters use the shared kernel database encoder; this repository does
  not embed engine-specific SQL syntax.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
