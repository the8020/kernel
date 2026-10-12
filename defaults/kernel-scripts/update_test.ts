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

async function fixture() {
  const temp = await Deno.makeTempDir({ prefix: "kernel-update-" });
  const root = `${temp}/instance`, kernel = `${root}/node/kernel`;
  const archives = `${temp}/archives`, tools = `${temp}/tools`;
  for (
    const directory of [
      archives,
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
    `${tools}/curl`,
    `#!/bin/bash
set -eu
while (( $# )); do
  if [[ "$1" == --output ]]; then destination=$2; shift 2; else url=$1; shift; fi
done
cp "$FIXTURE_ARCHIVES/\${url##*/}" "$destination"
`,
    { mode: 0o755 },
  );
  const env = {
    FIXTURE_ARCHIVES: archives,
    THE8020_INSTANCE_ROOT: root,
    PATH: `${tools}:${Deno.env.get("PATH")}`,
  };
  async function release(
    version: string,
    scripts: Record<string, string> = {},
  ) {
    const directory = `${temp}/release-${version}`;
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
      `#!/bin/bash\necho '${version}+${commit}'\n`,
      { mode: 0o755 },
    );
    await Deno.writeTextFile(
      `${directory}/bin/admin`,
      `#!/bin/bash
if [[ "$*" == *kernel.shutdown* ]]; then echo stop >> "$THE8020_INSTANCE_ROOT/stops"; echo '{"success":true}';
elif [[ "$*" == *kernel.config.get* ]]; then echo '{"success":true,"result":{"setting":{"configured_value":"rootless"}}}';
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
  async function pack(version: string, scripts: Record<string, string> = {}) {
    const directory = await release(version, scripts),
      filename = `the8020-kernel-${version}-linux-${arch}.tar.gz`;
    assert(
      (await run("tar", [
        "-czf",
        `${archives}/${filename}`,
        "-C",
        directory,
        "release.json",
        "bin",
        "scripts",
        "runtime",
        "development-scripts",
      ])).code === 0,
    );
    await checksum(version);
    return directory;
  }
  async function checksum(version: string) {
    const filename = `the8020-kernel-${version}-linux-${arch}.tar.gz`;
    const digest =
      (await run("sha256sum", [`${archives}/${filename}`])).output.split(
        " ",
      )[0];
    await Deno.writeTextFile(
      `${archives}/${filename}.sha256`,
      `${digest}  ${filename}\n`,
    );
  }
  const initial = await release("0.8.0");
  await Deno.rename(initial, `${kernel}/releases/0.8.0`);
  await Deno.symlink("releases/0.8.0", `${kernel}/current`);
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
    archives,
    pack,
    checksum,
    update: (action: string) =>
      run("bash", [`${kernel}/scripts/update.sh`, action], env),
    state: async () => JSON.parse(await read(`${kernel}/update/state.json`)),
    cleanup: () => Deno.remove(temp, { recursive: true }),
  };
}

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
    const directory = await f.pack("0.10.0", {
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

Deno.test("release builder preserves links and excludes instance data", async () => {
  const f = await fixture();
  try {
    const repository = `${f.temp}/repository`, output = `${f.temp}/published`;
    await Deno.mkdir(repository);
    await Deno.copyFile(
      new URL("../../release-build.sh", import.meta.url),
      `${repository}/release-build.sh`,
    );
    assert((await run("git", ["init", "--quiet", repository])).code === 0);
    assert(
      (await run("git", ["-C", repository, "add", "release-build.sh"])).code ===
        0,
    );
    assert(
      (await run("git", [
        "-C",
        repository,
        "-c",
        "user.name=Fixture",
        "-c",
        "user.email=fixture@invalid",
        "-c",
        "commit.gpgsign=false",
        "commit",
        "--quiet",
        "-m",
        "fixture",
      ])).code === 0,
    );
    assert((await run("git", ["-C", repository, "tag", "0.8.0"])).code === 0);
    const sha = (await run("git", ["-C", repository, "rev-parse", "HEAD"]))
      .output.trim();
    await Deno.writeTextFile(
      `${f.kernel}/bin/kernel`,
      `#!/bin/bash\necho '0.8.0+${sha}'\n`,
    );
    await Deno.writeTextFile(`${f.temp}/private`, "do not copy this data");
    await Deno.symlink(`${f.temp}/private`, `${f.root}/scripts/link`);
    const packed = await run("bash", [
      `${repository}/release-build.sh`,
      f.root,
      output,
    ]);
    assert(packed.code === 0, packed.output);
    const archive = `${output}/the8020-kernel-0.8.0-linux-${arch}.tar.gz`;
    const listing = (await run("tar", ["-tvzf", archive])).output;
    assert(
      listing.includes("development-scripts/link ->"),
      "publisher dereferenced a link",
    );
    assert(
      !listing.includes("packages/keep") && !listing.includes("users/keep") &&
        !listing.includes("database/keep"),
    );
    await Deno.writeTextFile(`${repository}/untracked-source`, "changed");
    assert(
      (await run("bash", [`${repository}/release-build.sh`, f.root, output]))
        .code !== 0,
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

Deno.test("skip releases, preserve instance state, switch one pointer, and finish", async () => {
  const f = await fixture();
  try {
    await f.pack("0.10.0", {
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

Deno.test("failed migration resumes without repeating completed scripts", async () => {
  const f = await fixture();
  try {
    await f.pack("0.8.2", {
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

Deno.test("checksum and archive errors cannot shut down the active kernel", async () => {
  const f = await fixture();
  try {
    const directory = await f.pack("0.8.1");
    const filename = `the8020-kernel-0.8.1-linux-${arch}.tar.gz`;
    await Deno.writeTextFile(
      `${f.archives}/${filename}.sha256`,
      `${"f".repeat(64)}  ${filename}\n`,
    );
    assert((await f.update("0.8.1")).code !== 0);
    await Deno.mkdir(`${directory}/packages`);
    await Deno.writeTextFile(
      `${directory}/packages/forbidden`,
      "must not install",
    );
    assert(
      (await run("tar", [
        "-czf",
        `${f.archives}/${filename}`,
        "-C",
        directory,
        "release.json",
        "bin",
        "scripts",
        "runtime",
        "development-scripts",
        "packages",
      ])).code === 0,
    );
    await f.checksum("0.8.1");
    const result = await f.update("0.8.1");
    assert(
      result.code !== 0 && result.output.includes("Unsafe release archive"),
      result.output,
    );
    // GNU tar must refuse an archive that writes through an outside symlink.
    const outside = `${f.temp}/outside`;
    await Deno.mkdir(outside);
    await Deno.writeTextFile(`${outside}/keep`, "unchanged");
    const crafted = await run("python3", [
      "-c",
      `import io,sys,tarfile
with tarfile.open(sys.argv[2], "w:gz") as archive:
  for name in ["release.json", "bin", "scripts", "runtime", "development-scripts"]:
    archive.add(sys.argv[1] + "/" + name, arcname=name)
  link=tarfile.TarInfo("bin/outside")
  link.type=tarfile.SYMTYPE
  link.linkname=sys.argv[3]
  archive.addfile(link)
  file=tarfile.TarInfo("bin/outside/keep")
  file.size=len(b"overwritten")
  archive.addfile(file, io.BytesIO(b"overwritten"))
`,
      directory,
      `${f.archives}/${filename}`,
      outside,
    ]);
    assert(crafted.code === 0, crafted.output);
    await f.checksum("0.8.1");
    assert((await f.update("0.8.1")).code !== 0);
    assert(await read(`${outside}/keep`) === "unchanged");
    assert(await Deno.readLink(`${f.kernel}/current`) === "releases/0.8.0");
    let stopped = false;
    try {
      await read(`${f.root}/stops`);
      stopped = true;
    } catch { /* not stopped */ }
    assert(!stopped);
  } finally {
    await f.cleanup();
  }
});
