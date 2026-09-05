# Purpose

- Expose the immutable logical execution context available to package code as
  `@the8020/context`.

# Local Contracts

- `authenticated` is the synchronous application-policy result already
  established during protected request setup; it is false for public requests
  and jobs. Reading any context property never checks a token, account, or
  session.

- Context is invocation-scoped. Concurrent requests in one Worker must observe
  different values through the Worker bridge's asynchronous context.
- Package code receives getters and frozen snapshots only. Installing or
  replacing the provider is an internal runtime operation.
- `type` and `id` identify the outer platform execution that entered the Worker:
  service, job, or standalone package program. Package-local concepts such as a
  UUI session remain owned by their package.
- Every active context has a user validated by the kernel. Anonymous requests
  use the service's assigned user; jobs use an explicit or inherited identity.
  The runtime never supplies a default user for missing metadata.

# Verification

- Kernel bridge and RuntimeWorker tests cover isolation, immutability, service
  users, job users, and program origins.
