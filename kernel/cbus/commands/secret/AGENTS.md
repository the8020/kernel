# Purpose

- Expose global named-secret administration through thin command handlers.

# Ownership

- Own `secret list`, `secret get`, and `secret set` definitions and delegation.
- Do not persist values, apply them to Git, or render administration screens.

# Local Contracts

- List and set return summaries without values. Get is the sole command that
  returns a stored value.
- Set acquires its value through command-bus secret input metadata; values are
  never ordinary command tokens.

# Work Guidance

- Keep all validation and persistence in `kernel/secrets`.

# Verification

- Generated catalog and handler tests cover all leaves; `kernel/secrets` owns
  storage behavior.

# Child DOX Index
