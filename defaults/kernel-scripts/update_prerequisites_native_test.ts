// Opt-in real old-set qualification; use task-owned fresh numbered instances.
const root = Deno.env.get("THE8020_NATIVE_PREREQUISITES_ROOT");
const fresh = Deno.env.get("THE8020_NATIVE_FRESH_ROOT");
const source = new URL("../../", import.meta.url).pathname;
const assert = (value: unknown, message: string) => {
  if (!value) throw new Error(message);
};

Deno.test({
  name:
    "real 0.10.14 package set admits a complete approved development prerequisite set",
  ignore: !root || !fresh,
  async fn() {
    const oldSource = `${root}/source`, instance = `${root}/instance`;
    const candidate = `${fresh}/node/kernel/bin`;
    const env = {
      ...Deno.env.toObject(),
      GOMAXPROCS: "2",
      GOMEMLIMIT: "512MiB",
      GOFLAGS: "-p=1",
      GOCACHE: `${source}/.development/cache/go-build`,
      GOMODCACHE: `${source}/.development/cache/go-mod`,
      GOWORK: "off",
    };
    const run = async (
      program: string,
      args: string[],
      cwd = source,
      input?: string,
    ) => {
      const child = new Deno.Command(program, {
        args,
        cwd,
        env,
        stdin: input === undefined ? "null" : "piped",
        stdout: "piped",
        stderr: "piped",
      }).spawn();
      if (input !== undefined) {
        const writer = child.stdin.getWriter();
        await writer.write(new TextEncoder().encode(input));
        await writer.close();
      }
      const result = await child.output();
      const output = new TextDecoder().decode(result.stdout);
      if (!result.success) {
        throw new Error(
          `${program}: ${output.slice(-4000)} ${
            new TextDecoder().decode(result.stderr).slice(-4000)
          }`,
        );
      }
      return output.trim();
    };
    const admin = async (path: string, ...args: string[]) => {
      const value = JSON.parse(
        await run(`${candidate}/admin`, ["--root", path, "--json", ...args]),
      );
      assert(value.success, JSON.stringify(value));
      return value.result;
    };
    const evaluate = (path: string, code: string) =>
      admin(
        path,
        "runtime",
        "eval",
        code,
        "--read",
        "/workspace/packages",
        "--network",
        "127.0.0.1",
        "--timeout-ms",
        "90000",
      );
    const waitReady = async (path: string) => {
      let last: unknown;
      for (let attempt = 0; attempt < 180; attempt++) {
        try {
          last = await admin(path, "kernel.status");
          const state = last as Record<string, unknown>;
          if (
            state.runtime_ready && state.database_initialized &&
            !state.runtime_application_failure
          ) return;
        } catch (error) {
          last = String(error);
        }
        await new Promise((resolve) => setTimeout(resolve, 1000));
      }
      throw new Error(`Readiness: ${JSON.stringify(last)}`);
    };
    const port = () => {
      const server = Deno.listen({ hostname: "127.0.0.1", port: 0 });
      const value = (server.addr as Deno.NetAddr).port;
      server.close();
      return value;
    };
    const children: { process: Deno.ChildProcess; instance: string }[] = [];
    const start = (path: string, binary: string, http: number) => {
      const child = new Deno.Command("bash", {
        args: [
          "-c",
          'exec "$BINARY" --root "$INSTANCE" --set "network.main_port=$HTTP_PORT" --set "network.ssh_port=$SSH_PORT" --set network.https_port=0 --set sandbox.runtime.mode=rootless --set sandbox.warm_pool.size=0 --set runtime.sandbox.keep_alive=0 --set runtime.sandbox.stop_grace_period=1000 --set job.default.idle_runtime_timeout=1000 > "$INSTANCE/native-upgrade.log" 2>&1',
        ],
        env: {
          ...env,
          BINARY: binary,
          INSTANCE: path,
          HTTP_PORT: String(http),
          SSH_PORT: String(port()),
        },
        stdin: "null",
        stdout: "null",
        stderr: "null",
      }).spawn();
      children.push({ process: child, instance: path });
    };
    await Deno.mkdir(oldSource, { recursive: true });
    try {
      await run("bash", [
        "-o",
        "pipefail",
        "-c",
        'git archive 0.10.14 | tar -xf - -C "$1"',
        "archive",
        oldSource,
      ]);
      const go = `${source}/.development/toolchains/go/bin/go`;
      await run(go, ["run", "./kernel/cbus/gen"], oldSource);
      await run(go, [
        "build",
        "-mod=mod",
        "-ldflags=-s -X main.buildID=0.10.14",
        "-o",
        `${root}/old-kernel`,
        "./cmd/kernel",
      ], `${oldSource}/.development/generated`);
      await run(`${root}/old-kernel`, [
        "--root",
        instance,
        "--init-defaults",
        "--init-only",
      ]);
      await run("cp", [
        "-a",
        `${fresh}/node/kernel/bin/.`,
        `${instance}/node/kernel/bin`,
      ]);
      await Deno.copyFile(
        `${root}/old-kernel`,
        `${instance}/node/kernel/bin/kernel`,
      );
      await run("cp", [
        "-a",
        `${fresh}/node/kernel/runtime/images/.`,
        `${instance}/node/kernel/runtime/images`,
      ]);
      await run("cp", [
        "-a",
        `${oldSource}/defaults/config/runtime/.`,
        `${instance}/node/kernel/runtime/definitions`,
      ]);
      const runtime =
        `${instance}/node/kernel/runtime/images/rootless/rootfs/opt/runtime`;
      await Deno.remove(runtime, { recursive: true });
      await run("bash", [
        `${oldSource}/defaults/config/runtime/stage-service-runtime.sh`,
        oldSource,
        runtime,
      ]);
      await run("cp", [
        `${oldSource}/defaults/config/runtime/protocol/generated.ts`,
        `${runtime}/protocol.ts`,
      ]);
      await run("cp", [
        `${oldSource}/defaults/config/runtime/image/deno.json`,
        `${runtime}/deno.json`,
      ]);
      const oldImage = `${instance}/node/kernel/runtime/images/rootless`;
      await run("bash", [
        `${oldSource}/defaults/config/runtime/smoke-portable.sh`,
        `${instance}/node/kernel/bin/runsc`,
        `${oldImage}/rootfs`,
        `${oldImage}/image.json`,
        `${oldImage}/smoke.json`,
        `${instance}/node/kernel/runtime/smoke-upgrade`,
      ]);
      await run("bash", [
        `${oldSource}/install-development-assets.sh`,
        instance,
      ], oldSource);
      const oldList = await Deno.readTextFile(
        `${oldSource}/defaults/bootstrap-packages.toml`,
      );
      const targetList = await Deno.readTextFile(
        `${source}/defaults/bootstrap-packages.toml`,
      );
      const parseList = (text: string) =>
        [...text.matchAll(
          /id = "([^"]+)"\nsource = "([^"]+)"\nversion = "([^"]+)"/g,
        )].map((match) => ({
          id: match[1]!,
          source: match[2]!,
          version: match[3]!,
        }));
      const old = parseList(oldList), target = parseList(targetList);
      assert(
        old.length === 17 && target.length === 17,
        "unexpected release list",
      );
      const devPort = port(), oldPort = port();
      const origin = `http://127.0.0.1:${devPort}`;
      const peerSource = (id: string) =>
        `${origin}/the8020/deployments/peer/git/${id}.git`;
      for (const item of old) {
        const destination = `${instance}/packages/${item.id}`;
        await run("git", [
          "clone",
          "--quiet",
          "--no-hardlinks",
          "--branch",
          item.version,
          `${fresh}/packages/${item.id}`,
          destination,
        ]);
        await run(
          "git",
          ["remote", "set-url", "origin", peerSource(item.id)],
          destination,
        );
      }
      const password = "test-only-upgrade-password";
      const version = "0.10.17";
      const commit = await run("git", ["rev-parse", "HEAD"]);
      const freshKernel = `${fresh}/node/kernel`;
      const current = `${freshKernel}/current`;
      let seeded = false;
      try {
        await Deno.lstat(current);
        seeded = true;
      } catch (error) {
        if (!(error instanceof Deno.errors.NotFound)) throw error;
      }
      if (!seeded) {
        const payload = `${freshKernel}/releases/${version}`;
        await Deno.mkdir(`${payload}/runtime`, { recursive: true });
        for (const name of ["bin", "scripts"]) {
          await Deno.rename(`${freshKernel}/${name}`, `${payload}/${name}`);
          await Deno.symlink(`current/${name}`, `${freshKernel}/${name}`);
        }
        for (const name of ["definitions", "images"]) {
          await Deno.rename(
            `${freshKernel}/runtime/${name}`,
            `${payload}/runtime/${name}`,
          );
          await Deno.symlink(
            `../current/runtime/${name}`,
            `${freshKernel}/runtime/${name}`,
          );
        }
        await Deno.writeTextFile(
          `${payload}/release.json`,
          JSON.stringify({
            version,
            commit,
            architecture: Deno.build.arch === "x86_64" ? "amd64" : "arm64",
          }),
        );
        await Deno.symlink(`releases/${version}`, current);
      }
      const entryBin = `${root}/entrypoint/bin`;
      await Deno.mkdir(entryBin, { recursive: true });
      for (const name of ["kernel", "admin", "logd"]) {
        await Deno.symlink(`${candidate}/${name}`, `${entryBin}/${name}`);
      }
      for (const name of ["docker-entrypoint.sh", "the8020-check-cpu"]) {
        await Deno.copyFile(
          `${source}/docker/rootfs/usr/local/bin/${name}`,
          `${entryBin}/${name}`,
        );
      }
      const entrypoint = new Deno.Command("bash", {
        args: [
          "-c",
          'exec bash "$ENTRYPOINT" serve > "$THE8020_INSTANCE_ROOT/native-upgrade.log" 2>&1',
        ],
        env: {
          ...env,
          ENTRYPOINT: `${entryBin}/docker-entrypoint.sh`,
          THE8020_INSTANCE_ROOT: fresh!,
          THE8020_NETWORK_MAIN_PORT: String(devPort),
          THE8020_NETWORK_SSH_PORT: String(port()),
          THE8020_NETWORK_HTTPS_PORT: "0",
          THE8020_USERNAME: "upgrader",
          THE8020_PASSWORD: password,
        },
        stdin: "null",
        stdout: "null",
        stderr: "null",
      }).spawn();
      children.push({ process: entrypoint, instance: fresh! });
      await waitReady(fresh!);
      let bootstrapped = false;
      for (let attempt = 0; attempt < 300; attempt++) {
        try {
          await Deno.stat(`${fresh}/node/docker/initial-user.done`);
          bootstrapped = true;
          break;
        } catch {
          await new Promise((resolve) => setTimeout(resolve, 100));
        }
      }
      assert(bootstrapped, "entrypoint first-user bootstrap");
      assert(
        (await evaluate(fresh!, "export default 42")).result === 42,
        "fresh ordinary Worker",
      );
      const login = await fetch(`${origin}/the8020/uui/login/`);
      await login.arrayBuffer();
      assert(login.status === 200, "fresh login");
      start(instance, `${instance}/node/kernel/bin/kernel`, oldPort);
      await waitReady(instance);
      await evaluate(
        instance,
        'import {setSystemProfile} from "/p/the8020/system/profile.ts"; export default () => setSystemProfile({name:"Upgrade fixture",role:"test"});',
      );
      await run(
        `${candidate}/admin`,
        [
          "--root",
          instance,
          "deployments.connect",
          origin,
          "upgrader",
          "--password-stdin",
        ],
        source,
        password + "\n",
      );
      const release = `${instance}/node/kernel/releases/0.10.17`;
      await Deno.mkdir(`${release}/scripts`, { recursive: true });
      await Deno.writeTextFile(
        `${release}/scripts/bootstrap-packages.toml`,
        targetList,
      );
      await Deno.writeTextFile(
        `${release}/release.json`,
        JSON.stringify({ version, commit }),
      );
      const preflight = () =>
        run(
          `${fresh}/node/kernel/runtime/images/rootless/rootfs/usr/bin/deno`,
          [
            "run",
            "-A",
            "--no-config",
            `${source}/defaults/kernel-scripts/update.ts`,
            instance,
            "--preflight",
            "0.10.17",
          ],
        );
      let rejected = false;
      try {
        await preflight();
      } catch (error) {
        rejected = String(error).includes("before kernel switching");
      }
      assert(rejected, "old set unexpectedly passed selector admission");
      assert(
        (await admin(instance, "kernel.status")).runtime_ready,
        "old kernel stopped on rejection",
      );
      const selections: {
        package_id: string;
        source: string;
        tag: string;
        before: string;
      }[] = [];
      for (const item of target) {
        const before = await run(
          "git",
          ["rev-parse", "HEAD"],
          `${instance}/packages/${item.id}`,
        );
        selections.push({
          package_id: item.id,
          source: peerSource(item.id),
          tag: item.version,
          before,
        });
      }
      const apply = (packages: typeof selections) =>
        run(
          `${candidate}/admin`,
          [
            "--root",
            instance,
            "kernel.packages.synchronize.set",
            "--input",
            JSON.stringify({
              activation_id: "act-" +
                crypto.randomUUID().replaceAll("-", "").slice(0, 10),
              packages,
            }),
            "--git-username",
            "upgrader",
            "--git-token-stdin",
          ],
          source,
          password + "\n",
        );
      const broken = selections.map((item, index) =>
        index === selections.length - 1
          ? { ...item, tag: "missing-upgrade-fixture" }
          : item
      );
      let failed = false;
      try {
        await apply(broken);
      } catch {
        failed = true;
      }
      assert(failed, "partial fetch failure accepted");
      for (const item of selections) {
        assert(
          await run(
            "git",
            ["rev-parse", "HEAD"],
            `${instance}/packages/${item.package_id}`,
          ) === item.before,
          "partial failure changed source",
        );
      }
      await apply(selections);
      await preflight();
      for (const item of selections) {
        const record =
          (await admin(instance, "kernel.packages.inspect", item.package_id))
            .package;
        assert(
          record.source === item.source && record.state === "ready",
          `source identity/readiness: ${item.package_id}`,
        );
        assert(
          await run(
            "git",
            ["rev-parse", "HEAD"],
            `${instance}/packages/${item.package_id}`,
          ) ===
            await run(
              "git",
              ["rev-parse", `${item.tag}^{commit}`],
              `${fresh}/packages/${item.package_id}`,
            ),
          `target commit: ${item.package_id}`,
        );
      }
      await Deno.writeTextFile(
        `${instance}/users/keep`,
        "private fixture work",
      );
      await admin(instance, "kernel.shutdown");
      await children.pop()!.process.status;
      for (const name of ["definitions", "images"]) {
        await Deno.remove(`${instance}/node/kernel/runtime/${name}`, {
          recursive: true,
        });
        await run("cp", [
          "-a",
          `${fresh}/node/kernel/runtime/${name}/.`,
          `${instance}/node/kernel/runtime/${name}`,
        ]);
      }
      await Deno.mkdir(`${instance}/node/kernel/scripts`, { recursive: true });
      await Deno.writeTextFile(
        `${instance}/node/kernel/scripts/bootstrap-packages.toml`,
        targetList,
      );
      start(instance, `${candidate}/kernel`, oldPort);
      await waitReady(instance);
      assert(
        (await admin(instance, "kernel.packages.synchronize.set"))
          .node_installations === true,
        "replacement startup did not adopt node-aware storage",
      );
      await admin(instance, "kernel.packages.recommended");
      assert(
        (await evaluate(instance, "export default 42")).result === 42,
        "replacement ordinary Worker",
      );
      const upgradedLogin = await fetch(
        `http://127.0.0.1:${oldPort}/the8020/uui/login/`,
      );
      await upgradedLogin.arrayBuffer();
      assert(upgradedLogin.status === 200, "replacement login");
      assert(
        await Deno.readTextFile(`${instance}/users/keep`) ===
          "private fixture work",
        "private work changed",
      );
      for (const item of selections) {
        const record =
          (await admin(instance, "kernel.packages.inspect", item.package_id))
            .package;
        assert(
          record.source === item.source && record.state === "ready",
          `replacement provenance: ${item.package_id}`,
        );
      }
      console.log(
        "Real old installed set, approved authenticated development transport, fail-before-switch, partial fetch preservation and same-target retry, replacement startup, node-aware recommendations and private-work preservation passed.",
      );
    } finally {
      for (const child of children.reverse()) {
        try {
          await admin(child.instance, "kernel.shutdown");
        } catch {
          try {
            child.process.kill("SIGTERM");
          } catch { /* Already exited; preserve the qualification error. */ }
        }
        await child.process.status;
      }
    }
  },
});
