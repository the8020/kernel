# Purpose

- Share the small dependency and domain-error adapter used by authentication commands.

# Ownership

- Own bootstrap-authentication availability checks, typed string extraction, and stable command-bus error mapping.

# Local Contracts

- Invalid user/password/session identifiers map to invalid arguments, duplicate users to conflict, and missing users to not found; storage failures remain safe internal failures.

# Work Guidance

- Do not add authentication behavior or persistence here.

# Verification

- Authentication command-handler tests exercise each mapping through real domain errors.

# Child DOX Index
