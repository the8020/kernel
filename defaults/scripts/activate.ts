const endpoint = Deno.env.get("DEVELOPMENT_ACTIVATION_ENDPOINT");
const user = Deno.env.get("DEVELOPMENT_USER_ID");
const token = Deno.env.get("DEVELOPMENT_ACTIVATION_TOKEN");
let preview = false;
let schema_preview = false;
const force_columns: string[] = [];
let json = false;
let description = "";
let authorName = "";
let authorEmail = "";
let action = "";
let attempt_id = "";
const selected_packages: string[] = [];
const package_messages: Record<string, string> = {};
for (let index = 0; index < Deno.args.length; index++) {
  const argument = Deno.args[index]!;
  if (argument === "--help" || argument === "-h") {
    console.log(
      "Usage: activate --message 'Describe the changes' [--package namespace/package] [--force table-id.column]\n       activate --preview [--schema] [--package namespace/package]\n       activate --status\n       activate --continue [--message 'Describe the changes']\n       activate --abort [--attempt ID]\n\nAbort preserves private edits and distinct resolution work; safely disposable generated staging is removed. Committed schema changes require forward recovery; abort cannot undo them. Legacy attempts need explicit disposition. Cleanup retries need no message.\n--force may repeat. Cleanup and native activation run first. Only incompatible cells in exact authorized columns may use NULL or an authored target default. Rows, other cells, keys and identity are retained; unsupported repair stops. Requires --package.\nUse --json for machine-readable output.",
    );
    Deno.exit(0);
  }
  if (argument === "--json") json = true;
  else if (["--status", "--continue", "--abort"].includes(argument)) {
    if (action) throw new Error("Select only one lifecycle action");
    action = argument.slice(2);
  } else if (argument === "--attempt") {
    attempt_id = Deno.args[++index] ?? "";
    if (!attempt_id || attempt_id.startsWith("--")) {
      throw new Error("--attempt requires an ID");
    }
  } else if (argument === "--preview") preview = true;
  else if (argument === "--schema") schema_preview = true;
  else if (argument === "--force") {
    const table = Deno.args[++index];
    if (!table || table.startsWith("--")) {
      throw new Error("--force requires an exact table.column");
    }
    force_columns.push(table);
  } else if (argument === "--message") description = Deno.args[++index] ?? "";
  else if (argument === "--package") {
    selected_packages.push(Deno.args[++index] ?? "");
  } else if (argument === "--package-message") {
    const raw = Deno.args[++index] ?? "";
    const separator = raw.indexOf("=");
    if (separator < 1) {
      throw new Error("--package-message requires package=message");
    }
    package_messages[raw.slice(0, separator)] = raw.slice(separator + 1);
  } else if (argument === "--author-name") {
    authorName = Deno.args[++index] ?? "";
  } else if (argument === "--author-email") {
    authorEmail = Deno.args[++index] ?? "";
  } else if (argument.startsWith("-")) {
    throw new Error(`unknown option ${argument}`);
  } else if (!description) description = argument;
  else throw new Error(`unexpected argument ${argument}`);
}
if (!endpoint || !user || !token) {
  throw new Error("activate is available only inside a development sandbox");
}
if (!preview && !action && !description.trim()) {
  throw new Error("an activation description is required");
}
if (
  action && preview ||
  (action === "status" || action === "abort") && force_columns.length
) {
  throw new Error(
    "Status and abort cannot force activation; lifecycle actions cannot preview",
  );
}
if (attempt_id && action !== "abort") {
  throw new Error("--attempt requires --abort");
}
if (schema_preview && !preview) throw new Error("--schema requires --preview");
if (force_columns.length && !selected_packages.length) {
  throw new Error("--force requires --package");
}
const operation = preview ? "preview" : "activate";
if (force_columns.length) {
  const capability = await fetch(
    `${endpoint}/v1/development/sandboxes/${user}/preview`,
    {
      method: "POST",
      headers: {
        authorization: `Bearer ${token}`,
        "content-type": "application/json",
      },
      body: JSON.stringify({ selected_packages }),
    },
  );
  if (
    !capability.ok || (await capability.json()).schema_options_version !== 2
  ) {
    throw new Error(
      "Force requires an updated kernel; no activation was submitted",
    );
  }
}
const response = await fetch(
  `${endpoint}/v1/development/sandboxes/${user}/${operation}`,
  {
    method: "POST",
    headers: {
      authorization: `Bearer ${token}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({
      description,
      action,
      attempt_id,
      selected_packages,
      package_messages,
      author_name: authorName,
      author_email: authorEmail,
      schema_preview,
      force_columns,
    }),
  },
);
const body = await response.text();
if (json) console.log(body.trim());
else {
  const result = JSON.parse(body);
  const quote = (value: string) => `'${value.replaceAll("'", "'\\''")}'`;
  const inventory = result.retained ?? [];
  for (const retained of inventory) {
    console.log(
      `${retained.attempt_id}: ${retained.status} — ${retained.worktree}`,
    );
    if (retained.recovery) console.log(retained.recovery);
    if (retained.status === "legacy-unresolved") {
      console.log(
        `  Recover/export using ordinary Git, or preserve explicitly: activate --abort --attempt ${
          quote(retained.attempt_id)
        }`,
      );
    }
  }
  if (result.inventory_truncated) {
    console.log(
      "Inventory limited to 128 entries; inspect retained directories in the terminal.",
    );
  }
  if (action === "status") {
    console.log(
      `${result.status}${
        result.attempt_id ? ` (${result.attempt_id}, ${result.phase})` : ""
      }`,
    );
    if (result.error) console.log(result.error);
    for (const item of result.packages ?? []) {
      console.log(
        `  ${item.package_id}: ${item.status} ${item.worktree ?? ""}`,
      );
    }
  } else if (result.status === "aborted-preserved") {
    console.log(
      "Attempt closed. Files and captures are preserved for recovery/export. Current private changes remain pending.",
    );
  } else if (result.success && result.phase === "aborted") {
    console.log("Attempt closed. Current private changes remain pending.");
  } else if (preview) {
    if (result.pending) {
      console.log(`${result.pending.status}: ${result.pending.error}`);
    }
    if (result.schema_plan) {
      for (const table of result.schema_plan.tables) {
        console.log(
          `${table.table_id}: ${table.route}; ${
            table.reasons.join("; ") || "automatic"
          }; ${
            table.affected_columns.map((c: { name: string; repair: string }) =>
              `${c.name}: ${
                c.repair === "unsupported"
                  ? "no automatic repair"
                  : `invalid cells may use ${c.repair}`
              }`
            ).join("; ")
          }`,
        );
      }
    }
    for (const item of result.packages ?? []) {
      console.log(
        `${item.package_id}: ${item.changed_files} files, +${item.added_rows} / -${item.removed_rows} lines${
          item.activation_ready ? "" : " (blocked)"
        }`,
      );
    }
    if (!result.pending && !result.packages?.length && !result.schema_plan) {
      console.log("No private changes.");
    }
  } else if (result.success) {
    console.log(
      result.status === "not-committed"
        ? "No changes to activate."
        : result.resumed
        ? "Captured changes activated. Review remaining workspace changes."
        : "Activation complete.",
    );
    for (
      const [table, receipt] of Object.entries(
        result.schema_result ?? {},
      ) as Array<[string, { route: string; columns?: string[] }]>
    ) {
      console.log(
        `  ${table}: ${
          receipt.route === "repair"
            ? `incompatible cells repaired in ${
              receipt.columns?.join(", ") ?? "authorized columns"
            }`
            : receipt.route === "cleanup"
            ? "cleanup and schema applied"
            : "schema applied"
        }`,
      );
    }
    for (const item of result.packages ?? []) {
      console.log(
        `  ${item.package_id}: ${
          item.resulting_commit ? item.resulting_commit.slice(0, 12) : "deleted"
        }`,
      );
    }
  } else {
    console.error(
      result.status === "conflicted"
        ? "Activation needs conflict resolution. Your changes are preserved."
        : `Activation failed: ${result.error ?? result.status ?? body}`,
    );
    for (const item of result.packages ?? []) {
      console.error(`\n${item.package_id}`);
      for (const path of item.conflicts ?? []) {
        console.error(`  CONFLICT ${path}`);
      }
      if (item.conflict_worktree) {
        console.error(
          `\n  cd ${
            quote(item.conflict_worktree)
          }\n  git status\n  # Edit the conflicting files; use git rm for a deletion.\n  git add -A\n  git commit -m 'Resolve activation conflicts'`,
        );
      } else if (item.error) console.error(item.error);
      else if (item.worktree) {
        console.error(`  Inspect preserved staging: ${item.worktree}`);
      }
    }
    if (result.status === "conflicted") {
      console.error(
        `\nThen continue: activate --continue --message ${
          quote(description)
        }\nOr preserve and leave this attempt: activate --abort\nYou can also resolve these files in Development → Review changes → Resolve conflicts.`,
      );
    }
  }
}
if (!response.ok) Deno.exit(response.status === 409 ? 3 : 1);
