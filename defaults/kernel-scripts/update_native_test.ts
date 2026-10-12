// Opt-in single-node test: use a fresh numbered /root/8020/test<N> directory.
const testRoot = Deno.env.get("THE8020_NATIVE_UPDATE_ROOT");
const source = new URL("../../", import.meta.url).pathname;
const commit = "0".repeat(40);

Deno.test({
  name:
    "native cbus update keeps its launcher alive and boots the selected kernel",
  ignore: !testRoot,
  async fn() {
    const root = `${testRoot}/instance`, kernel = `${root}/node/kernel`;
    const tools = `${testRoot}/tools`, archives = `${testRoot}/archives`;
    const env: Record<string, string> = {
      ...Deno.env.toObject(),
      GOMAXPROCS: "2",
      GOMEMLIMIT: "256MiB",
      GOWORK: "off",
      GOCACHE: `${source}/.development/cache/go-build`,
      GOMODCACHE: `${source}/.development/cache/go-mod`,
      PATH: `${tools}:${Deno.env.get("PATH")}`,
      THE8020_INSTANCE_ROOT: root,
      THE8020_SANDBOX_RUNTIME_MODE: "rootless",
      FIXTURE_ARCHIVES: archives,
    };
    async function run(program: string, args: string[], cwd = source) {
      const result = await new Deno.Command(program, {
        args,
        env,
        cwd,
        stdout: "piped",
        stderr: "piped",
      }).output();
      if (!result.success) {
        throw new Error(
          `${program}: ${new TextDecoder().decode(result.stderr).slice(-4000)}`,
        );
      }
      return new TextDecoder().decode(result.stdout).trim();
    }
    const assert = (value: unknown, message: string) => {
      if (!value) throw new Error(message);
    };
    await Deno.mkdir(testRoot!); // Refuse a reused instance.
    for (
      const directory of [
        tools,
        archives,
        `${kernel}/releases`,
        `${kernel}/runtime`,
        `${root}/packages`,
        `${root}/users`,
        `${root}/database`,
      ]
    ) await Deno.mkdir(directory, { recursive: true });
    const go = `${source}/.development/toolchains/go/bin/go`;
    await run(go, ["run", "./kernel/cbus/gen"]);
    for (const version of ["0.8.0", "0.8.2"]) {
      const release = `${kernel}/releases/${version}`;
      for (
        const directory of [
          "bin",
          "scripts/update",
          "runtime/definitions",
          "runtime/images/rootless",
          "runtime/images/development",
          "development-scripts",
        ]
      ) await Deno.mkdir(`${release}/${directory}`, { recursive: true });
      await run(go, [
        "build",
        "-p",
        "1",
        "-ldflags",
        `-s -X main.buildID=${version}+${commit}`,
        "-o",
        `${release}/bin/kernel`,
        "./cmd/kernel",
      ], `${source}/.development/generated`);
      if (version === "0.8.0") {
        await run(go, [
          "build",
          "-p",
          "1",
          "-o",
          `${release}/bin/admin`,
          "./cmd/admin",
        ], `${source}/.development/generated`);
      } else {await Deno.copyFile(
          `${kernel}/releases/0.8.0/bin/admin`,
          `${release}/bin/admin`,
        );}
      await Deno.copyFile(
        `${source}/.development/bin/logd`,
        `${release}/bin/logd`,
      );
      await run("cp", [
        "-a",
        `${source}/.development/runtime-bin/.`,
        `${release}/bin/`,
      ]);
      for (const name of ["update.sh", "update.ts", "launch.sh"]) {
        await Deno.copyFile(
          `${source}/defaults/kernel-scripts/${name}`,
          `${release}/scripts/${name}`,
        );
      }
      await run("bash", [
        "-c",
        'tar --exclude=node_modules -cf - . | tar -xf - -C "$1"',
        "copy-definitions",
        `${release}/runtime/definitions`,
      ], `${source}/defaults/config/runtime`);
      // Copy prepared fixtures across filesystems without following their links.
      await run("cp", [
        "-a",
        "--reflink=auto",
        `${source}/.development/runtime/service-image/.`,
        `${release}/runtime/images/rootless/`,
      ]);
      await run("cp", [
        "-a",
        "--reflink=auto",
        `${source}/.development/runtime/development/.`,
        `${release}/runtime/images/development/`,
      ]);
      try {
        await Deno.remove(`${release}/runtime/images/rootless/smoke.json`);
      } catch (error) {
        if (!(error instanceof Deno.errors.NotFound)) throw error;
      }
      await Deno.writeTextFile(
        `${release}/release.json`,
        JSON.stringify({
          version,
          commit,
          architecture: Deno.build.arch === "x86_64" ? "amd64" : "arm64",
        }),
      );
    }
    const target = `${kernel}/releases/0.8.2`;
    for (
      const [name, text] of Object.entries({
        "0.8.1/010-first.sh": "echo first >> migrations",
        "0.8.2/010-second.sh": "echo second >> migrations",
      })
    ) {
      const path = `${target}/scripts/update/${name}`;
      await Deno.mkdir(path.slice(0, path.lastIndexOf("/")), {
        recursive: true,
      });
      await Deno.writeTextFile(path, text);
    }
    const filename = `the8020-kernel-0.8.2-linux-${
      Deno.build.arch === "x86_64" ? "amd64" : "arm64"
    }.tar.gz`;
    await run("tar", [
      "-czf",
      `${archives}/${filename}`,
      "-C",
      target,
      "release.json",
      "bin",
      "scripts",
      "runtime",
      "development-scripts",
    ]);
    const digest =
      (await run("sha256sum", [`${archives}/${filename}`])).split(" ")[0];
    await Deno.writeTextFile(
      `${archives}/${filename}.sha256`,
      `${digest}  ${filename}\n`,
    );
    await Deno.remove(target, { recursive: true });
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
    await run(`${kernel}/bin/kernel`, [
      "--root",
      root,
      "--init-defaults",
      "--init-only",
    ]);
    await Deno.writeTextFile(`${root}/users/keep`, "private work");
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
    async function observe(version: string) {
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
        } catch {
          /* Control plane is temporarily unavailable during restart. */
        }
        await new Promise((resolve) => setTimeout(resolve, 100));
      }
      throw new Error(
        `Kernel ${version} did not become ready: ${
          new TextDecoder().decode(
            await Deno.readFile(`${kernel}/update.log`).catch(() =>
              new Uint8Array()
            ),
          )
        }`,
      );
    }
    try {
      const initial = await observe("0.8.0");
      const result = JSON.parse(
        await run(`${kernel}/bin/admin`, [
          "--root",
          root,
          "--json",
          "kernel.update",
          "0.8.2",
        ]),
      );
      assert(
        result.success === true && result.result.requested === true,
        "cbus did not start updater",
      );
      const selected = await observe("0.8.2");
      assert(initial.pid !== selected.pid, "kernel process was not replaced");
      assert(
        Number(await Deno.readTextFile(`${kernel}/run/launcher.pid`)) ===
          launcher.pid,
        "launcher was replaced",
      );
      const deadline = Date.now() + 5000;
      while (Date.now() < deadline) {
        const state = JSON.parse(
          await Deno.readTextFile(`${kernel}/update/state.json`),
        );
        if (state.phase === "complete") break;
        await new Promise((resolve) => setTimeout(resolve, 100));
      }
      assert(
        JSON.parse(await Deno.readTextFile(`${kernel}/update/state.json`))
          .phase === "complete",
        "update was not completed",
      );
      assert(
        await Deno.readTextFile(`${root}/migrations`) === "first\nsecond\n",
        "migration order was incorrect",
      );
      assert(
        await Deno.readTextFile(`${root}/users/keep`) === "private work",
        "private work changed",
      );
      console.log(
        "native update passed: selected kernel, ordered migrations, retained launcher and private work",
      );
    } finally {
      try {
        launcher.kill("SIGTERM");
      } catch { /* Already exited. */ }
      await launcher.status;
      await Promise.all([out, err]);
      for (const chunk of errors) await Deno.stderr.write(chunk);
    }
  },
});
