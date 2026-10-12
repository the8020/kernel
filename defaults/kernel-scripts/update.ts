type Release = { version: string; commit: string; architecture: string };
type Update = {
  from: string;
  target: string;
  phase: "prepared" | "offline" | "installed" | "complete";
  completed: string[];
  error?: string;
};
const interrupted = new AbortController();

export function compareVersions(left: string, right: string): number {
  const parse = (value: string) => {
    if (!/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(value)) {
      throw new Error("Version must be exactly major.minor.patch");
    }
    return value.split(".").map(BigInt);
  };
  const a = parse(left), b = parse(right);
  for (let i = 0; i < 3; i++) {
    if (a[i] !== b[i]) return a[i] > b[i] ? 1 : -1;
  }
  return 0;
}

async function exists(path: string): Promise<boolean> {
  try {
    await Deno.lstat(path);
    return true;
  } catch (error) {
    if (error instanceof Deno.errors.NotFound) return false;
    throw error;
  }
}

async function json<T>(path: string): Promise<T> {
  if ((await Deno.stat(path)).size > 65536) {
    throw new Error(`Oversized metadata: ${path}`);
  }
  return JSON.parse(await Deno.readTextFile(path));
}

async function save(path: string, value: unknown) {
  await Deno.writeTextFile(`${path}.tmp`, JSON.stringify(value) + "\n", {
    mode: 0o600,
  });
  using file = await Deno.open(`${path}.tmp`, { write: true });
  await file.sync();
  await Deno.rename(`${path}.tmp`, path);
  using directory = await Deno.open(path.slice(0, path.lastIndexOf("/")), {
    read: true,
  });
  await directory.sync();
}

async function command(
  program: string,
  args: string[],
  cwd?: string,
): Promise<string> {
  const child = new Deno.Command(program, {
    args,
    cwd,
    stdin: "null",
    stdout: "piped",
    stderr: "inherit",
    signal: AbortSignal.any([interrupted.signal, AbortSignal.timeout(300_000)]),
  }).spawn();
  const decoder = new TextDecoder(), output: string[] = [];
  let size = 0;
  try {
    for await (const chunk of child.stdout) {
      size += chunk.length;
      if (size > 16 * 1024 * 1024) {
        throw new Error(`${program} output exceeds 16 MiB`);
      }
      output.push(decoder.decode(chunk, { stream: true }));
    }
  } catch (error) {
    try {
      child.kill("SIGTERM");
    } catch { /* Already exited. */ }
    await child.status;
    throw error;
  }
  const result = await child.status;
  if (!result.success) throw new Error(`${program} failed (${result.code})`);
  return (output.join("") + decoder.decode()).trim();
}

function architecture() {
  if (Deno.build.os !== "linux") {
    throw new Error("Kernel updates require Linux");
  }
  if (Deno.build.arch === "x86_64") return "amd64";
  if (Deno.build.arch === "aarch64") return "arm64";
  throw new Error("Unsupported architecture");
}

export async function migrationScripts(
  release: string,
  from: string,
  target: string,
  online: boolean,
) {
  const base = `${release}/scripts/update`;
  if (!await exists(base)) return [];
  const versions: string[] = [];
  for await (const entry of Deno.readDir(base)) {
    if (!entry.isDirectory) continue;
    if (
      compareVersions(entry.name, from) > 0 &&
      compareVersions(entry.name, target) <= 0
    ) {
      versions.push(entry.name);
    }
  }
  versions.sort(compareVersions);
  const scripts: string[] = [];
  for (const version of versions) {
    const directory = `${base}/${version}${online ? "/after-start" : ""}`;
    if (!await exists(directory)) continue;
    const names: string[] = [];
    for await (const entry of Deno.readDir(directory)) {
      if (entry.isFile && entry.name.endsWith(".sh")) names.push(entry.name);
    }
    names.sort();
    scripts.push(...names.map((name) => `${directory}/${name}`));
  }
  return scripts;
}

async function migrations(root: string, state: Update, online: boolean) {
  const kernel = `${root}/node/kernel`,
    release = `${kernel}/releases/${state.target}`;
  for (
    const path of await migrationScripts(
      release,
      state.from,
      state.target,
      online,
    )
  ) {
    const id = path.slice(`${release}/scripts/update/`.length);
    if (state.completed.includes(id)) continue;
    console.log(`migration: ${id}`);
    const result = await new Deno.Command("timeout", {
      args: ["--signal=TERM", "--kill-after=5s", "300", "bash", path],
      cwd: root,
      env: {
        THE8020_INSTANCE_ROOT: root,
        THE8020_UPDATE_FROM: state.from,
        THE8020_UPDATE_TO: state.target,
        THE8020_UPDATE_RELEASE: release,
        PATH: `${kernel}/bin:${Deno.env.get("PATH") ?? "/usr/bin:/bin"}`,
      },
      stdin: "null",
      stdout: "inherit",
      stderr: "inherit",
      signal: interrupted.signal,
    }).spawn().status;
    if (!result.success) {
      throw new Error(
        `Migration ${id} failed (${result.code}); retry the same version after repair`,
      );
    }
    state.completed.push(id);
    await save(`${kernel}/update/state.json`, state);
  }
}

async function validateRelease(path: string, target: string) {
  const release = await json<Release>(`${path}/release.json`);
  if (
    release.version !== target || release.architecture !== architecture() ||
    !/^[a-f0-9]{40}$/.test(release.commit)
  ) {
    throw new Error(
      "Release metadata does not match the requested version and architecture",
    );
  }
  for (
    const directory of [
      "bin",
      "runtime",
      "scripts",
      "runtime/definitions",
      "runtime/images",
      "development-scripts",
    ]
  ) {
    if (!(await Deno.lstat(`${path}/${directory}`)).isDirectory) {
      throw new Error(`Missing release directory: ${directory}`);
    }
  }
  for (const binary of ["kernel", "admin", "logd", "runsc"]) {
    const info = await Deno.stat(`${path}/bin/${binary}`);
    if (!info.isFile || !((info.mode ?? 0) & 0o111)) {
      throw new Error(`Missing release binary: ${binary}`);
    }
  }
  if (
    await command(`${path}/bin/kernel`, ["--version"]) !==
      `${target}+${release.commit}`
  ) {
    throw new Error("Kernel binary does not match release metadata");
  }
  for (const file of ["update.sh", "update.ts", "launch.sh"]) {
    if (!(await Deno.stat(`${path}/scripts/${file}`)).isFile) {
      throw new Error(`Missing updater: ${file}`);
    }
  }
  return release;
}

async function status(root: string): Promise<Record<string, unknown>> {
  const response = JSON.parse(
    await command(`${root}/node/kernel/bin/admin`, [
      "--root",
      root,
      "--json",
      "kernel.status",
    ]),
  );
  if (response.success !== true || !response.result) {
    throw new Error("Cannot inspect the running kernel");
  }
  return response.result;
}

async function prepare(root: string, target: string) {
  const kernel = `${root}/node/kernel`,
    stateFile = `${kernel}/update/state.json`;
  const current = await json<Release>(`${kernel}/current/release.json`);
  compareVersions(target, current.version);
  if (target.split(".")[0] !== current.version.split(".")[0]) {
    throw new Error("Major updates require a separate installation");
  }
  if (compareVersions(target, current.version) < 0) {
    throw new Error("Kernel downgrades are unsupported");
  }
  if (compareVersions(current.version, "0.8.0") < 0) {
    throw new Error(
      "This instance predates the supported update baseline (0.8.0)",
    );
  }
  const pending = await exists(stateFile)
    ? await json<Update>(stateFile)
    : undefined;
  if (pending && pending.phase !== "complete") {
    if (pending.target !== target) {
      throw new Error(`Finish the pending update to ${pending.target} first`);
    }
    if (pending.phase === "installed") return await finish(root, pending);
    await save(`${kernel}/update/request`, target);
    if (pending.phase === "prepared") {
      await command(`${kernel}/bin/admin`, ["--root", root, "kernel.shutdown"]);
    }
    console.log(`update: resuming ${target}`);
    return;
  }
  if (target === current.version) {
    console.log(`update: ${target} is already installed`);
    return;
  }
  const launcher = (await Deno.readTextFile(`${kernel}/run/launcher.pid`))
    .trim();
  if (!/^[1-9]\d*$/.test(launcher) || !await exists(`/proc/${launcher}`)) {
    throw new Error("The installed launcher is not running");
  }
  const observed = await status(root);
  const configuration = JSON.parse(
    await command(`${kernel}/bin/admin`, [
      "--root",
      root,
      "--json",
      "kernel.config.get",
      "sandbox.runtime.mode",
    ]),
  );
  if (
    configuration.success !== true ||
    configuration.result?.setting?.configured_value !== "rootless"
  ) {
    throw new Error(
      "In-place updates currently support rootless instances only",
    );
  }
  if (observed.shutdown_requested) {
    throw new Error("The kernel is already shutting down");
  }
  const stage = `${kernel}/update/download`,
    destination = `${kernel}/releases/${target}`;
  await Deno.remove(stage, { recursive: true }).catch((error) => {
    if (!(error instanceof Deno.errors.NotFound)) throw error;
  });
  await Deno.mkdir(stage, { recursive: true, mode: 0o700 });
  const filename = `the8020-kernel-${target}-linux-${architecture()}.tar.gz`;
  const url =
    `https://github.com/the8020/kernel/releases/download/${target}/${filename}`;
  console.log(`update: downloading ${target}`);
  for (const suffix of [".sha256", ""]) {
    await command("curl", [
      "--fail",
      "--location",
      "--silent",
      "--show-error",
      "--connect-timeout",
      "10",
      "--max-time",
      "300",
      "--max-filesize",
      suffix ? "256" : "1073741824",
      "--retry",
      "2",
      "--output",
      `${stage}/${filename}${suffix}`,
      url + suffix,
    ]);
  }
  const checksum = await Deno.readTextFile(`${stage}/${filename}.sha256`);
  if (checksum.length > 256) throw new Error("Oversized release checksum file");
  if (
    !new RegExp(`^[a-f0-9]{64}  ${filename.replaceAll(".", "\\.")}\\n?$`).test(
      checksum,
    )
  ) throw new Error("Invalid release checksum file");
  await command(
    "sha256sum",
    ["--check", "--status", `${filename}.sha256`],
    stage,
  );
  const entries = (await command("tar", ["-tzf", `${stage}/${filename}`]))
    .split("\n");
  for (const raw of entries) {
    const name = raw.replace(/^\.\//, "");
    if (!name || name === ".") continue;
    if (
      name.startsWith("/") || name.split("/").includes("..") ||
      !/^(release\.json$|(bin|scripts|runtime|development-scripts)(\/|$))/.test(
        name,
      )
    ) {
      throw new Error(`Unsafe release archive entry: ${name}`);
    }
  }
  const listing = await command("tar", [
    "--numeric-owner",
    "-tvzf",
    `${stage}/${filename}`,
  ]);
  let size = 0;
  for (const line of listing.split("\n")) {
    const match = line.match(/^([dlh-])\S*\s+\S+\s+(\d+)\s/);
    if (!match) throw new Error("Unsupported release archive entry");
    size += Number(match[2]);
  }
  const free = Number(
    (await command("df", ["--output=avail", "-B1", kernel])).split("\n").at(-1),
  );
  if (!Number.isFinite(free) || free < size + 64 * 1024 * 1024) {
    throw new Error("Insufficient disk space for the staged release");
  }
  const unpack = `${stage}/unpacked`;
  await Deno.mkdir(unpack);
  await command("tar", [
    "-xzf",
    `${stage}/${filename}`,
    "--delay-directory-restore",
    "-C",
    unpack,
  ]);
  await validateRelease(unpack, target);
  await migrationScripts(unpack, current.version, target, false);
  await migrationScripts(unpack, current.version, target, true);
  const image = `${unpack}/runtime/images/rootless`;
  if (await exists(`${image}/smoke.json`)) {
    await Deno.remove(`${image}/smoke.json`);
  }
  await command("timeout", [
    "--kill-after=5s",
    "30",
    "bash",
    `${unpack}/runtime/definitions/smoke-portable.sh`,
    `${unpack}/bin/runsc`,
    `${image}/rootfs`,
    `${image}/image.json`,
    `${image}/smoke.json`,
    `${stage}/smoke`,
  ]);
  await Deno.remove(destination, { recursive: true }).catch((error) => {
    if (!(error instanceof Deno.errors.NotFound)) throw error;
  });
  await Deno.rename(unpack, destination);
  await save(
    stateFile,
    {
      from: current.version,
      target,
      phase: "prepared",
      completed: [],
    } satisfies Update,
  );
  await save(`${kernel}/update/request`, target);
  await command(`${kernel}/bin/admin`, ["--root", root, "kernel.shutdown"]);
  using stopped = await Deno.open(`${kernel}/run/kernel.lock`, {
    create: true,
    write: true,
    mode: 0o600,
  });
  let released = false;
  for (let attempt = 0; attempt < 1200; attempt++) {
    if (stopped.tryLockSync(true)) {
      stopped.unlockSync();
      released = true;
      break;
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  if (!released) {
    await Deno.remove(`${kernel}/update/request`);
    throw new Error(
      "Graceful shutdown exceeded two minutes; the update was not applied",
    );
  }
  console.log(
    `update: ${target} staged; launcher will stop, migrate, and start the kernel`,
  );
}

async function apply(root: string, state: Update) {
  const kernel = `${root}/node/kernel`;
  using stopped = await Deno.open(`${kernel}/run/kernel.lock`, {
    create: true,
    write: true,
    mode: 0o600,
  });
  if (!stopped.tryLockSync(true)) {
    throw new Error("The kernel is still running; refusing to migrate");
  }
  await Deno.remove(`${kernel}/update/request`);
  if (state.phase === "installed") {
    if (await exists(`${kernel}/update/offline`)) {
      await Deno.remove(`${kernel}/update/offline`);
    }
    return;
  }
  if (!await exists(`${kernel}/update/offline`)) {
    await Deno.writeTextFile(`${kernel}/update/offline`, state.target);
  }
  state.phase = "offline";
  delete state.error;
  await save(`${kernel}/update/state.json`, state);
  await validateRelease(`${kernel}/releases/${state.target}`, state.target);
  await migrations(root, state, false);
  const selected = await Deno.readLink(`${kernel}/current`);
  if (selected !== `releases/${state.target}`) {
    if (selected !== `releases/${state.from}`) {
      throw new Error("The installed release changed during the update");
    }
    if (await exists(`${kernel}/current.next`)) {
      await Deno.remove(`${kernel}/current.next`);
    }
    await Deno.symlink(`releases/${state.target}`, `${kernel}/current.next`);
    await Deno.rename(`${kernel}/current.next`, `${kernel}/current`);
    using directory = await Deno.open(kernel, { read: true });
    await directory.sync();
  }
  state.phase = "installed";
  await save(`${kernel}/update/state.json`, state);
  await Deno.remove(`${kernel}/update/offline`);
  console.log(`update: installed ${state.target}`);
}

async function finish(root: string, state: Update) {
  if (state.phase === "complete") return;
  if (state.phase !== "installed") {
    throw new Error("The offline update has not finished");
  }
  const kernel = `${root}/node/kernel`;
  const release = await json<Release>(`${kernel}/current/release.json`);
  let observed: Record<string, unknown> = {};
  for (let attempt = 0; attempt < 300; attempt++) {
    observed = await status(root);
    if (observed.build_id !== `${state.target}+${release.commit}`) break;
    if (observed.runtime_ready === true) break;
    if (
      observed.runtime_failure &&
      observed.runtime_failure !== "runtime initialization is in progress"
    ) break;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  if (
    observed.build_id !== `${state.target}+${release.commit}` ||
    observed.runtime_ready !== true
  ) {
    throw new Error(
      "The new kernel/runtime is not ready; repair it and retry the same version",
    );
  }
  await migrations(root, state, true);
  if (observed.runtime_application_failure) {
    console.log(
      `update: application initialization needs attention: ${observed.runtime_application_failure}`,
    );
  }
  state.phase = "complete";
  delete state.error;
  await save(`${kernel}/update/state.json`, state);
  await Deno.remove(`${kernel}/update/download`, { recursive: true }).catch(
    (error) => {
      if (!(error instanceof Deno.errors.NotFound)) throw error;
    },
  );
  for await (const entry of Deno.readDir(`${kernel}/releases`)) {
    if (
      entry.name !== state.from && entry.name !== state.target &&
      entry.isDirectory
    ) {
      await Deno.remove(`${kernel}/releases/${entry.name}`, {
        recursive: true,
      });
    }
  }
  console.log(`update: completed ${state.target}`);
}

if (import.meta.main) {
  const [root, action, ...extra] = Deno.args;
  if (!root || !action || extra.length) {
    throw new Error("usage: update.sh <major.minor.patch>");
  }
  const kernel = `${await Deno.realPath(root)}/node/kernel`;
  const stop = () => interrupted.abort(new Error("Update interrupted"));
  Deno.addSignalListener("SIGTERM", stop);
  Deno.addSignalListener("SIGINT", stop);
  await Deno.mkdir(`${kernel}/update`, { recursive: true, mode: 0o700 });
  using lock = await Deno.open(`${kernel}/update/lock`, {
    create: true,
    write: true,
    mode: 0o600,
  });
  if (!lock.tryLockSync(true)) {
    if (action !== "--apply") console.error("Another updater is running");
    Deno.exit(75);
  }
  try {
    if (action === "--apply") {
      await apply(root, await json<Update>(`${kernel}/update/state.json`));
    } else if (action === "--finish") {
      await finish(root, await json<Update>(`${kernel}/update/state.json`));
    } else await prepare(root, action);
  } catch (error) {
    const path = `${kernel}/update/state.json`;
    if (await exists(path)) {
      const state = await json<Update>(path);
      if (state.phase !== "complete") {
        state.error = error instanceof Error ? error.message : String(error);
        await save(path, state);
      }
    }
    console.error(error instanceof Error ? error.message : error);
    Deno.exitCode = 1;
  } finally {
    Deno.removeSignalListener("SIGTERM", stop);
    Deno.removeSignalListener("SIGINT", stop);
  }
}
