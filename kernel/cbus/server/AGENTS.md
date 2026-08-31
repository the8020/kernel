# Purpose

- Expose the command registry over HTTP/JSON on the instance Unix socket.

# Ownership

- Own socket binding/mode, request decoding, response encoding, dispatch calls, shutdown rejection, and transport shutdown.
- Do not validate command semantics or execute domain services directly.

# Local Contracts

- Public API: `Server`, `New`, `Start`, `BeginShutdown`, and `Shutdown`.
- Only `POST /v1/cbus/execute` is served; request bodies are bounded and the socket is created under a restrictive umask, verified as mode `0600`, and also chmodded where the filesystem supports socket chmod.
- A filesystem that returns `EINVAL` for socket chmod is accepted only when the
  owning `node/kernel/run` directory is mode `0700`; normal Unix filesystems
  must expose socket mode `0600`.
- `BeginShutdown` rejects new commands with `SHUTTING_DOWN` except an explicit
  read/idempotent allowlist, keeping status plus repeated shutdown/restart
  requests available during application cleanup. `Shutdown` then waits for
  active requests and closes/removes the socket; `app` coordinates both phases.
- A future transport must call the same `core.Registry`, not copy dispatch.

# Work Guidance

- Keep standard `net/http` and `net`; do not add a web framework.

# Verification

- `server_test.go` uses the real client over a temporary Unix socket and checks mode, typed dispatch, errors, mutation rejection during drain, and allowed status dispatch.

# Child DOX Index
