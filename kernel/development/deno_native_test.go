package development

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"the8020/kernel/deployment"
)

func TestNativeDevelopmentDeno(t *testing.T) {
	manager, driver, repository := nativeRuntime(t)
	writeTestFile(t, filepath.Join(repository, "deno.json"), `{
  "imports": {"fixture": "./fixture.ts"},
  "tasks": {"check": "deno check --no-remote main.ts"}
}`)
	shared := filepath.Join(filepath.Dir(repository), "deno-types")
	writeTestFile(t, filepath.Join(shared, "package.toml"), "schema = 1\n")
	writeTestFile(t, filepath.Join(shared, "deno.json"), `{"imports": {"fixture": "./limit.ts"}}`)
	writeTestFile(t, filepath.Join(shared, "limit.ts"), `export default 10;`)
	writeTestFile(t, filepath.Join(shared, "shared.ts"), `import type { LogQuery } from "@the8020/kernel";
import limit from "fixture";
export const query: LogQuery = { limit };
`)
	writeTestFile(t, filepath.Join(repository, "fixture.ts"), `export default "typecheck";`)
	writeTestFile(t, filepath.Join(repository, "main.ts"), `import { kernel } from "@the8020/kernel";
import { context } from "@the8020/context";
import { query } from "/p/the8020/deno-types/shared.ts";
import event from "fixture";
const username: string = context.username;
kernel.logs.query(query);
kernel.events.emit(event, { username });
`)
	sandbox, err := manager.Create(context.Background(), "denoproof")
	if err != nil {
		t.Fatal(err)
	}
	nativeExec(t, driver, sandbox, "set -e; cd /workspace/packages/the8020/dev-core; deno task check; deno check --no-remote --config=deno.json main.ts")
	nativeExec(t, driver, sandbox, `set -eu
cd /workspace
deno check --no-remote packages/the8020/dev-core/main.ts
for file in /workspace/deno.json /opt/runtime/kernel/mod.ts /opt/runtime/protocol.ts; do
  if (printf forbidden >> "$file") 2>/dev/null; then exit 1; fi
done
printf 'import { context } from "@the8020/context"; const wrong: number = context.username;\n' > invalid.ts
if deno check --no-remote invalid.ts > /tmp/typecheck-error 2>&1; then exit 1; fi
grep -F TS2322 /tmp/typecheck-error
`)
	nativeDenoLanguageServer(t, driver, sandbox)
}

func TestNativeDevelopmentDenoDiscovery(t *testing.T) {
	m, driver, repository := nativeRuntime(t)
	ctx := context.Background()
	writeTestFile(t, filepath.Join(repository, "deno.json"), `{}`)
	sandbox, err := m.Create(ctx, "discovery")
	if err != nil {
		t.Fatal(err)
	}
	m.SetSchemaDeployment(nativeActivationHook{
		prepare:  func(context.Context, string, []deployment.Candidate) error { return nil },
		complete: func(context.Context, string, bool) error { return nil },
	})
	members := func() []string {
		t.Helper()
		var config struct{ Workspace []string }
		if err := json.Unmarshal([]byte(nativeExec(t, driver, sandbox, "cat /workspace/deno.json")), &config); err != nil {
			t.Fatal(err)
		}
		for _, member := range config.Workspace {
			if strings.ContainsAny(member, "*?") || strings.Contains(member, "/.") {
				t.Fatalf("unsafe member %q", member)
			}
		}
		return config.Workspace
	}
	expect := func(name string, present bool) {
		t.Helper()
		got := members()
		if slices.Contains(got, "./packages/"+name) != present {
			t.Fatalf("%s present=%v: %v", name, present, got)
		}
	}
	expect("the8020/dev-core", true)
	// Retain the old generated config only as a disposable before/control input.
	nativeExec(t, driver, sandbox, `deno eval --no-config 'const c=JSON.parse(Deno.readTextFileSync("/workspace/deno.json")); c.workspace=["./packages/*/*"]; Deno.writeTextFileSync("/workspace/legacy.json",JSON.stringify(c));'`)
	measure := func(label, command string) {
		t.Helper()
		t.Logf("%s: %s", label, nativeExec(t, driver, sandbox, "set -e; cd /workspace; export DENO_NO_UPDATE_CHECK=1; TIMEFORMAT='wall=%R user=%U sys=%S'; for n in 1 2 3; do { time "+command+"; } 2>&1; done"))
	}
	measure("glob-small", "deno eval --cached-only --config=legacy.json 1")
	for i := 0; i < 64; i++ {
		for j := 0; j < 128; j++ {
			writeTestFile(t, filepath.Join(m.config.PackagesRoot, ".conflicts/retained/source", fmt.Sprint(i), fmt.Sprint(j)+".txt"), "retained")
		}
	}
	measure("glob-conflicts", "deno eval --cached-only --config=legacy.json 1")
	measure("explicit-conflicts", "deno eval --cached-only 1")
	measure("helper-glob-conflicts", "deno run --config=legacy.json --allow-env=DEVELOPMENT_USER_ID,DEVELOPMENT_ACTIVATION_ENDPOINT,DEVELOPMENT_ACTIVATION_TOKEN --allow-net=127.0.0.1 /workspace/scripts/activate.ts --help >/dev/null")
	measure("helper-conflicts", "activate --help >/dev/null")
	writeTestFile(t, filepath.Join(m.config.PackagesRoot, ".conflicts/poison/deno.json"), "invalid config")
	nativeExec(t, driver, sandbox, `set -eu
cd /workspace
deno eval --cached-only 1
mkdir -p /workspace/packages/broken/config
printf 'invalid config' >/workspace/packages/broken/config/deno.json
if deno eval --cached-only 1 >/tmp/config-error 2>&1; then exit 1; fi
activate --help >/dev/null
sql --help >/dev/null
kernel-logs --help >/dev/null
uui --help >/dev/null
if deno run --no-config /workspace/scripts/activate.ts --preview --json >/tmp/permission-error 2>&1; then exit 1; fi
grep -F 'Requires env access' /tmp/permission-error
rm -r /workspace/packages/broken
activate --preview --json
mkdir -p /workspace/packages/probe/private
printf 'schema = 1\n' >/workspace/packages/probe/private/package.toml
printf '%s' '{"name":"@probe/private","version":"1.0.0","exports":"./mod.ts","imports":{"value":"./value.ts"}}' >/workspace/packages/probe/private/deno.jsonc
printf 'export const value: number = 42;\n' >/workspace/packages/probe/private/value.ts
printf 'export { value } from "value";\n' >/workspace/packages/probe/private/mod.ts
printf 'import { value } from "@probe/private"; const answer: number = value;\n' >/workspace/private-check.ts
deno check --no-remote private-check.ts
`)
	expect("probe/private", true)
	nativeExec(t, driver, sandbox, "mv /workspace/packages/probe/private /workspace/packages/probe/renamed; cd /workspace; deno check --no-remote private-check.ts")
	expect("probe/private", false)
	expect("probe/renamed", true)
	activate := func() {
		t.Helper()
		result, err := m.Activate(ctx, sandbox.UserID, ActivationOptions{Description: "Deno membership fixture", SelectedPackages: []string{"probe/renamed"}, AuthorName: "Jan Tomášek", AuthorEmail: "tomasekhonza@gmail.com"})
		if err != nil || !result.Success {
			t.Fatalf("activate: %+v, %v", result, err)
		}
	}
	activate()
	expect("probe/renamed", true)
	if _, err := os.Stat(filepath.Join(m.config.PackagesRoot, "probe/renamed/deno.jsonc")); err != nil {
		t.Fatal(err)
	}
	nativeExec(t, driver, sandbox, "rm /workspace/packages/probe/renamed/deno.jsonc")
	expect("probe/renamed", false)
	nativeExec(t, driver, sandbox, `printf '%s' '{"name":"@probe/private","exports":"./mod.ts"}' >/workspace/packages/probe/renamed/package.json`)
	expect("probe/renamed", true)
	nativeExec(t, driver, sandbox, "printf 'export const value = 42;\\n' >/workspace/packages/probe/renamed/mod.ts; cd /workspace; deno check --no-remote private-check.ts")
	// External package installation/removal needs no sandbox restart or publication callback.
	external := filepath.Join(m.config.PackagesRoot, "other/installed")
	writeTestFile(t, filepath.Join(external, "package.json"), `{"name":"installed"}`)
	expect("other/installed", true)
	if err := os.RemoveAll(external); err != nil {
		t.Fatal(err)
	}
	expect("other/installed", false)
	if _, err := m.Stop(ctx, sandbox.UserID); err != nil {
		t.Fatal(err)
	}
	sandbox, err = m.Start(ctx, sandbox.UserID)
	if err != nil {
		t.Fatal(err)
	}
	expect("probe/renamed", true)
	nativeExec(t, driver, sandbox, "cd /workspace; deno check --no-remote private-check.ts")
	nativeExec(t, driver, sandbox, "rm -r /workspace/packages/probe/renamed")
	expect("probe/renamed", false)
	activate()
	expect("probe/renamed", false)
	if _, err := os.Stat(filepath.Join(m.config.PackagesRoot, "probe/renamed")); !os.IsNotExist(err) {
		t.Fatalf("shared package not removed: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(m.config.PackagesRoot, ".conflicts/retained/source/0/0.txt")); err != nil || string(data) != "retained" {
		t.Fatal("retained conflict data changed")
	}
}
