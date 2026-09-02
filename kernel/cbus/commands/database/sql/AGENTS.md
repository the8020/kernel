# Purpose

- Implement the administrative SQL query/execute command.

# Local Contracts

- Query is the default; `--execute` explicitly selects non-row-returning SQL.
- Optional parameters are one JSON array of scalar values.
- SQL parsing and schema semantics remain database responsibilities.
