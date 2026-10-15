// Opt-in single-node check: use a fresh numbered /root/8020/test<N> directory.
const testRoot = Deno.env.get("THE8020_NATIVE_UPDATE_ROOT");
const source = new URL("../../", import.meta.url).pathname;
const baselineCommit = "0".repeat(40);

Deno.test({
  name:
    "inside-container bootstrap adopts real 0.7.4 without losing its parent process",
  ignore: !testRoot,
  async fn() {
    const root = `${testRoot}/instance`, kernel = `${root}/node/kernel`;
    const factory = `${testRoot}/factory`, tools = `${testRoot}/tools`;
    const baseline = `${factory}/share/the8020/runtime-state`;
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
      PATH: `${tools}:${factory}/bin:${Deno.env.get("PATH")}`,
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
        `${factory}/bin`,
        `${factory}/share/the8020/runtime-bin`,
        `${baseline}/images/rootless`,
        `${baseline}/images/development`,
        `${baseline}/definitions`,
        `${root}/scripts`,
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
        `${factory}/bin/${binary}`,
        `./cmd/${binary}`,
      ], `${oldSource}/.development/generated`);
    }
    await Deno.copyFile(
      `${source}/.development/bin/logd`,
      `${factory}/bin/logd`,
    );
    await run("cp", [
      "-a",
      `${source}/.development/runtime-bin/.`,
      `${factory}/share/the8020/runtime-bin/`,
    ]);
    await run("bash", [
      "-o",
      "pipefail",
      "-c",
      'tar --exclude=node_modules -cf - . | tar -xf - -C "$1"',
      "copy-runtime",
      `${baseline}/definitions`,
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
        `${baseline}/images/${name}/`,
      ]);
    }
    await run("bash", [
      `${oldSource}/defaults/config/runtime/stage-service-runtime.sh`,
      oldSource,
      `${baseline}/images/rootless/rootfs/opt/runtime`,
    ]);
    try {
      await Deno.remove(`${baseline}/images/rootless/smoke.json`);
    } catch (error) {
      if (!(error instanceof Deno.errors.NotFound)) throw error;
    }
    await Deno.writeTextFile(
      `${factory}/share/the8020/release`,
      `release_line=0.7\nkernel_tag=0.7.4\nkernel_commit=${baselineCommit}\n`,
    );
    await Deno.symlink(`${factory}/share/the8020/runtime-bin`, `${kernel}/bin`);
    for (const name of ["definitions", "images"]) {
      await Deno.symlink(`${baseline}/${name}`, `${kernel}/runtime/${name}`);
    }
    await run("cp", [
      "-a",
      `${oldSource}/defaults/scripts/.`,
      `${root}/scripts/`,
    ]);
    await run(`${factory}/bin/kernel`, [
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
    await run("/usr/bin/git", ["tag", "0.8.6"], released);
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
  /usr/bin/git clone --quiet --branch 0.8.6 "$FIXTURE_SOURCE" "$destination"
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
    await run("bash", [
      `${baseline}/definitions/smoke-portable.sh`,
      `${kernel}/bin/runsc`,
      `${baseline}/images/rootless/rootfs`,
      `${baseline}/images/rootless/image.json`,
      `${baseline}/images/rootless/smoke.json`,
      `${kernel}/runtime/tmp`,
    ]);
    using log = await Deno.open(`${testRoot}/container.log`, {
      create: true,
      write: true,
    });
    const parent = new Deno.Command("bash", {
      args: [
        "-c",
        `
        "$1/bin/kernel" --root "$2" &
        child=$!
        trap 'kill -TERM "$child" 2>/dev/null || true; wait "$child" 2>/dev/null || true; exit' TERM INT
        wait "$child"
      `,
        "container-parent",
        factory,
        root,
      ],
      env,
      stdin: "null",
      stdout: "piped",
      stderr: "piped",
    }).spawn();
    const out = parent.stdout.pipeTo(log.writable, { preventClose: true });
    const errors: Uint8Array[] = [];
    const err = parent.stderr.pipeTo(
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
            await run(`${factory}/bin/admin`, [
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
    let driver: number | undefined;
    try {
      const initial = await observe("0.7.4", baselineCommit);
      const result = await run("bash", [`${source}/docker/update.sh`, "0.8.6"]);
      console.log(result);
      driver = Number(result.match(/PID (\d+)/)?.[1]);
      const deadline = Date.now() + 600000;
      while (Date.now() < deadline) {
        Deno.kill(parent.pid, 0);
        try {
          const state = JSON.parse(
            await Deno.readTextFile(`${kernel}/update/state.json`),
          );
          if (state.phase === "complete") break;
          if (state.error) throw new Error(state.error);
        } catch (error) {
          if (!(error instanceof Deno.errors.NotFound)) throw error;
        }
        await new Promise((resolve) => setTimeout(resolve, 500));
      }
      assert(
        JSON.parse(await Deno.readTextFile(`${kernel}/update/state.json`))
          .phase === "complete",
        "update incomplete",
      );
      const selected = await observe("0.8.6", targetCommit);
      assert(initial.pid !== selected.pid, "kernel was not replaced");
      const execution = JSON.parse(
        await run(`${kernel}/bin/admin`, [
          "--root",
          root,
          "--json",
          "runtime",
          "eval",
          "export default 42",
          "--timeout-ms",
          "15000",
        ]),
      );
      assert(
        execution.success === true && execution.result.result === 42,
        `Worker execution through the installed image failed: ${
          JSON.stringify(execution)
        }`,
      );

      assert(
        Number(await Deno.readTextFile(`${kernel}/run/launcher.pid`)) ===
          initial.pid,
        "original kernel PID was not retained by launcher",
      );
      Deno.kill(parent.pid, 0);
      assert(
        (await Deno.readFile(`${factory}/bin/kernel`)).slice(0, 4).join() ===
          "127,69,76,70",
        "original native executable was not restored",
      );
      await run("cmp", [
        `${factory}/bin/kernel`,
        `${kernel}/releases/0.7.4/bin/kernel`,
      ]);
      const updateLog = await Deno.readTextFile(`${kernel}/update.log`);
      for (
        const message of [
          "bootstrap: waiting for the launcher and kernel control plane",
          "bootstrap: original kernel has stopped; adopting the launcher",
          "startup: installing the initial platform runtime",
          "bootstrap: kernel control plane is ready; starting update",
          "update: completed 0.8.6",
        ]
      ) {
        assert(
          updateLog.includes(message),
          `Missing update log phase: ${message}`,
        );
      }
      assert(
        (await Deno.stat(`${kernel}/update.log`)).mode! % 512 === 384,
        "update log is not private",
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
        "inside-container source update passed: real 0.7.4, retained parent and original child PID, ordinary installer, real Worker, packages, configuration, and private work",
      );
    } catch (error) {
      try {
        console.error(
          (await Deno.readTextFile(`${kernel}/update.log`)).slice(-12000),
        );
      } catch { /* No update log yet. */ }
      throw error;
    } finally {
      if (driver) {
        try {
          Deno.kill(driver, "SIGTERM");
        } catch { /* Already stopped. */ }
      }
      try {
        parent.kill("SIGTERM");
      } catch { /* Already stopped. */ }
      await parent.status;
      await Promise.all([out, err]);
      for (const chunk of errors) await Deno.stderr.write(chunk);
    }
  },
});
