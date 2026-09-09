// Sandbox Git helper called by the UUI program backend; terminal resolution
// uses this same worktree and index.
const limit = 48 * 1024;
const decode = new TextDecoder("utf-8", { fatal: true });
const encode = new TextEncoder();

async function bounded(stream: ReadableStream<Uint8Array>, maximum: number) {
  const chunks: Uint8Array[] = [];
  let size = 0;
  for await (const chunk of stream) {
    size += chunk.length;
    if (size > maximum) {
      throw new Error("Response too large; use the terminal for this file.");
    }
    chunks.push(chunk);
  }
  const bytes = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.length;
  }
  return bytes;
}

async function main() {
  const request = JSON.parse(
    decode.decode(await bounded(Deno.stdin.readable, 64 * 1024)),
  );
  const worktree = request.worktree;
  if (
    typeof worktree !== "string" ||
    !/^\/workspace\/packages\/\.conflicts\/[a-f0-9]{24}\/[a-zA-Z0-9_-][a-zA-Z0-9._-]*\/[a-zA-Z0-9_-][a-zA-Z0-9._-]*$/
      .test(worktree)
  ) {
    throw new Error("Invalid activation worktree.");
  }
  const root = await Deno.realPath(worktree);
  if (root !== worktree) {
    throw new Error("Activation worktree must be an ordinary directory.");
  }
  async function git(...args: string[]) {
    const child = new Deno.Command("/usr/bin/git", {
      args: ["-C", root, ...args],
      stdout: "piped",
      stderr: "piped",
    }).spawn();
    try {
      const [stdout, stderr, status] = await Promise.all([
        bounded(child.stdout, 1024 * 1024),
        bounded(child.stderr, 64 * 1024),
        child.status,
      ]);
      if (!status.success) {
        throw new Error(decode.decode(stderr) || "Git command failed.");
      }
      return decode.decode(stdout);
    } catch (error) {
      try {
        child.kill("SIGKILL");
      } catch { /* already exited */ }
      throw error;
    }
  }
  const unmerged = await git("ls-files", "-u", "-z");
  const entries = new Map<string, Map<number, string>>();
  for (const entry of unmerged.split("\0").filter(Boolean)) {
    const tab = entry.indexOf("\t");
    const [mode, oid, stage] = entry.slice(0, tab).split(" ");
    const name = entry.slice(tab + 1);
    const stages = entries.get(name) ?? new Map<number, string>();
    stages.set(Number(stage), `${mode} ${oid}`);
    entries.set(name, stages);
  }
  if (request.action === "list") {
    return {
      files: [...entries].map(([path, stages]) => ({
        path,
        kind: stages.has(2) && stages.has(3)
          ? "Both changed"
          : stages.has(2)
          ? "Deleted upstream"
          : stages.has(1)
          ? "Deleted by you"
          : "Added upstream",
      })),
    };
  }
  if (request.action === "finish") {
    if (entries.size) throw new Error("Resolve all conflicting files first.");
    let merging = false;
    try {
      await Deno.stat(
        (await git("rev-parse", "--git-path", "MERGE_HEAD")).trim(),
      );
      merging = true;
    } catch (error) {
      if (!(error instanceof Deno.errors.NotFound)) throw error;
    }
    if (
      merging ||
      (await git("status", "--porcelain", "--untracked-files=no")).trim()
    ) {
      await git(
        "-c",
        "user.name=" + (Deno.env.get("DEVELOPMENT_USER_ID") ?? "Developer"),
        "-c",
        "user.email=development@the8020.local",
        "commit",
        "-m",
        "Resolve activation conflicts",
      );
    }
    return { resolved: true };
  }
  const path = request.path;
  if (
    typeof path !== "string" || !path || path.startsWith("/") ||
    path.split("/").some((part: string) =>
      !part || part === "." || part === ".." || part === ".git"
    ) || path.includes("\0")
  ) {
    throw new Error("Invalid conflict file.");
  }
  if (!entries.has(path)) {
    throw new Error(
      "This conflict changed or was already resolved. Refresh the file list.",
    );
  }
  const filename = `${root}/${path}`;
  // Resolve every existing parent before reading or saving; no symlink escape.
  let parent = filename.slice(0, filename.lastIndexOf("/"));
  while (true) {
    try {
      const real = await Deno.realPath(parent);
      if (real !== root && !real.startsWith(root + "/")) {
        throw new Error("Conflict path escapes the worktree.");
      }
      break;
    } catch (error) {
      if (!(error instanceof Deno.errors.NotFound)) throw error;
      parent = parent.slice(0, parent.lastIndexOf("/"));
    }
  }
  let bytes = new Uint8Array();
  let binary = false;
  let exists = false;
  let stamp: unknown = null;
  try {
    const info = await Deno.lstat(filename);
    stamp = [info.dev, info.ino, info.size, info.mtime, info.ctime];
    exists = true;
    binary = !info.isFile || info.size > limit;
    if (!binary) bytes = await Deno.readFile(filename);
  } catch (error) {
    if (!(error instanceof Deno.errors.NotFound)) throw error;
  }
  let content = "";
  try {
    content = decode.decode(bytes);
    binary ||= content.includes("\0");
  } catch {
    binary = true;
  }
  const status = await git("status", "--porcelain", "--", path);
  const signature = encode.encode(
    JSON.stringify([unmerged, status, exists, binary, stamp, [...bytes]]),
  );
  const version = [
    ...new Uint8Array(await crypto.subtle.digest("SHA-256", signature)),
  ].map((value) => value.toString(16).padStart(2, "0")).join("");
  if (request.action === "read") {
    const sides: Record<string, string | null> = {};
    for (
      const [name, stage] of [["original", 1], ["private", 2], [
        "shared",
        3,
      ]] as const
    ) {
      const entry = entries.get(path)!.get(stage);
      if (!entry) {
        sides[name] = null;
        continue;
      }
      const [, oid] = entry.split(" ");
      if (Number(await git("cat-file", "-s", oid)) > limit) {
        sides[name] = null;
        continue;
      }
      try {
        const text = await git("cat-file", "blob", oid);
        sides[name] = text.includes("\0") ? null : text;
      } catch {
        sides[name] = null;
      }
    }
    return {
      path,
      content,
      binary,
      exists,
      version,
      ...sides,
      hasPrivate: entries.get(path)!.has(2),
      hasShared: entries.get(path)!.has(3),
    };
  }
  if (request.version !== version) {
    throw new Error(
      "The file changed in the terminal. Refresh before saving your resolution.",
    );
  }
  if (request.action === "delete") {
    await git("rm", "-f", "--sparse", "--", path);
  } else if (request.action === "private" || request.action === "shared") {
    const stage = request.action === "private" ? 2 : 3;
    if (!entries.get(path)!.has(stage)) {
      throw new Error(
        "That version is deleted. Use Delete file to accept deletion.",
      );
    }
    await git(
      "checkout",
      "--ignore-skip-worktree-bits",
      stage === 2 ? "--ours" : "--theirs",
      "--",
      path,
    );
    await git("add", "--sparse", "--", path);
  } else if (request.action === "save") {
    if (
      binary || typeof request.content !== "string" ||
      encode.encode(request.content).length > limit
    ) throw new Error("Use a side selection or the terminal for this file.");
    if (/^(<{7}|\|{7}|={7}|>{7})( |$)/m.test(request.content)) {
      throw new Error("Remove all Git conflict markers before saving.");
    }
    await Deno.mkdir(filename.slice(0, filename.lastIndexOf("/")), {
      recursive: true,
    });
    const temporary = `${filename}.resolve-${crypto.randomUUID()}`;
    try {
      const mode = entries.get(path)!.get(2)?.startsWith("100755")
        ? 0o755
        : 0o644;
      await Deno.writeTextFile(temporary, request.content, {
        createNew: true,
        mode,
      });
      await Deno.rename(temporary, filename);
    } catch (error) {
      // Preserve the save error; cleanup only concerns our temporary file.
      await Deno.remove(temporary).catch(() => undefined);
      throw error;
    }
    await git("add", "--sparse", "--", path);
  } else throw new Error("Unknown conflict action.");
  return { resolved: true };
}

if (import.meta.main) {
  try {
    console.log(JSON.stringify(await main()));
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    Deno.exit(1);
  }
}
