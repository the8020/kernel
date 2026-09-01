# Purpose

- Expose bootstrap-administrator and authentication-session administration.

# Ownership

- Own declarative `auth bootstrap-admin` user lifecycle commands and `auth session` inspection, revocation, and cleanup commands.
- Do not own authentication storage, password hashing, cookie issuance, HTTP access enforcement, or UUI logical sessions.

# Local Contracts

- Password-changing commands use metadata-declared secret input with terminal confirmation or the explicit `--password-stdin` automation flag; passwords are never ordinary command arguments.
- `auth bootstrap-admin add` accepts an explicit username token or asks for
  `Username: ` before requesting the password when that token is omitted.
- Usernames are 3-32 lowercase ASCII letters or digits, matching the
  authentication domain's Linux-compatible identity contract.
- User lists expose username, enabled state, authentication version, timestamps, and active authentication-session count without password hashes.
- Authentication-session lists expose public identifiers, username, timestamps, validity, and authentication version without cookie secrets or stored hashes.

# Work Guidance

- Keep authentication-session terminology distinct from UUI sessions.

# Verification

- Generator, CLI, handler, authentication-domain, and application integration
  tests cover the omitted-username prompt, prompt ordering, secret input,
  delegation, error mapping, mutation, revocation, and non-disclosure.

# Child DOX Index

- `internal/AGENTS.md`: shared authentication-command dependency and error mapping.
- Leaf command folders contain one declarative command and one thin handler each.
