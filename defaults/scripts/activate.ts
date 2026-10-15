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
const selected_packages: string[] = [];
const package_messages: Record<string, string> = {};
for (let index = 0; index < Deno.args.length; index++) {
  const argument = Deno.args[index];
  if (argument === "--help" || argument === "-h") {
    console.log(
      "Usage: activate --message 'Describe the changes' [--package namespace/package] [--force table-id.column]\n       activate --preview [--schema] [--package namespace/package]\n\n--force may repeat. Cleanup and native activation run first. Only incompatible cells in exact authorized columns may use NULL or an authored target default. Rows, other cells, keys and identity are retained; unsupported repair stops. Requires --package.\nUse --json for machine-readable output.",
    );
    Deno.exit(0);
  }
  if (argument === "--json") json = true;
  else if (argument === "--preview") preview = true;
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
if (!preview && !description.trim()) {
  throw new Error("an activation description is required");
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
  if (preview) {
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
    if (!result.packages?.length && !result.schema_plan) {
      console.log("No private changes.");
    }
  } else if (result.success) {
    console.log(
      result.status === "not-committed"
        ? "No changes to activate."
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
    }
    if (result.status === "conflicted") {
      console.error(
        `\nThen rerun: activate ${
          Deno.args.map(quote).join(" ")
        }\nYou can also resolve these files in Development → Review changes → Resolve conflicts.`,
      );
    }
  }
}
if (!response.ok) Deno.exit(response.status === 409 ? 3 : 1);
