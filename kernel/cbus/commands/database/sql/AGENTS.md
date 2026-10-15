Parent DOX: [kernel/kernel/cbus/commands/database DOX](../AGENTS.md).

# Purpose

- Adapt native SQL query/execute for private runtime operations and
  authenticated development-sandbox ingress.

# Local Contracts

- Query is the default; `--execute` explicitly selects non-row-returning SQL.
- Optional parameters are one JSON array of scalar values.
- SQL parsing and schema semantics remain database responsibilities.
- `NewHTTP` adapts the same handler for the development manager's private `sql`
  operation. Composition supplies it; the development owner verifies the sandbox
  token before dispatch. It is not a public HTTP service.
- Requests contain `statement`, `parameters` as a JSON-array string, and
  `execute`. Reject malformed, unknown, trailing, or over-1-MiB input. Preserve
  native parameter/result integers, existing row/byte bounds, cancellation, and
  a 30-second deadline. Query mode is not a read-only authorization boundary.
- SQL does not require application schema initialization. Neither HTTP nor the
  CLI retries failed operations.

# Verification

- `sql_test.go` runs the production adapter against disposable SQLite without
  application initialization, covering queries, mutations, exact integers,
  request validation, and result bounds. With Deno on PATH it also runs the
  shipped dev-skills SQL CLI through HTTP, checking output and failure exits.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
