package development

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"the8020/kernel/database"
	"the8020/kernel/database/evaluator"
	workspacepackages "the8020/kernel/packages"
)

func TestNativeSchemaEvolution(t *testing.T) {
	m, sparse, sandbox, shared := nativeSparseRuntime(t, "evolution", 1)
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
	defer cancel()
	source, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"db", "packages", "secrets", "system"} {
		destination := filepath.Join(m.config.PackagesRoot, "the8020", name)
		nativeStagePackage(t, filepath.Join(source, name), destination)
		if name == "db" {
			for _, file := range []string{"schema.ts", "catalog.ts"} {
				current, err := os.ReadFile(filepath.Join(destination, "internal", file))
				if err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(destination, "internal", "next-"+file+".fixture"), string(current))
				previous, err := gitCommand(ctx, filepath.Join(source, name), nil, "show", "0.7.0:internal/"+file)
				if err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(destination, "internal", file), string(previous))
			}
		}
		initializeTestRepository(t, m, "the8020/"+name, "Fixture", "fixture@example.test", "Evolution fixture")
	}
	before := `import {t,table} from "/p/the8020/db/mod.ts";
export default table("the8020__dev_core__evolution",{id:t.integer().generated().primaryKey(),status:t.enum(["OPEN","CANCELLED"]).default("OPEN"),total:t.decimal(12,2),legacy:t.text().nullable()});
export function migrate(){throw new Error("fresh and automatic paths must skip migrate");}
`
	writeTestFile(t, filepath.Join(shared, "tables/evolution.ts"), before)
	writeTestFile(t, filepath.Join(shared, "tables/gate.ts"), `import {t,table} from "/p/the8020/db/mod.ts";export default table("the8020__dev_core__gate",{id:t.text().primaryKey(),state:t.text()});`)
	if _, err := gitCommand(ctx, shared, nil, "add", "tables"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-m", "Initial evolution tables"); err != nil {
		t.Fatal(err)
	}
	db := database.New(database.Config{Backend: database.BackendSQLite, Location: filepath.Join(m.config.Root, "database/system.db"), InstanceRoot: m.config.Root, MaximumOpenConnections: 8, MaximumIdleConnections: 2})
	t.Cleanup(func() { _ = db.Close() })
	runner := nativeSchemaJobs(t, m, db)
	catalog, err := workspacepackages.NewCatalog(m.config.PackagesRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	evaluation, err := evaluator.New(evaluator.Config{Packages: catalog, Jobs: runner, Database: db})
	if err != nil {
		t.Fatal(err)
	}
	db.SetSchemaExecutor(evaluation.RunSchema)
	db.SetDefinitionEvaluator(evaluation.Evaluate)
	if _, err := db.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := evaluation.SynchronizeInitialSchemas(ctx, false); err != nil {
		t.Fatal(err)
	}
	store, err := workspacepackages.New(workspacepackages.Config{WorkspaceRoot: m.config.Root, PackagesRoot: m.config.PackagesRoot, Database: db})
	if err != nil {
		t.Fatal(err)
	}
	makeOwner := func() *workspacepackages.ActivationCoordinator {
		owner, err := workspacepackages.NewActivationCoordinator(workspacepackages.ActivationCoordinatorConfig{Database: db, Schema: evaluation, Packages: store, Jobs: runner})
		if err != nil {
			t.Fatal(err)
		}
		m.SetSchemaDeployment(owner)
		return owner
	}
	owner := makeOwner()
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
	// Refresh the private baseline after fixture source installation.
	if _, err := m.ResetSource(ctx, sandbox.UserID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO the8020__dev_core__evolution (id,status,total,legacy) VALUES (7,'CANCELLED',123,'retained'),(9,'OPEN',456,'other')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO the8020__dev_core__gate (id,state) VALUES ('unrelated','allow')`); err != nil {
		t.Fatal(err)
	}
	// Upgrade the real v2 schema owner, then recover a hook failure before its source switch.
	if db.Status().CatalogVersion != 2 {
		t.Fatal("fixture did not start with catalog v2")
	}
	nativeExec(t, sparse.RunscDriver, sandbox, `set -e; cd /workspace/packages/the8020/db; cp internal/next-schema.ts.fixture internal/schema.ts; cp internal/next-catalog.ts.fixture internal/catalog.ts; mkdir -p hooks programs/upgrade-gate; printf '%s' 'hook="pre-activate"
description="Upgrade gate"
program="the8020/db/upgrade-gate"
' >hooks/upgrade-gate.toml; printf '%s' 'entrypoint="main.ts"' >programs/upgrade-gate/program.toml; printf '%s' 'import gate from "/p/the8020/dev-core/tables/gate.ts";export default async function(){const row=await gate.selectAll().where("id","=","unrelated").executeTakeFirstOrThrow();if(row.state!=="allow")throw new Error("upgrade gate unavailable");}' >programs/upgrade-gate/main.ts`)
	if _, err := db.ExecContext(ctx, `UPDATE the8020__dev_core__gate SET state='blocked'`); err != nil {
		t.Fatal(err)
	}
	upgrade := nativeExec(t, sparse.RunscDriver, sandbox, "activate --json --package the8020/db --message 'Upgrade schema owner'; true")
	var upgradeResult ActivationResult
	if err := json.Unmarshal([]byte(upgrade), &upgradeResult); err != nil || upgradeResult.Success || !strings.Contains(upgradeResult.Error, "schema/data committed") {
		t.Fatalf("catalog upgrade interruption: %s %v", upgrade, err)
	}
	if db.Status().CatalogVersion != 3 {
		t.Fatal("catalog upgrade did not commit")
	}
	if _, err := db.ExecContext(ctx, `UPDATE the8020__dev_core__gate SET state='allow'`); err != nil {
		t.Fatal(err)
	}
	owner = makeOwner()
	if err := owner.Recover(ctx); err != nil {
		t.Fatalf("DB-package upgrade recovery: %v", err)
	}
	upgrade = nativeExec(t, sparse.RunscDriver, sandbox, "activate --json --package the8020/db --message 'Upgrade schema owner'; true")
	if err := json.Unmarshal([]byte(upgrade), &upgradeResult); err != nil || !upgradeResult.Success {
		t.Fatalf("DB-package upgrade acknowledgement: %s %v", upgrade, err)
	}
	prefix := "set -e; cd /workspace/packages/the8020/dev-core; "
	activate := func(message string, force bool) ActivationResult {
		command := "activate --json --package the8020/dev-core --message " + shellQuote(message)
		if force {
			command += " --force the8020__dev_core__evolution.status"
		}
		// Preserve failures as JSON; assertions below establish the actual outcome.
		output := nativeExec(t, sparse.RunscDriver, sandbox, command+"; true")
		var result ActivationResult
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			t.Fatalf("activation JSON: %v: %s", err, output)
		}
		return result
	}
	edit := func(name, body string) {
		nativeExec(t, sparse.RunscDriver, sandbox, prefix+"mkdir -p "+shellQuote(filepath.Dir(name))+"; printf %s "+shellQuote(body)+" >"+shellQuote(name))
	}
	widened := strings.Replace(before, `["OPEN","CANCELLED"]`, `["OPEN","CANCELLED","PAUSED"]`, 1)
	edit("tables/evolution.ts", widened)
	if result := activate("Automatic enum widening", false); !result.Success {
		t.Fatalf("automatic: %+v", result)
	}
	cleanup := `import {t,table} from "/p/the8020/db/mod.ts";
export default table("the8020__dev_core__evolution",{id:t.integer().generated().primaryKey(),status:t.enum(["OPEN","PAUSED"]).default("OPEN"),total:t.decimal(12,2),display:t.text().default("ready")});
export async function migrate({table,tx}){await tx.deleteFrom(table).where("status","=","CANCELLED").execute();}
`
	edit("tables/evolution.ts", cleanup)
	output := nativeExec(t, sparse.RunscDriver, sandbox, "activate --preview --schema --json --package the8020/dev-core --message 'Staged cleanup'")
	var preview ActivationPreview
	if err := json.Unmarshal([]byte(output), &preview); err != nil || len(preview.SchemaPlan) == 0 {
		t.Fatalf("metadata preview: %s %v", output, err)
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM the8020__dev_core__evolution WHERE id=7`).Scan(&status); err != nil || status != "CANCELLED" {
		t.Fatalf("preview ran cleanup: %s %v", status, err)
	}
	if result := activate("Staged cleanup", true); !result.Success || !strings.Contains(string(result.SchemaResult), `"cleanup"`) {
		t.Fatalf("staged cleanup: %+v", result)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM the8020__dev_core__evolution WHERE id=7`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("intentional cleanup deletion %d %v", count, err)
	}
	var legacy string
	if err := db.QueryRowContext(ctx, `SELECT legacy FROM the8020__dev_core__evolution WHERE id=9`).Scan(&legacy); err != nil || legacy != "other" {
		t.Fatalf("retired data %s %v", legacy, err)
	}
	edit("tables/evolution.ts", cleanup+"\n// Function code alone must not rerun cleanup.\n")
	if result := activate("Retain cleanup", false); !result.Success {
		t.Fatalf("code-only cleanup reran: %+v", result)
	}
	var firstGenerated int
	if err := db.QueryRowContext(ctx, `INSERT INTO the8020__dev_core__evolution (status,total) VALUES ('OPEN',100) RETURNING id`).Scan(&firstGenerated); err != nil {
		t.Fatal(err)
	}
	failed := strings.ReplaceAll(cleanup, `"OPEN"`, `"FINAL"`)
	start := strings.Index(failed, "export async function migrate")
	failed = failed[:start] + `export async function migrate({tx,table}){await tx.updateTable(table).set({status:"PAUSED"}).where("status","=","OPEN").execute();throw new Error("failed after cleanup");}`
	edit("tables/evolution.ts", failed)
	if result := activate("Column repair", false); result.Success || !strings.Contains(result.Error, "failed after cleanup") {
		t.Fatalf("cleanup rollback: %+v", result)
	}
	if err := db.QueryRowContext(ctx, `SELECT status FROM the8020__dev_core__evolution WHERE id=9`).Scan(&status); err != nil || status != "OPEN" {
		t.Fatalf("failed cleanup changed row %s %v", status, err)
	}
	edit("hooks/gate.toml", `hook="pre-activate"
description="Transient fixture gate"
program="the8020/dev-core/gate"
`)
	edit("programs/gate/program.toml", "entrypoint=\"main.ts\"\n")
	edit("programs/gate/main.ts", `import gate from "../../tables/gate.ts";export default async function(){const row=await gate.selectAll().where("id","=","unrelated").executeTakeFirstOrThrow();if(row.state!=="allow")throw new Error("gate unavailable");}`)
	repaired := activate("Column repair", true)
	if !repaired.Success || !strings.Contains(string(repaired.SchemaResult), `"repair"`) {
		t.Fatalf("column repair: %+v", repaired)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM the8020__dev_core__evolution WHERE status='FINAL'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("repair erased rows %d %v", count, err)
	}
	var generated int
	if err := db.QueryRowContext(ctx, `INSERT INTO the8020__dev_core__evolution (status,total) VALUES ('FINAL',250) RETURNING id`).Scan(&generated); err != nil || generated != firstGenerated+1 {
		t.Fatalf("identity reset %d %v", generated, err)
	}
	if result := activate("Publish gate hook", false); !result.Success {
		t.Fatalf("gate publication: %+v", result)
	}
	edit("tables/evolution.ts", strings.ReplaceAll(failed, `"FINAL"`, `"LAST"`))
	retryOptions := ActivationOptions{Description: "Forward-only repair", SelectedPackages: []string{"the8020/dev-core"}, ForceColumns: []string{"the8020__dev_core__evolution.status"}}
	previewOptions := retryOptions
	previewOptions.SchemaPreview = true
	retryPreview, err := m.Preview(ctx, sandbox.UserID, previewOptions)
	if err != nil {
		t.Fatal(err)
	}
	var approved struct{ Identity string }
	if err := json.Unmarshal(retryPreview.SchemaPlan, &approved); err != nil || approved.Identity == "" {
		t.Fatalf("retry preview: %s %v", retryPreview.SchemaPlan, err)
	}
	retryOptions.ApprovedPlan = approved.Identity
	synchronizations := 0
	activationID := ""
	db.SetSchemaExecutor(func(ctx context.Context, request database.SchemaRequest) (json.RawMessage, error) {
		encoded, err := evaluation.RunSchema(ctx, request)
		if request.Operation == "synchronize" && err == nil {
			var response struct{ Error string }
			if err := json.Unmarshal(encoded, &response); err != nil || response.Error != "" {
				return encoded, err
			}
			options := request.Input.(map[string]any)["options"].(database.SynchronizationOptions)
			if options.ApprovedPlan != retryOptions.ApprovedPlan || len(options.ForceColumns) != 1 || options.ForceColumns[0] != retryOptions.ForceColumns[0] {
				t.Fatal("lost-response retry changed the original consent")
			}
			if activationID != "" && activationID != options.ActivationID {
				t.Fatal("lost-response retry changed activation identity")
			}
			activationID = options.ActivationID
			synchronizations++
			if synchronizations == 1 {
				return nil, errors.New("fixture lost schema response after commit")
			}
		}
		return encoded, err
	})
	if _, err := db.ExecContext(ctx, `UPDATE the8020__dev_core__gate SET state='blocked'`); err != nil {
		t.Fatal(err)
	}
	interrupted, err := m.Activate(ctx, sandbox.UserID, retryOptions)
	if err == nil || interrupted.Success || !strings.Contains(err.Error(), "fixture lost schema response after commit") {
		t.Fatalf("lost schema response: %+v %v", interrupted, err)
	}
	var stage string
	if err := db.QueryRowContext(ctx, `SELECT stage FROM the8020__packages__activations WHERE "activationId"=$1`, activationID).Scan(&stage); err != nil || stage != "staged" {
		t.Fatalf("lost response passed schema checkpoint: %s %v", stage, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO the8020__dev_core__evolution (status,total,display) VALUES ('LAST',350,'after commit')`); err != nil {
		t.Fatal(err)
	}
	interrupted, err = m.Activate(ctx, sandbox.UserID, retryOptions)
	if err == nil || interrupted.Success || !strings.Contains(err.Error(), "gate unavailable") || synchronizations != 2 {
		t.Fatalf("original consent did not reach post-schema hook: %+v %v (synchronizations %d)", interrupted, err, synchronizations)
	}
	if err := db.QueryRowContext(ctx, `SELECT stage FROM the8020__packages__activations WHERE "activationId"=$1`, activationID).Scan(&stage); err != nil || stage != "schema_synchronized" {
		t.Fatalf("retry missed schema checkpoint: %s %v", stage, err)
	}
	db.SetSchemaExecutor(evaluation.RunSchema)
	if _, err := db.ExecContext(ctx, `UPDATE the8020__dev_core__gate SET state='allow'`); err != nil {
		t.Fatal(err)
	}
	owner = makeOwner()
	if err := owner.Recover(context.Background()); err != nil {
		t.Fatalf("forward repair recovery: %v", err)
	}
	if result := activate("Forward-only repair", false); !result.Success {
		t.Fatalf("recovery acknowledgement %+v", result)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM the8020__dev_core__evolution WHERE id=$1 AND display='after commit'`, firstGenerated+2).Scan(&count); err != nil || count != 1 {
		t.Fatalf("repair replayed %d %v", count, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT legacy FROM the8020__dev_core__evolution WHERE id=9`).Scan(&legacy); err != nil || legacy != "other" {
		t.Fatalf("unrelated retired cell changed %s %v", legacy, err)
	}
	journal := filepath.Join(m.sandboxRoot(sandbox), "activation/active.json")
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("recovery left journal %v", err)
	}
}
