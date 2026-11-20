#!/usr/bin/env bash
set -euo pipefail

# Older installed updaters cannot execute the new online prerequisite check.
"$THE8020_INSTANCE_ROOT/node/kernel/runtime/images/rootless/rootfs/usr/bin/deno" eval --quiet --no-config '
  const [root, release, target] = Deno.args;
  const path = `${root}/node/kernel/update/prerequisites.json`;
  try {
    if ((await Deno.stat(path)).size > 65536) throw new Error("oversized receipt");
    const checked = JSON.parse(await Deno.readTextFile(path));
    const installed = JSON.parse(await Deno.readTextFile(`${release}/release.json`));
    if (checked.target !== target || checked.commit !== installed.commit) throw new Error("wrong candidate");
  } catch {
    throw new Error("Package prerequisites were not checked online for this candidate; the kernel pointer has not switched. Use the target update helper and its --preflight action against the original kernel before retrying. Preserve the update and activation journals.");
  }
' "$THE8020_INSTANCE_ROOT" "$THE8020_UPDATE_RELEASE" "$THE8020_UPDATE_TO"
