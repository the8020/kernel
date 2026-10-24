package development

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"the8020/kernel/database"
	"the8020/kernel/database/evaluator"
	"the8020/kernel/deployment"
	workspacepackages "the8020/kernel/packages"
)

// Abrupt loss leaves the workspace journal and native activation admission durable.
type interruptedSchema struct {
	*evaluator.Evaluator
	interrupt bool
}

func (h *interruptedSchema) Prepare(ctx context.Context, id string, candidates []deployment.Candidate) error {
	if h.interrupt {
		h.interrupt = false
		panic("fixture process interruption before schema preparation")
	}
	return h.Evaluator.Prepare(ctx, id, candidates)
}

func TestNativeMissingSourceRecovery(t *testing.T) {
	for _, first := range []bool{false, true} {
		name := "existing"
		if first {
			name = "new"
		}
		t.Run(name, func(t *testing.T) {
			m, sparse, sandbox, _ := nativeSparseRuntime(t, "recovery", 1)
			ctx := t.Context()
			source, err := filepath.Abs("../../..")
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"db", "packages", "secrets", "system"} {
				nativeStagePackage(t, filepath.Join(source, name), filepath.Join(m.config.PackagesRoot, "the8020", name))
				initializeTestRepository(t, m, "the8020/"+name, "Jan Tomášek", "tomasekhonza@gmail.com", "Disposable schema fixture")
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
			hook := &interruptedSchema{Evaluator: evaluation}
			owner, err := workspacepackages.NewActivationCoordinator(workspacepackages.ActivationCoordinatorConfig{Database: db, Schema: hook, Packages: store, Jobs: runner})
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
			id := "the8020/dev-core"
			if first {
				id = "example/new-package"
				nativeExec(t, sparse.RunscDriver, sandbox, "mkdir -p /workspace/packages/example/new-package; printf 'schema = 1\\n' >/workspace/packages/example/new-package/package.toml; printf 'new source\\n' >/workspace/packages/example/new-package/probe.txt")
			} else {
				nativeExec(t, sparse.RunscDriver, sandbox, "printf 'changed source\\n' >/workspace/packages/the8020/dev-core/probe.txt")
			}
			preview, err := m.Preview(ctx, sandbox.UserID, ActivationOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("preview succeeded: %+v", preview)
			hook.interrupt = true
			options := ActivationOptions{Description: "Disposable interruption probe", SelectedPackages: []string{id}}
			interrupted := false
			func() {
				defer func() {
					if value := recover(); value != nil {
						if value != "fixture process interruption before schema preparation" {
							panic(value)
						}
						interrupted = true
					}
				}()
				_, _ = m.Activate(ctx, sandbox.UserID, options)
			}()
			if !interrupted {
				t.Fatal("fault did not interrupt activation")
			}
			journal := filepath.Join(m.sandboxRoot(sandbox), "activation/active.json")
			var attempt activationAttempt
			if err := readJSON(journal, &attempt); err != nil || attempt.Phase != "preparing" {
				t.Fatalf("pending journal: %+v %v", attempt, err)
			}
			// Temporary staged code is lost, while durable private captures remain.
			for _, candidate := range attempt.Candidates {
				if err := os.RemoveAll(candidate.Root); err != nil {
					t.Fatal(err)
				}
			}
			for retry := 0; retry < 2; retry++ {
				forward, err := owner.ForwardRequired(ctx, attempt.TransactionID)
				if err != nil || forward {
					t.Fatalf("receipt retry %d: %t %v", retry, forward, err)
				}
			}
			if first {
				if _, err := os.Stat(filepath.Join(m.config.PackagesRoot, id)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("inspection created unpublished source: %v", err)
				}
			}
			result, err := m.Activate(ctx, sandbox.UserID, options)
			if err != nil || !result.Success {
				t.Fatalf("subsequent activation: %+v %v", result, err)
			}
			if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal remained: %v", err)
			}
			if body, err := os.ReadFile(filepath.Join(m.config.PackagesRoot, id, "probe.txt")); err != nil || !strings.Contains(string(body), "source") {
				t.Fatalf("source not published: %q %v", body, err)
			}
			var phase string
			if err := db.QueryRowContext(ctx, `SELECT stage FROM the8020__packages__activations WHERE "activationId"=$1`, attempt.TransactionID).Scan(&phase); err != nil || phase != "failed" {
				t.Fatalf("old activation not aborted: %s %v", phase, err)
			}
			t.Logf("absent-source inspection, abort and fresh activation succeeded: %+v", result)
		})
	}
}
