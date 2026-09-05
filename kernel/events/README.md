# Package events and hooks

Events are declared in flat `events/*.toml` files. Filenames are arbitrary.
Every listener requires an `event`, a `description`, and a full
`namespace/package/program` ID in `program`:

```toml
# events/notify.toml
event = "order-created"
description = "Notify about a created order"
program = "acme/orders/notify"
```

The referenced program uses its ordinary `programs/notify/program.toml` manifest
to select its TypeScript entrypoint. Handler folders contain TOML declarations;
executable source belongs in programs.

```ts
// programs/notify/program.ts
import type { PackageEvent } from "@the8020/kernel";
export default function (event: PackageEvent<{ orderId: string }>) {
  console.log(event.name, event.data.orderId, event.nodeId, event.occurredAt);
}
```

Programs may belong to another package. Activation validates their manifests and
entrypoints before publication. Discovery rejects TypeScript handlers, unknown
TOML fields, missing descriptions/programs, traversal, and symlinks. The kernel
caches ready declarations and target commits at startup and package publication.

Emit from Deno:

```ts
import { kernel } from "@the8020/kernel";
const receipt = await kernel.events.emit("order-created", { orderId: "123" });
```

Or from the kernel command bus:

```text
kernel.events.emit order-created --data '{"orderId":"123"}'
kernel.events.emit minute
```

Both return `{id, listeners}` after admission, without waiting for program
admission or completion. Delivery is local to the current node. Each program
receives one independent JSON event object: `{id,name,nodeId,occurredAt,data}`.
Omitted data is null. Deno emissions inherit the emitting user; native commands
use system identity. A command invoked from Deno retains its execution user.
Errors are logged per listener and do not fail siblings or an accepted emitter.

Each kernel emits `minute` as system at seconds 00, with its UTC minute boundary
in occurredAt and null data. Timer startup follows full runtime publication.
Host clock changes recalculate the next boundary; elapsed minute events are not
replayed. Every node owns its timer; no leader is elected.

Events are memory-only. Shutdown cancels outstanding listeners. The catalog is
bounded to 2,048 listeners, event data to 64 KiB, and outstanding dispatch to
4,096 listeners. Admission over that bound fails immediately without partial
delivery. Listener execution has a ten-minute bound including job admission.
Application packages own durable queues, retries, and idempotence where needed.

Hooks use flat `hooks/*.toml` files with an explicit `hook` field:

```toml
# hooks/ensure-system-user.toml
hook = "post-activate"
description = "Create the built-in system user when absent"
program = "the8020/users/ensure-system-user"
```

Hooks run synchronously in activation order. Their programs receive one context
argument with `package_id`, `previous_commit`, `candidate_commit`,
`first_activation`, and `activation_id`, identifying the declaring package's
activation. Referenced programs resolve from the candidate set before ready
installed packages, including references to another candidate package. Staged
hooks use read-only candidate mounts. Existing durable phase records, failure
behavior, and recovery of unfinished hooks remain in force.

Hooks support `pre-activate` and `post-activate`, with one handler per phase per
package. Duplicate hook declarations are rejected. Events allow multiple
listeners for the same event. Nested declaration folders are rejected.

The existing reindex entry point refreshes commands and both handler indexes:

```text
kernel.reindex
kernel.reindex --packages the8020/jobs,the8020/users
```

Omit the selection for a full rebuild. A selection replaces those packages'
fragments, removing deleted declarations while preserving unselected handlers.
Cached references to programs in changed packages also receive the new target
commit. Invalid handler declarations leave both handler indexes unchanged.

The kernel invokes this same entry point on boot, after activation (including
checkout/pull/synchronization), and after local source convergence to a shared
package revision. Normal emission never scans declaration files. Both activation
phases use an isolated index of the validated candidate declarations until source
publication. Results include total `events` and `hooks` counts alongside the
existing command report. Reindexing itself never executes hooks.
