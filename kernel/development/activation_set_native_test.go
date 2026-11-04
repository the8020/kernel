package development

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"the8020/kernel/database"
	"the8020/kernel/database/evaluator"
	"the8020/kernel/database/schematest"
	"the8020/kernel/deployment"
	workspacepackages "the8020/kernel/packages"
)

func TestNativeDevelopmentMutualImports(t *testing.T) {
	for _, order := range [][]string{{"fixture/alpha", "fixture/beta"}, {"fixture/beta", "fixture/alpha"}} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			m, driver, _ := nativeRuntime(t)
			deno, err := exec.LookPath("deno")
			if err != nil {
				t.Skip("requires host Deno for candidate import checks")
			}
			ctx := context.Background()
			for _, id := range order {
				root := filepath.Join(m.config.PackagesRoot, id)
				writeTestFile(t, filepath.Join(root, "package.toml"), "schema = 1\n")
				writeTestFile(t, filepath.Join(root, "old.ts"), "export default 'old';\n")
				initializeTestRepository(t, m, id, "Fixture", "fixture@example.test", "Base")
			}
			sandbox, err := m.Create(ctx, "mutual")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = m.Delete(context.Background(), sandbox.UserID) })
			nativeExec(t, driver, sandbox, `cat > /workspace/packages/fixture/alpha/mod.ts <<'EOF'
import { beta } from '/p/fixture/beta/mod.ts';
export function alpha() { return 'alpha'; }
export function result() { return beta(); }
EOF
cat > /workspace/packages/fixture/beta/mod.ts <<'EOF'
import { alpha } from '/p/fixture/alpha/mod.ts';
export function beta() { return alpha() + '-beta'; }
EOF`)
			check := func(roots map[string]string) error {
				imports := map[string]string{}
				for id, root := range roots {
					imports["/p/"+id+"/"] = "file://" + root + "/"
				}
				data, err := json.Marshal(map[string]any{"imports": imports})
				if err != nil {
					return err
				}
				mapping := filepath.Join(m.config.Root, "mutual-imports.json")
				if err := os.WriteFile(mapping, data, 0600); err != nil {
					return err
				}
				command := exec.CommandContext(ctx, deno, "eval", "--no-config", "--import-map", mapping, `const m = await import('/p/fixture/alpha/mod.ts'); if (m.result() !== 'alpha-beta') throw Error('unexpected result');`)
				if output, err := command.CombinedOutput(); err != nil {
					return fmt.Errorf("mutual imports: %s: %w", output, err)
				}
				return nil
			}
			prepared, completed := 0, 0
			m.SetSchemaDeployment(nativeActivationHook{
				prepare: func(_ context.Context, _ string, candidates []deployment.Candidate) error {
					prepared++
					roots := map[string]string{}
					for _, candidate := range candidates {
						roots[candidate.PackageID] = candidate.Root
					}
					if len(roots) != 2 || roots["fixture/alpha"] == "" || roots["fixture/beta"] == "" {
						return fmt.Errorf("expected both selected candidates: %v", roots)
					}
					for _, id := range order {
						if _, err := os.Stat(filepath.Join(m.config.PackagesRoot, id, "mod.ts")); !os.IsNotExist(err) {
							return fmt.Errorf("candidate already published: %s: %v", id, err)
						}
					}
					return check(roots)
				},
				complete: func(_ context.Context, _ string, activated bool) error {
					if !activated {
						return nil
					}
					completed++
					return check(map[string]string{
						"fixture/alpha": filepath.Join(m.config.PackagesRoot, "fixture/alpha"),
						"fixture/beta":  filepath.Join(m.config.PackagesRoot, "fixture/beta"),
					})
				},
			})
			result, err := m.Activate(ctx, sandbox.UserID, ActivationOptions{Description: "Mutual imports", SelectedPackages: order})
			if err != nil || !result.Success || prepared != 1 || completed != 1 {
				t.Fatalf("activation: %+v, %v; prepare=%d complete=%d", result, err, prepared, completed)
			}
		})
	}
}

// Unlike the candidate-view boundary check above, this uses the real activation
// coordinator, evaluator and ordinary sandbox Workers for both hook phases.
func TestNativeDevelopmentMutualImportHooks(t *testing.T) {
	for _, order := range [][]string{{"fixture/alpha", "fixture/beta"}, {"fixture/beta", "fixture/alpha"}} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			m, driver, _ := nativeRuntime(t)
			ctx := t.Context()
			source, err := filepath.Abs("../../..")
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"db", "packages", "secrets", "system", "i18n"} {
				nativeStagePackage(t, filepath.Join(source, name), filepath.Join(m.config.PackagesRoot, "the8020", name))
				initializeTestRepository(t, m, "the8020/"+name, "Fixture", "fixture@example.test", "Schema fixture")
			}
			for _, id := range order {
				writeTestFile(t, filepath.Join(m.config.PackagesRoot, id, "package.toml"), "schema = 1\ndescription = 'Development cycle'\n")
				initializeTestRepository(t, m, id, "Fixture", "fixture@example.test", "Before cycle")
			}
			db := database.New(database.Config{Backend: database.BackendSQLite, Location: filepath.Join(m.config.Root, "database/system.db"), InstanceRoot: m.config.Root, MaximumOpenConnections: 8, MaximumIdleConnections: 2})
			t.Cleanup(func() { _ = db.Close() })
			schematest.Attach(t, db)
			if _, err := db.InitializeCatalog(ctx); err != nil {
				t.Fatal(err)
			}
			runner := nativeSchemaJobs(t, m, db)
			catalog, err := workspacepackages.NewCatalog(m.config.PackagesRoot, nil)
			if err != nil {
				t.Fatal(err)
			}
			evaluation, err := evaluator.New(evaluator.Config{Packages: catalog, Jobs: runner, Database: db})
			if err != nil {
				t.Fatal(err)
			}
			db.SetDefinitionEvaluator(evaluation.Evaluate)
			if _, err := evaluation.SynchronizeInitialSchemas(ctx, false); err != nil {
				t.Fatal(err)
			}
			store, err := workspacepackages.New(workspacepackages.Config{WorkspaceRoot: m.config.Root, PackagesRoot: m.config.PackagesRoot, Database: db})
			if err != nil {
				t.Fatal(err)
			}
			owner, err := workspacepackages.NewActivationCoordinator(workspacepackages.ActivationCoordinatorConfig{Database: db, Schema: evaluation, Packages: store, Jobs: runner})
			if err != nil {
				t.Fatal(err)
			}
			commits, err := evaluation.PackageSet(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := owner.Bootstrap(ctx, commits); err != nil {
				t.Fatal(err)
			}
			if err := db.CompleteInitialization(ctx, commits); err != nil {
				t.Fatal(err)
			}
			if err := evaluation.UseActivatedPackages(store); err != nil {
				t.Fatal(err)
			}
			m.SetSchemaDeployment(owner)
			sandbox, err := m.Create(ctx, "cycle")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = m.Delete(context.Background(), sandbox.UserID) })
			for _, name := range []string{"alpha", "beta"} {
				peer := "alpha"
				if name == "alpha" {
					peer = "beta"
				}
				files := map[string]string{
					"mod.ts":          fmt.Sprintf("import {value as peer} from '/p/fixture/%s/mod.ts'; export function value(){return 'new';} export function check(){if(peer()!=='new')throw Error('unstaged peer');}\n", peer),
					"tables/value.ts": fmt.Sprintf("import {check} from '../mod.ts'; import {t,table} from '/p/the8020/db/mod.ts'; check(); export default table('fixture__%s__value',{id:t.text().primaryKey()});\n", name),
				}
				for _, phase := range []string{"pre", "post"} {
					files["hooks/"+phase+".toml"] = fmt.Sprintf("hook = '%s-activate'\ndescription = 'Cycle check'\nprogram = 'fixture/%s/%s'\n", phase, name, phase)
					files["programs/"+phase+"/program.toml"] = "entrypoint = 'program.ts'\n"
					files["programs/"+phase+"/program.ts"] = "import {check} from '../../mod.ts'; export default check;\n"
				}
				for file, body := range files {
					path := "/workspace/packages/fixture/" + name + "/" + file
					nativeExec(t, driver, sandbox, "mkdir -p "+shellQuote(filepath.Dir(path))+"; printf %s "+shellQuote(body)+" >"+shellQuote(path))
				}
			}
			result, err := m.Activate(ctx, sandbox.UserID, ActivationOptions{Description: "Real mutual imports", SelectedPackages: order})
			if err != nil || !result.Success {
				t.Fatalf("activation: %+v %v", result, err)
			}
			var succeeded int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "the8020__packages__hook_runs" WHERE "packageId" IN ('fixture/alpha','fixture/beta') AND "state" = 'succeeded' AND "attempts" = 1`).Scan(&succeeded); err != nil || succeeded != 4 {
				t.Fatalf("real hook checkpoints: %d %v", succeeded, err)
			}
		})
	}
}
