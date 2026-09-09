const endpoint = Deno.env.get("DEVELOPMENT_ACTIVATION_ENDPOINT");
const user = Deno.env.get("DEVELOPMENT_USER_ID");
const token = Deno.env.get("DEVELOPMENT_ACTIVATION_TOKEN");
let preview = false;
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
      "Usage: activate --message 'Describe the changes' [--package namespace/package]\n       activate --preview\n\nResolve conflicts in the reported Git worktree, commit the resolution, then rerun the same activation command.\nUse --json for machine-readable output.",
    );
    Deno.exit(0);
  }
  if (argument === "--json") json = true;
  else if (argument === "--preview") preview = true;
  else if (argument === "--message") description = Deno.args[++index] ?? "";
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
const operation = preview ? "preview" : "activate";
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
    }),
  },
);
const body = await response.text();
if (json) console.log(body.trim());
else {
  const result = JSON.parse(body);
  const quote = (value: string) => `'${value.replaceAll("'", "'\\''")}'`;
  if (preview) {
    for (const item of result.packages ?? []) {
      console.log(
        `${item.package_id}: ${item.changed_files} files, +${item.added_rows} / -${item.removed_rows} lines${
          item.activation_ready ? "" : " (blocked)"
        }`,
      );
    }
    if (!result.packages?.length) console.log("No private changes.");
  } else if (result.success) {
    console.log(
      result.status === "not-committed"
        ? "No changes to activate."
        : "Activation complete.",
    );
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
