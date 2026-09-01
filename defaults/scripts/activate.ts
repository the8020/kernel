const endpoint = Deno.env.get("DEVELOPMENT_ACTIVATION_ENDPOINT");
const user = Deno.env.get("DEVELOPMENT_USER_ID");
const token = Deno.env.get("DEVELOPMENT_ACTIVATION_TOKEN");
if (!endpoint || !user || !token) {
  throw new Error("activate is available only inside a development sandbox");
}

let preview = false;
let description = "";
let authorName = "";
let authorEmail = "";
const selected_packages: string[] = [];
const package_messages: Record<string, string> = {};
for (let index = 0; index < Deno.args.length; index++) {
  const argument = Deno.args[index];
  if (argument === "--preview") preview = true;
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
console.log(body.trim());
if (!response.ok) Deno.exit(response.status === 409 ? 3 : 1);
