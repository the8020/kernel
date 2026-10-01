Parent DOX: [kernel/kernel DOX](../AGENTS.md).

# Purpose

- Validate named credential references at native Git boundaries.

# Ownership

- Own only `ValidateName`. The Deno secrets package owns storage, encryption
  calls, and explicit list/get/set. Native Git composition resolves values by
  invoking its ordinary get program.
- Do not own package metadata, Git behavior, authorization policy, command-bus
  transport, or application screens.

# Local Contracts

- Names contain 1–128 ASCII letters/digits/dots/underscores/hyphens, start with
  a letter or digit, and have no surrounding whitespace.
- This package reads no database and provides no secret storage API.

# Work Guidance

- Generic private-key encryption belongs to `kernel/auth`; package code owns
  storage semantics and automatic encryption/decryption.

# Verification

- Package index tests cover native reference validation. Secrets-package tests
  cover encrypted storage; app composition tests cover ordinary-program
  resolution.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
