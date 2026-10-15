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
  env?: Record<string, string>,
  timeout = 300_000,
  progress?: string,
): Promise<string> {
  const child = new Deno.Command(program, {
    args,
    cwd,
    env,
    stdin: "null",
    stdout: "piped",
    stderr: "inherit",
    signal: AbortSignal.any([interrupted.signal, AbortSignal.timeout(timeout)]),
  }).spawn();
  const decoder = new TextDecoder(), output: string[] = [];
  let size = 0;
  const started = Date.now();
  const heartbeat = progress
    ? setInterval(() => {
      console.log(
        `update: ${progress}; still running (${
          Math.floor((Date.now() - started) / 1000)
        }s)`,
      );
    }, 10_000)
    : undefined;
  try {
    for await (const chunk of child.stdout) {
      if (progress) {
        await Deno.stdout.write(chunk);
        continue;
      }
      size += chunk.length;
      if (size > 16 * 1024 * 1024) {
        throw new Error(`${program} output exceeds 16 MiB`);
      }
      output.push(decoder.decode(chunk, { stream: true }));
    }
    const result = await child.status;
    const response = (output.join("") + decoder.decode()).trim();
    if (!result.success) {
      throw new Error(
        `${program} failed (${result.code}): ${response.slice(-4096)}`,
      );
    }
    return response;
  } catch (error) {
    try {
      child.kill("SIGTERM");
    } catch { /* Already exited. */ }
    await child.status;
    throw error;
  } finally {
    if (heartbeat !== undefined) clearInterval(heartbeat);
  }
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
  if (compareVersions(current.version, "0.7.4") < 0) {
    throw new Error(
      "This instance predates the supported update baseline (0.7.4)",
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
  console.log("update: checking the active kernel and runtime configuration");
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
  const free = Number(
    (await command("df", ["--output=avail", "-B1", kernel])).split("\n").at(-1),
  );
  if (!Number.isFinite(free) || free < 6 * 1024 ** 3) {
    throw new Error(
      "Preparing a kernel update requires 6 GiB of free temporary disk space",
    );
  }
  const source = `${stage}/source`, instance = `${stage}/instance`;
  console.log(`update: fetching kernel ${target}`);
  await command("git", [
    "clone",
    "--depth",
    "1",
    "--single-branch",
    "--branch",
    target,
    "https://github.com/the8020/kernel.git",
    source,
  ]);
  const commit = await command("git", ["rev-parse", "HEAD"], source);
  if (
    !/^[a-f0-9]{40}$/.test(commit) ||
    await command(
        "git",
        ["rev-parse", `refs/tags/${target}^{commit}`],
        source,
      ) !== commit
  ) throw new Error("The selected version is not an exact kernel tag");
  await Deno.mkdir(instance);
  await command(`${kernel}/bin/kernel`, [
    "--root",
    instance,
    "--init-defaults",
    "--init-only",
  ]);
  const runtime = `${instance}/node/kernel/runtime`;
  console.log(
    `update: preparing kernel ${target}; the current kernel stays running`,
  );
  await command(
    "timeout",
    [
      "--kill-after=10s",
      "1800",
      "bash",
      `${source}/install.sh`,
      `--instance-root=${instance}`,
      "--skip-runtime-host",
      "--skip-verification",
    ],
    source,
    {
      GOMAXPROCS: "2",
      GOMEMLIMIT: "512MiB",
      GOFLAGS: "-p=1",
      THE8020_RELEASE_VERSION: target.split(".").slice(0, 2).join("."),
    },
    1_810_000,
    `preparing kernel ${target}`,
  );
  const unpack = `${stage}/unpacked`;
  await Deno.mkdir(`${unpack}/runtime`, { recursive: true });
  for (const name of ["bin", "scripts"]) {
    await Deno.rename(`${instance}/node/kernel/${name}`, `${unpack}/${name}`);
  }
  for (const name of ["definitions", "images"]) {
    await Deno.rename(`${runtime}/${name}`, `${unpack}/runtime/${name}`);
  }
  await Deno.rename(`${instance}/scripts`, `${unpack}/development-scripts`);
  await save(`${unpack}/release.json`, {
    version: target,
    commit,
    architecture: architecture(),
  });
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
  await Deno.remove(stage, { recursive: true });
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
  console.log("update: verifying ordinary Worker execution");
  const execution = JSON.parse(
    await command(
      `${kernel}/bin/admin`,
      [
        "--root",
        root,
        "--json",
        "runtime",
        "eval",
        "export default 42",
        "--timeout-ms",
        "15000",
      ],
      undefined,
      undefined,
      20_000,
    ),
  );
  if (execution.success !== true || execution.result?.result !== 42) {
    throw new Error(`Worker verification failed: ${JSON.stringify(execution)}`);
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
    try {
      await Deno.remove(`${kernel}/update/download`, { recursive: true });
    } catch (cleanup) {
      if (!(cleanup instanceof Deno.errors.NotFound)) {
        console.error(`Could not clean update preparation: ${cleanup}`);
      }
    }
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
