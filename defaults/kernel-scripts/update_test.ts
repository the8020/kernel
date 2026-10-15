import { compareVersions, migrationScripts } from "./update.ts";

const source = new URL(".", import.meta.url).pathname;
const commit = "0".repeat(40);
const arch = Deno.build.arch === "x86_64" ? "amd64" : "arm64";
const assert = (value: unknown, message = "Assertion failed") => {
  if (!value) throw new Error(message);
};
const read = (path: string) => Deno.readTextFile(path);

async function run(
  program: string,
  args: string[],
  env?: Record<string, string>,
) {
  const output = await new Deno.Command(program, {
    args,
    env,
    stdout: "piped",
    stderr: "piped",
  }).output();
  return {
    code: output.code,
    output: new TextDecoder().decode(output.stdout) +
      new TextDecoder().decode(output.stderr),
  };
}

async function fixture(baseline = "0.8.0") {
  const temp = await Deno.makeTempDir({ prefix: "kernel-update-" });
  const root = `${temp}/instance`, kernel = `${root}/node/kernel`;
  const versions = `${temp}/versions`, tools = `${temp}/tools`;
  for (
    const directory of [
      versions,
      tools,
      `${kernel}/releases`,
      `${kernel}/run`,
      `${kernel}/runtime`,
      `${root}/packages`,
      `${root}/users`,
      `${root}/database`,
    ]
  ) await Deno.mkdir(directory, { recursive: true });
  for (
    const file of [
      "kernel.toml",
      "packages/keep",
      "users/keep",
      "database/keep",
    ]
  ) await Deno.writeTextFile(`${root}/${file}`, "keep");
  await Deno.writeTextFile(`${kernel}/run/launcher.pid`, String(Deno.pid));
  await Deno.writeTextFile(
    `${tools}/git`,
    `#!/bin/bash
set -eu
if [[ "$1" == clone ]]; then
  [[ ! -f "$FIXTURE_ROOT/fail-git" ]] || exit 2
  version=; previous=
  for argument in "$@"; do
    [[ "$previous" != --branch ]] || version=$argument
    previous=$argument
  done
  cp -a "$FIXTURE_VERSIONS/$version" "${"${!#}"}"
else printf '%040d\\n' 0; fi
`,
    { mode: 0o755 },
  );
  const env = {
    FIXTURE_VERSIONS: versions,
    FIXTURE_ROOT: temp,
    THE8020_INSTANCE_ROOT: root,
    THE8020_USERNAME: "bootstrap-only",
    THE8020_PASSWORD: "bootstrap-only",
    PATH: `${tools}:${Deno.env.get("PATH")}`,
  };
  async function release(
    version: string,
    scripts: Record<string, string> = {},
  ) {
    const directory = `${versions}/${version}`;
    for (
      const name of [
        "bin",
        "scripts/update",
        "development-scripts",
        "runtime/definitions",
        "runtime/images/rootless/rootfs/usr/bin",
      ]
    ) await Deno.mkdir(`${directory}/${name}`, { recursive: true });
    await Deno.writeTextFile(
      `${directory}/release.json`,
      JSON.stringify({ version, commit, architecture: arch }),
    );
    await Deno.writeTextFile(
      `${directory}/runtime/definitions/smoke-portable.sh`,
      "#!/bin/bash\nexit 0\n",
    );
    await Deno.writeTextFile(
      `${directory}/runtime/images/rootless/image.json`,
      "{}",
    );
    await Deno.writeTextFile(
      `${directory}/bin/kernel`,
      `#!/bin/bash
if [[ "$1" == --version ]]; then echo '${version}+${commit}'; exit; fi
[[ -z "${"${THE8020_USERNAME:-}"}" && -z "${"${THE8020_PASSWORD:-}"}" ]] || exit 2
root=$2
mkdir -p "$root/packages" "$root/users"
touch "$root/kernel.toml"
`,
      { mode: 0o755 },
    );
    await Deno.writeTextFile(
      `${directory}/bin/admin`,
      `#!/bin/bash
if [[ "$*" == *kernel.shutdown* ]]; then echo stop >> "$THE8020_INSTANCE_ROOT/stops"; echo '{"success":true}';
elif [[ "$*" == *kernel.config.get* ]]; then echo '{"success":true,"result":{"setting":{"configured_value":"rootless"}}}';
elif [[ "$*" == *"runtime eval"* ]]; then
  if [[ -f "$FIXTURE_ROOT/fail-worker" ]]; then echo '{"success":true,"result":{"state":"FAILED"}}';
  else echo '{"success":true,"result":{"result":42}}'; fi
else echo '{"success":true,"result":{"build_id":"${version}+${commit}","runtime_mode":"rootless","runtime_ready":true}}'; fi
`,
      { mode: 0o755 },
    );
    for (const name of ["logd", "runsc"]) {
      await Deno.writeTextFile(
        `${directory}/bin/${name}`,
        "#!/bin/bash\nexit 0\n",
        { mode: 0o755 },
      );
    }
    for (const name of ["update.sh", "update.ts", "launch.sh"]) {
      await Deno.copyFile(`${source}/${name}`, `${directory}/scripts/${name}`);
    }
    await Deno.symlink(
      Deno.execPath(),
      `${directory}/runtime/images/rootless/rootfs/usr/bin/deno`,
    );
    for (const [name, body] of Object.entries(scripts)) {
      const file = `${directory}/scripts/update/${name}`;
      await Deno.mkdir(file.slice(0, file.lastIndexOf("/")), {
        recursive: true,
      });
      await Deno.writeTextFile(file, body);
    }
    return directory;
  }
  async function version(
    version: string,
    scripts: Record<string, string> = {},
  ) {
    const directory = await release(version, scripts);
    await Deno.writeTextFile(
      `${directory}/install.sh`,
      `#!/bin/bash
set -eu
[[ ! -f "$FIXTURE_ROOT/fail-build" ]] || exit 2
root=
for argument in "$@"; do
  case "$argument" in --instance-root=*) root=${"${argument#*=}"} ;; esac
 done
if [[ -f "$FIXTURE_ROOT/wait-build" ]]; then
  echo 'installer stdout is live'
  while [[ -f "$FIXTURE_ROOT/wait-build" ]]; do sleep 0.05; done
fi
[[ -f "$root/kernel.toml" && -d "$root/packages" && ! -f "$root/packages/keep" ]]
mkdir -p "$root/node/kernel" "$root/scripts"
for name in bin scripts runtime; do
  cp -a "$PWD/$name" "$root/node/kernel/"
done
`,
      { mode: 0o755 },
    );
    return directory;
  }
  const initial = await release(baseline);
  await Deno.rename(initial, `${kernel}/releases/${baseline}`);
  await Deno.symlink(`releases/${baseline}`, `${kernel}/current`);
  for (const name of ["bin", "scripts"]) {
    await Deno.symlink(`current/${name}`, `${kernel}/${name}`);
  }
  for (const name of ["definitions", "images"]) {
    await Deno.symlink(
      `../current/runtime/${name}`,
      `${kernel}/runtime/${name}`,
    );
  }
  await Deno.symlink(
    "node/kernel/current/development-scripts",
    `${root}/scripts`,
  );
  return {
    temp,
    root,
    kernel,
    env,
    version,
    update: (action: string) =>
      run("bash", [`${kernel}/scripts/update.sh`, action], env),
    state: async () => JSON.parse(await read(`${kernel}/update/state.json`)),
    cleanup: () => Deno.remove(temp, { recursive: true }),
  };
}

Deno.test("installer stdout and heartbeat are visible before preparation finishes", async () => {
  const f = await fixture();
  await f.version("0.8.1");
  const gate = `${f.temp}/wait-build`;
  await Deno.writeTextFile(gate, "wait");
  const child = new Deno.Command("bash", {
    args: [`${f.kernel}/scripts/update.sh`, "0.8.1"],
    env: f.env,
    stdout: "piped",
    stderr: "piped",
    signal: AbortSignal.timeout(20_000),
  }).spawn();
  let output = "";
  const capture = () =>
    new WritableStream<Uint8Array>({
      write(chunk) {
        output += new TextDecoder().decode(chunk);
      },
    });
  const streams = [
    child.stdout.pipeTo(capture()),
    child.stderr.pipeTo(capture()),
  ];
  async function waitFor(message: string, timeout: number) {
    const deadline = Date.now() + timeout;
    while (!output.includes(message) && Date.now() < deadline) {
      await new Promise((resolve) => setTimeout(resolve, 20));
    }
    assert(output.includes(message), `Missing ${message}: ${output}`);
  }
  try {
    await waitFor("installer stdout is live", 3000);
    await waitFor("preparing kernel 0.8.1; still running (", 12_000);
  } finally {
    await Deno.remove(gate);
    const status = await child.status;
    await Promise.all(streams);
    await f.cleanup();
    assert(status.success, output);
  }
});

Deno.test("numeric version ordering and migration phases", async () => {
  assert(compareVersions("0.10.0", "0.9.99") > 0);
  assert(compareVersions("0.8.10", "0.8.9") > 0);
  for (const invalid of ["0.8", "0.08.1", "0.8.1-rc", "1;echo bad"]) {
    let rejected = false;
    try {
      compareVersions(invalid, "0.8.0");
    } catch {
      rejected = true;
    }
    assert(rejected, `accepted ${invalid}`);
  }
  const f = await fixture();
  try {
    const directory = await f.version("0.10.0", {
      "0.8.0/010-old.sh": "exit 1",
      "0.9.0/020-second.sh": "exit 0",
      "0.9.0/010-first.sh": "exit 0",
      "0.9.0/after-start/010-online.sh": "exit 0",
      "0.10.0/010-third.sh": "exit 0",
      "0.11.0/010-future.sh": "exit 1",
    });
    const paths = await migrationScripts(directory, "0.8.0", "0.10.0", false);
    assert(
      paths.map((path) => path.slice(directory.length)).join(",") ===
        "/scripts/update/0.9.0/010-first.sh,/scripts/update/0.9.0/020-second.sh,/scripts/update/0.10.0/010-third.sh",
    );
    assert(
      (await migrationScripts(directory, "0.8.0", "0.10.0", true)).length === 1,
    );
  } finally {
    await f.cleanup();
  }
});

Deno.test("update lock rejects simultaneous requests without touching the kernel", async () => {
  const f = await fixture();
  try {
    await Deno.mkdir(`${f.kernel}/update`);
    using lock = await Deno.open(`${f.kernel}/update/lock`, {
      create: true,
      write: true,
    });
    lock.lockSync(true);
    const result = await f.update("0.8.1");
    assert(
      result.code === 75 && result.output.includes("Another updater"),
      result.output,
    );
    assert(await Deno.readLink(`${f.kernel}/current`) === "releases/0.8.0");
  } finally {
    await f.cleanup();
  }
});

Deno.test("bootstrapped 0.7.4 upgrades across the minor version", async () => {
  const f = await fixture("0.7.4");
  try {
    await f.version("0.8.2");
    for (const action of ["0.8.2", "--apply", "--finish"]) {
      const result = await f.update(action);
      assert(result.code === 0, result.output);
    }
    assert(await Deno.readLink(`${f.kernel}/current`) === "releases/0.8.2");
    assert((await f.state()).phase === "complete");
  } finally {
    await f.cleanup();
  }
});

Deno.test("skip releases, preserve instance state, switch one pointer, and finish", async () => {
  const f = await fixture();
  try {
    await f.version("0.10.0", {
      "0.8.1/020-b.sh": "echo b >> trace",
      "0.8.1/010-a.sh": "echo a >> trace",
      "0.9.0/010-c.sh": "echo c >> trace",
      "0.10.0/010-d.sh": "echo d >> trace",
      "0.9.0/after-start/010-package.sh":
        'admin --root "$THE8020_INSTANCE_ROOT" kernel.status >/dev/null; echo online >> trace',
    });
    let result = await f.update("0.10.0");
    assert(result.code === 0, result.output);
    assert(await Deno.readLink(`${f.kernel}/current`) === "releases/0.8.0");
    assert(await read(`${f.root}/stops`) === "stop\n");
    using running = await Deno.open(`${f.kernel}/run/kernel.lock`, {
      write: true,
      create: true,
    });
    running.lockSync(true);
    result = await f.update("--apply");
    assert(
      result.code !== 0 && result.output.includes("still running"),
      result.output,
    );
    running.unlockSync();
    result = await f.update("--apply");
    assert(result.code === 0, result.output);
    assert(await Deno.readLink(`${f.kernel}/current`) === "releases/0.10.0");
    assert(await read(`${f.root}/trace`) === "a\nb\nc\nd\n");
    result = await f.update("--finish");
    assert(result.code === 0, result.output);
    assert(await read(`${f.root}/trace`) === "a\nb\nc\nd\nonline\n");
    assert((await f.state()).phase === "complete");
    for (
      const file of [
        "kernel.toml",
        "packages/keep",
        "users/keep",
        "database/keep",
      ]
    ) assert(await read(`${f.root}/${file}`) === "keep");
    result = await f.update("0.10.0");
    assert(
      result.code === 0 && result.output.includes("already installed"),
      result.output,
    );
    for (const target of ["0.9.0", "1.0.0", "0.10", "00.10.0"]) {
      assert((await f.update(target)).code !== 0);
    }
  } finally {
    await f.cleanup();
  }
});

Deno.test("runtime ready cannot complete an update when ordinary Workers fail", async () => {
  const f = await fixture();
  try {
    await f.version("0.8.1");
    assert((await f.update("0.8.1")).code === 0);
    assert((await f.update("--apply")).code === 0);
    await Deno.writeTextFile(`${f.temp}/fail-worker`, "");
    const result = await f.update("--finish");
    assert(
      result.code !== 0 && result.output.includes("Worker verification failed"),
      result.output,
    );
    assert((await f.state()).phase === "installed");
    await Deno.remove(`${f.temp}/fail-worker`);
    assert((await f.update("--finish")).code === 0);
    assert((await f.state()).phase === "complete");
  } finally {
    await f.cleanup();
  }
});

Deno.test("failed migration resumes without repeating completed scripts", async () => {
  const f = await fixture();
  try {
    await f.version("0.8.2", {
      "0.8.1/010-once.sh": "echo once >> trace",
      "0.8.2/010-repair.sh": "[[ -f allow ]] || exit 2; echo repaired >> trace",
    });
    assert((await f.update("0.8.2")).code === 0);
    const failed = await f.update("--apply");
    assert(
      failed.code !== 0 && failed.output.includes("Migration"),
      failed.output,
    );
    assert((await f.state()).phase === "offline");
    assert(await Deno.readLink(`${f.kernel}/current`) === "releases/0.8.0");
    assert((await f.update("0.8.3")).code !== 0);
    await Deno.writeTextFile(`${f.root}/allow`, "");
    assert((await f.update("0.8.2")).code === 0);
    // Recover a crash between creating and renaming the active pointer.
    await Deno.symlink("releases/0.8.2", `${f.kernel}/current.next`);
    const resumed = await f.update("--apply");
    assert(resumed.code === 0, resumed.output);
    assert((await f.update("--finish")).code === 0);
    assert(await read(`${f.root}/trace`) === "once\nrepaired\n");
  } finally {
    await f.cleanup();
  }
});

Deno.test("fetch, preparation, and validation errors preserve the active kernel", async () => {
  for (const failure of ["git", "build", "binary"]) {
    const f = await fixture();
    try {
      const directory = await f.version("0.8.3");
      if (failure === "binary") {
        await Deno.writeTextFile(
          `${directory}/bin/kernel`,
          "#!/bin/bash\necho invalid\n",
        );
      } else await Deno.writeTextFile(`${f.temp}/fail-${failure}`, "");
      const result = await f.update("0.8.3");
      assert(result.code !== 0, result.output);
      let staging = false;
      try {
        await Deno.stat(`${f.kernel}/update/download`);
        staging = true;
      } catch { /* Removed. */ }
      assert(!staging, "failed preparation retained temporary data");
      assert(await Deno.readLink(`${f.kernel}/current`) === "releases/0.8.0");
      let stopped = false;
      try {
        await read(`${f.root}/stops`);
        stopped = true;
      } catch { /* Not stopped. */ }
      assert(!stopped);
      for (
        const file of [
          "kernel.toml",
          "packages/keep",
          "users/keep",
          "database/keep",
        ]
      ) {
        assert(await read(`${f.root}/${file}`) === "keep");
      }
    } finally {
      await f.cleanup();
    }
  }
});
