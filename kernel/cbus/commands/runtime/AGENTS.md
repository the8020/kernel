# Purpose

- Expose Phase 1B runtime diagnostics, image state, administrative execution, and aggregate status.

# Ownership

- Own declarative `runtime doctor`, `runtime image status`, `runtime eval`, `runtime run`, and `runtime status` handlers, including selected isolation mode/capability reporting.
- Do not probe hosts, execute code, or manage sandboxes directly; delegate to typed runtime services.

# Local Contracts

- Doctor remains usable while runtime lifecycle initialization is degraded and reports full and rootless readiness separately.
- Runtime status reports isolation/readiness plus aggregate sandbox, Worker, port, and warm-pool counts; it never embeds per-resource records already available through list and inspect commands.
- Eval/run always submit sandboxed runtime work and never invoke host Deno.
- Eval/run may forward one explicit instance-root-bounded development workspace
  to the ordinary job path; host writes remain opt-in.
- Eval/run show state, program result, emitted logs, and human-readable duration by default; `--detail` exposes the complete artifact, execution, permission, timing, and sandbox resource record.

# Work Guidance

- Return structured reports suitable for human and JSON rendering, with concise defaults and explicit detailed diagnostics.

# Verification

- Generator and handler tests cover every command and degraded runtime behavior.

# Child DOX Index

- This domain contract owns its leaf command folders; they contain only one declarative command and thin handler each.
