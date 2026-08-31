# Purpose

- Implement `system.status` as declared by the adjacent authoritative TOML.

# Ownership

- Own assembly of current instance, process, uptime, socket, network, logging, selected runtime mode/readiness, initialization progress/failure, graceful-shutdown progress, and build information.
- Do not cache status or mutate services.

# Local Contracts

- Public API: handler constructor `New(*services.Services) core.Handler`.
- Result field names remain aligned with `command.toml`.
- `instance_root` is the initialized node directory; source-installation paths
  are not part of process status.
- Status reads one synchronized runtime snapshot; while asynchronous runtime composition is incomplete it reports `runtime_ready=false` and the initialization-progress message without delaying the command.
- Shutdown fields report requested and restart intent, integer completed-stage
  percentage/count/total, current step, and current message; the percentage
  represents completed stages, not estimated elapsed time.

# Work Guidance

- Add fields only when the Phase status contract requires them and update TOML first.

# Verification

- Application integration validates live status identity and values.

# Child DOX Index
