package development

import (
	"context"
	"path/filepath"
	"testing"
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
}
