# Purpose

- Expose Phase 1B warm-pool accounting and desired-size changes.

# Ownership

- Own declarative status/resize handlers for the real warm-sandbox controller.

# Local Contracts

- Profile keys are immutable registered profile hashes; resize triggers real sandbox provisioning or trimming through the warm-pool owner.

# Work Guidance

- Return desired, ready, creating, reserved, assigned, failed, and replenish counts.

# Verification

- Generated validation and handler tests cover status and validation.

# Child DOX Index

- This domain contract owns its leaf command folders; they contain only one declarative command and thin handler each.
