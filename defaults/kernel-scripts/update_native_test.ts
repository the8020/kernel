// Opt-in single-node check: use a fresh numbered /root/8020/test<N> directory.
const testRoot = Deno.env.get("THE8020_NATIVE_UPDATE_ROOT");
const source = new URL("../../", import.meta.url).pathname;
const baselineCommit = "0".repeat(40);

Deno.test({
  name:
    "0.7.4 prepares tagged source with the ordinary installer and updates in place",
  ignore: !testRoot,
  async fn() {
    const root = `${testRoot}/instance`, kernel = `${root}/node/kernel`;
    const baseline = `${kernel}/releases/0.7.4`, tools = `${testRoot}/tools`;
    const released = `${testRoot}/released-source`,
      oldSource = `${testRoot}/old-source`;
    const go = Deno.env.get("THE8020_BUILD_GO") ??
      `${source}/.development/toolchains/go/bin/go`;
    const env: Record<string, string> = {
      ...Deno.env.toObject(),
      GOMAXPROCS: "2",
      GOMEMLIMIT: "512MiB",
      GOFLAGS: "-p=1",
      GOWORK: "off",
      GOCACHE: `${source}/.development/cache/go-build`,
      GOMODCACHE: `${source}/.development/cache/go-mod`,
      PATH: `${tools}:${go.slice(0, go.lastIndexOf("/"))}:${
        Deno.env.get("PATH")
      }`,
      THE8020_INSTANCE_ROOT: root,
      THE8020_SANDBOX_RUNTIME_MODE: "rootless",
      FIXTURE_SOURCE: released,
      FIXTURE_CACHE: `${source}/.development/cache`,
    };
    async function run(program: string, args: string[], cwd = source) {
      const result = await new Deno.Command(program, {
        args,
        cwd,
        env,
        stdout: "piped",
        stderr: "piped",
      }).output();
      if (!result.success) {
        throw new Error(
          `${program}: ${new TextDecoder().decode(result.stderr).slice(-6000)}`,
        );
      }
      return new TextDecoder().decode(result.stdout).trim();
    }
    const assert = (value: unknown, message: string) => {
      if (!value) throw new Error(message);
    };
    await Deno.mkdir(testRoot!);
    for (
      const directory of [
        tools,
        released,
        oldSource,
        `${baseline}/bin`,
        `${baseline}/scripts`,
        `${baseline}/runtime/images/rootless`,
        `${baseline}/runtime/images/development`,
        `${baseline}/runtime/definitions`,
        `${baseline}/development-scripts`,
        `${kernel}/runtime`,
      ]
    ) {
      await Deno.mkdir(directory, { recursive: true });
    }
    await run("bash", [
      "-o",
      "pipefail",
      "-c",
      'git -C "$1" archive 0.7.4 | tar -xf - -C "$2"',
      "copy-old",
      source,
      oldSource,
    ]);
    await run(go, ["run", "./kernel/cbus/gen"], oldSource);
    for (const binary of ["kernel", "admin"]) {
      await run(go, [
        "build",
        "-mod=mod",
        "-p",
        "1",
        "-ldflags",
        `-s -X main.buildID=0.7.4+${baselineCommit}`,
        "-o",
        `${baseline}/bin/${binary}`,
        `./cmd/${binary}`,
      ], `${oldSource}/.development/generated`);
    }
    await Deno.copyFile(
      `${source}/.development/bin/logd`,
      `${baseline}/bin/logd`,
    );
    await run("cp", [
      "-a",
      `${source}/.development/runtime-bin/.`,
      `${baseline}/bin/`,
    ]);
    await run("bash", [
      "-o",
      "pipefail",
      "-c",
      'tar --exclude=node_modules -cf - . | tar -xf - -C "$1"',
      "copy-runtime",
      `${baseline}/runtime/definitions`,
    ], `${oldSource}/defaults/config/runtime`);
    for (
      const [prepared, name] of [["service-image", "rootless"], [
        "development",
        "development",
      ]]
    ) {
      await run("cp", [
        "-a",
        "--reflink=auto",
        `${source}/.development/runtime/${prepared}/.`,
        `${baseline}/runtime/images/${name}/`,
      ]);
    }
    await run("bash", [
      `${oldSource}/defaults/config/runtime/stage-service-runtime.sh`,
      oldSource,
      `${baseline}/runtime/images/rootless/rootfs/opt/runtime`,
    ]);
    try {
      await Deno.remove(`${baseline}/runtime/images/rootless/smoke.json`);
    } catch (error) {
      if (!(error instanceof Deno.errors.NotFound)) throw error;
    }
    for (const name of ["update.sh", "update.ts", "launch.sh"]) {
      await Deno.copyFile(
        `${source}/defaults/kernel-scripts/${name}`,
        `${baseline}/scripts/${name}`,
      );
    }
    await Deno.writeTextFile(
      `${baseline}/release.json`,
      JSON.stringify({
        version: "0.7.4",
        commit: baselineCommit,
        architecture: Deno.build.arch === "x86_64" ? "amd64" : "arm64",
      }),
    );
    await Deno.symlink("releases/0.7.4", `${kernel}/current`);
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
    await run(`${kernel}/bin/kernel`, [
      "--root",
      root,
      "--init-defaults",
      "--init-only",
    ]);
    await Deno.writeTextFile(`${root}/users/keep`, "private work");
    await Deno.mkdir(`${root}/packages/keep`);
    await Deno.writeTextFile(`${root}/packages/keep/value`, "unchanged");
    const config = await Deno.readTextFile(`${root}/kernel.toml`);
    // Copy ordinary source into a local tagged repository; only its transport is replaced.
    await run("bash", [
      "-o",
      "pipefail",
      "-c",
      'tar --exclude=.git --exclude=.development --exclude=node_modules --exclude=.github -cf - . | tar -xf - -C "$1"',
      "copy-source",
      released,
    ]);
    await run("/usr/bin/git", ["init", "--quiet"], released);
    await run("/usr/bin/git", ["add", "."], released);
    await run("/usr/bin/git", [
      "-c",
      "user.name=Fixture",
      "-c",
      "user.email=fixture@invalid",
      "-c",
      "commit.gpgsign=false",
      "commit",
      "--quiet",
      "-m",
      "source",
    ], released);
    await run("/usr/bin/git", ["tag", "0.8.3"], released);
    const targetCommit = await run(
      "/usr/bin/git",
      ["rev-parse", "HEAD"],
      released,
    );
    await Deno.writeTextFile(
      `${tools}/git`,
      `#!/bin/bash
set -eu
if [[ "$1" == clone ]]; then
  destination="${"${!#}"}"
  /usr/bin/git clone --quiet --branch 0.8.3 "$FIXTURE_SOURCE" "$destination"
  mkdir -p "$destination/.development"
  ln -s "$FIXTURE_CACHE" "$destination/.development/cache"
else exec /usr/bin/git "$@"; fi
`,
      { mode: 0o755 },
    );
    const port = () => {
      const socket = Deno.listen({ hostname: "127.0.0.1", port: 0 });
      const port = (socket.addr as Deno.NetAddr).port;
      socket.close();
      return String(port);
    };
    env.THE8020_NETWORK_MAIN_PORT = port();
    env.THE8020_NETWORK_SSH_PORT = port();
    using log = await Deno.open(`${testRoot}/launcher.log`, {
      create: true,
      write: true,
    });
    const launcher = new Deno.Command("bash", {
      args: [`${kernel}/scripts/launch.sh`, root],
      env,
      stdin: "null",
      stdout: "piped",
      stderr: "piped",
    }).spawn();
    const out = launcher.stdout.pipeTo(log.writable, { preventClose: true });
    const errors: Uint8Array[] = [];
    const err = launcher.stderr.pipeTo(
      new WritableStream({
        write(chunk) {
          errors.push(chunk);
        },
      }),
    );
    async function observe(version: string, commit: string) {
      const deadline = Date.now() + 60000;
      while (Date.now() < deadline) {
        try {
          const response = JSON.parse(
            await run(`${kernel}/bin/admin`, [
              "--root",
              root,
              "--json",
              "kernel.status",
            ]),
          );
          if (
            response.result.build_id === `${version}+${commit}` &&
            response.result.runtime_ready === true
          ) return response.result;
        } catch { /* Control plane changes during restart. */ }
        await new Promise((resolve) => setTimeout(resolve, 100));
      }
      throw new Error(`Kernel ${version} did not become ready`);
    }
    try {
      const initial = await observe("0.7.4", baselineCommit);
      const result = await run("bash", [
        `${kernel}/scripts/update.sh`,
        "0.8.3",
      ]);
      console.log(result);
      const selected = await observe("0.8.3", targetCommit);
      assert(initial.pid !== selected.pid, "kernel was not replaced");
      assert(
        Number(await Deno.readTextFile(`${kernel}/run/launcher.pid`)) ===
          launcher.pid,
        "launcher changed",
      );
      for (let i = 0; i < 100; i++) {
        const state = JSON.parse(
          await Deno.readTextFile(`${kernel}/update/state.json`),
        );
        if (state.phase === "complete") break;
        await new Promise((resolve) => setTimeout(resolve, 100));
      }
      assert(
        JSON.parse(await Deno.readTextFile(`${kernel}/update/state.json`))
          .phase === "complete",
        "update incomplete",
      );
      assert(
        await Deno.readTextFile(`${root}/users/keep`) === "private work",
        "private data changed",
      );
      assert(
        await Deno.readTextFile(`${root}/packages/keep/value`) === "unchanged",
        "packages changed",
      );
      assert(
        await Deno.readTextFile(`${root}/kernel.toml`) === config,
        "configuration changed",
      );
      console.log(
        "native source update passed: real 0.7.4, ordinary installer, retained launcher, packages, configuration, and private work",
      );
    } finally {
      try {
        launcher.kill("SIGTERM");
      } catch { /* Already stopped. */ }
      await launcher.status;
      await Promise.all([out, err]);
      for (const chunk of errors) await Deno.stderr.write(chunk);
    }
  },
});
