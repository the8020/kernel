# Sandbox startup qualification

Owner: [runtime DOX](AGENTS.md). Updated 2026-09-08 on Linux ARM64, 6 CPUs, Deno
2.9.4, and rootless gVisor.

## Implemented behavior

- Services and jobs execute without startup type checking. Explicit table
  dependency inspection still uses `deno info`; schema descriptors and runtime
  exports retain their normal validation.
- One ordinary indexing job passes the full selected package set through all
  ordered hooks in one Worker. Handlers share state; package fragments publish
  independently, with explicit errors retaining the previous accepted fragment.
- Bootstrap's completed full index is reused. Inherited workload restoration
  happens before any new schema or package jobs can start.
- Container bootstrap runs initial-account handling once per persistent volume,
  then skips user commands on subsequent starts. Its initial check parses user
  JSON with bundled Deno, preserving existing users regardless of field order.

The measurements below used 200 ms supervisor readiness polling and one-second
Docker login readiness retries. Production defaults now use 10 ms and 100 ms,
respectively, with the same timeout budgets. These tables retain the earlier
measurements; they do not quantify this later polling change. Parallel
per-package jobs and the lazy dynamic-import option were **not adopted**.

## Complete startup

Timing starts at the real Docker entrypoint and ends after initial-user handling
and HTTP 200 from the public login service. Runs use isolated mount/user
namespaces and an extracted container image, excluding Docker daemon and image
build time. The bundled gVisor smoke record is already valid. Fresh runs have
new databases and empty node caches; restarts retain both. Host filesystem and
network caches remain outside the experiment's control.

| Scenario             | Earlier shared-cache baseline | Implemented median | Final samples         |
| -------------------- | ----------------------------: | -----------------: | --------------------- |
| Fresh initialization |                      33.327 s |            8.796 s | 8.796, 8.927, 8.628 s |
| Normal restart       |                      14.495 s |            3.248 s | 3.450, 3.145, 3.248 s |

All six final runs returned login HTTP 200. Fresh boots created the initial
admin account and downloaded 17 resources each; restarts preserved that account
and downloaded nothing. Fresh state contains 12 package records, eight service
records, and eight service versions.

The earlier baseline used the fixed `f4c6fe0` source plus shared file caching.
Final measurements use the current coordinated kernel and package worktrees,
including concurrent service-update work. This is a same-host before/after
comparison, not an isolated attribution of every timing difference to one patch.
Preliminary runs and failed restart diagnostics are excluded from these medians.

## Individual new sandboxes

Three sequential samples per case with downloaded files cached. Each service
case starts a new sandbox and measures request to first HTTP 200, including
module loading, OpenAPI discovery, and Worker readiness. Import probes execute
ordinary module jobs; the `users.list` case executes the actual program.

| Workload                                      | Earlier baseline | Implemented median |
| --------------------------------------------- | ---------------: | -----------------: |
| Empty job, admission through execution        |          0.403 s |            0.432 s |
| Empty job, complete admin command and cleanup |          0.520 s |            0.518 s |
| Service-index import, complete admin command  |          0.932 s |            0.942 s |
| Actual `users.list`, complete admin command   |          0.904 s |            0.943 s |
| Minimal HTTP service                          |          1.133 s |            0.543 s |
| HTTP service importing DB SDK                 |          1.938 s |            0.694 s |
| HTTP service importing login module           |          3.043 s |            0.856 s |

Subsequent HTTP responses take approximately 3–6 ms. Ordinary jobs had no
mandatory type check before this change, so their unchanged timings are
expected. Table-evaluator jobs explicitly requested checks previously; that
checking path has been removed too. Sandbox provisioning, readiness polling,
module loading, and per-sandbox private Deno metadata remain measurable costs.
Unused literal dynamic imports still expand the users/UUI/admin dependency set;
this change does not introduce import rewriting or alter dependency resolution.

After changing readiness polling to 10 ms internally and 100 ms for Docker
login, a focused rootless gVisor smoke passed three new empty program sandboxes
and three new minimal service sandboxes. Their complete-call medians were
0.498 s and 0.514 s, respectively. The existing-instance restart returned login
HTTP 200 in 2.727 s with no downloads. Artifacts are in
`/tmp/8020-cache-benchmark/readiness-10ms`, using `readiness-kernel`,
`measure-readiness.py`, and `individual-readiness.py`. This smoke verifies the
new defaults; it does not replace the full fresh/restart benchmark above.

## Empty-sandbox keepalive

`runtime.sandbox.keep_alive` now retains empty service and job/module sandboxes
for 120000 milliseconds by default. Allocation protects pending Worker startup;
the last Worker leaving starts a new countdown. Existing maintenance processes
the cached idle queue every second without polling healthy supervisors. Zero
removes the retention delay; cleanup still waits for the authoritative empty
snapshot and maintenance pass when that snapshot arrives after owner release.
Explicit lifecycle cleanup bypasses the delay. Jobs, modules, and package
programs use the same default compatible group.

Three sequential cold/reuse pairs per workload used the same binary, cached
downloads, and the two-minute setting. Every reused invocation received a fresh
Worker while retaining the sandbox ID and native supervisor task PID. Service
fixtures used a 200 ms Worker keepalive and returned to zero Workers and owners
before the next measured request.

| Workload | New sandbox median | Retained supervisor median | Retained samples |
| --- | ---: | ---: | --- |
| Module, complete command through Worker cleanup | 459 ms | 77 ms | 69, 77, 85 ms |
| Minimal service, request to HTTP 200 | 560 ms | 94 ms | 108, 94, 89 ms |

A control run with retention disabled and confirmed sandbox deletion before
each repeat measured 435 ms for repeated module commands and 546 ms for repeated
service starts. A direct module and its synchronous child package program also
ran concurrently in the same default sandbox with distinct Workers.

A separate two-second keepalive run verified both workload types, then started
a three-second Worker halfway through the idle countdown. It survived the
original deadline, reset the countdown on completion, and its sandbox expired
2.875 seconds later, within the one-second maintenance cadence. Unit/race tests
also cover warm assignment, stale snapshots, allocation-versus-expiry races,
failed ownership rollback, deletion retry, and explicit deletion.
The final binary repeated the same native checks successfully, including the
configured two-second value and default 120000 ms read through kernel settings;
observed deletion took 3.072 seconds including maintenance, native cleanup, and
command polling. Its artifacts are in `keepalive-final-smoke`.

Alternating fresh/restart pairs compared zero retention with two minutes using
the same binary and runtime image, three pairs per setting:

| Scenario | Zero retention median | Two-minute median | Two-minute samples |
| --- | ---: | ---: | --- |
| Fresh initialization | 7.897 s | 7.898 s | 7.898, 7.989, 7.890 s |
| Normal restart | 2.557 s | 2.524 s | 2.527, 2.524, 2.325 s |

The comparison does not show a material whole-boot improvement. Both settings
use shared default grouping, and short gaps can still reuse a sandbox before
the empty snapshot/cleanup pass with zero retention. Repeated workload startup
is the measured benefit. All twelve boot runs returned login HTTP 200; fresh
runs downloaded 17 resources and created the initial user, while restarts
downloaded none and preserved it. Zero-retention samples were
10.171/7.689/7.897 seconds fresh and 2.557/2.438/3.764 seconds on restart; host
load and network variation are included rather than filtered out.

Artifacts are under `/tmp/8020-cache-benchmark/keepalive-performance`,
`keepalive-disabled-checked`, `keepalive-short-rerun`, and
`keepalive-{enabled,disabled}-{fresh,restart}-{1,2,3}`. Reproduction uses
`measure-keepalive.py`, `individual-keepalive.py`, and `startup-current.py` with
`THE8020_RUNTIME_SANDBOX_KEEP_ALIVE` set to `0`, `2000`, or `120000`.

## One-time account bootstrap

Container startup now records completed initial-account handling in
`node/docker/initial-user.done`. Later starts skip `users.list`, `users.add`,
and their JSON parser. An existing volume without the marker performs the
initial check once, preserving its existing accounts. Failed creation remains
retryable; later deletion or disabling of accounts never triggers automatic
recreation.

One fresh native gVisor run completed in 7.190 seconds and created the initial
account. Three restarts of that volume completed in 1.926, 1.925, and 1.919
seconds, with zero user-program executions and zero downloads. All returned
login HTTP 200. These use the same kernel binary as the keepalive qualification;
the entrypoint and staged application worktrees are current, so the earlier
2.524-second restart median is contextual rather than a controlled attribution.

On those restarts the first job sandbox runs service indexing. From kernel boot
to its ready supervisor took 261–293 ms; the remaining indexing and runtime
initialization took 674–740 ms. The login service then needed 209–233 ms for
its own sandbox/supervisor and 502–524 ms from Worker creation to readiness.
Worker readiness includes its imported application modules. The earlier direct
index-hook profile separately measured median import time of 615 ms and handler
time of 147 ms for twelve packages.

The previous `users.list` startup was a second job sandbox overlapping the
first indexing sandbox. One representative restart spent about 388 ms from
user-job admission to supervisor readiness, 662 ms loading its Worker, and
15 ms executing the query. Fresh bootstrap starts with table-schema evaluation
instead. These are complete application startup costs, not a fixed one-second
Worker creation cost; the small retained-supervisor module qualification above
completed in 77 ms.

Artifacts are in `/tmp/8020-cache-benchmark/bootstrap-once-fresh` and
`bootstrap-once-restart-{1,2,3}`, reproduced with `startup-once.py` and
`keepalive-final-kernel`. The shell regression also checks restart with all
users removed, existing-account preservation, malformed results, and retry after
failed account creation. Docker daemon build/run was not exercised.

## Verification and reproduction

- Kernel Go unit suite passed; runtime Deno suite passed all 107 tests; services
  checks and all seven tests passed. Container entrypoint tests cover HTTP
  readiness, existing users with intervening JSON fields, enabled/password flags
  belonging to different users, and invalid responses.
- Go indexing regressions assert one invocation for multiple packages, targeted
  repair, cancellation, foreign-scope rejection, and independent retention of
  invalid fragments. Deno hook tests assert ordered handlers, identical object
  and Worker identity, frozen nested scope, and failure stopping the chain.
- Real gVisor executed statically invalid jobs and explicit dependency
  inspection. Hook results also passed ordered execution, same-object/Worker
  identity, updated-handler loading, and propagated failure checks. The broader
  gVisor suite still fails its log-reference retrieval assertion; it is not
  reported as a passing suite.
- All validation used disposable instances and high ports. No live deployment
  was changed and Docker daemon build/run verification was not performed.

Local artifacts are in `/tmp/8020-cache-benchmark/`: `batch-kernel`,
`batch-image`, `startup-current.py`, `measure-current.py`,
`run-image-current.sh`, `batch-fixed-fresh-{1,2,3}`,
`batch-fixed-restart-{1,2,3}`, and `batch-individual`. Each trial records
`result.json` and startup logs; the individual run also records
`individual.json`. The harness stages current package files into clean
disposable Git repositories.

Example repeat on this host, with a new unused label:

```sh
python3 /tmp/8020-cache-benchmark/startup-current.py batch-repeat \
  /tmp/8020-cache-benchmark/batch-kernel \
  /tmp/8020-cache-benchmark/batch-image
```

Append an existing trial's `instance` path to exercise restart. The real runtime
regression command and toolchain requirements are recorded in
[the rootless backend contract](../../../kernel/sandbox/backend/rootless/AGENTS.md).
