# Purpose

- Own structured kernel file logging and runtime rotation/retention policy.

# Ownership

- Own the `slog` logger, concurrent writer, UTC period splitting, record-preserving size rotation, owned-file cleanup, enable/disable, policy preparation, and graceful closure.
- Do not log command-specific presentation or delete non-kernel files.

# Local Contracts

- Public API: `ErrInitialization`, `Policy`, `Manager`, `New`, `PolicyFromValues`, `Logger`, `ActiveFile`, `Enabled`, `Write`, `Prepare`, and `Close`.
- Files are `kernel-*.log` under `node/kernel/logs`; cleanup never deletes the
  active file and removes oldest closed owned files first.
- A replacement writer is opened before persistence, atomically swapped on commit, and removed on discard.
- UTC boundaries support none, minute, hour, day, week, month, and year.
- Extend only for a current kernel logging acceptance criterion.

# Work Guidance

- Use `log/slog` and this small writer; do not add a logging framework.

# Verification

- `logging_test.go` covers all period boundaries, default-enabled file creation, size rotation, total cleanup, active preservation, disable/re-enable, policy replacement failure, and prepared-file discard.

# Child DOX Index
