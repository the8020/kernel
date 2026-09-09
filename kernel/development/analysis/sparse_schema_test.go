//go:build workflowanalysis

package development

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"the8020/kernel/database"
	"the8020/kernel/database/evaluator"
	"the8020/kernel/deployment"
	"the8020/kernel/execution/coordinator"
	"the8020/kernel/execution/jobs"
	programrunner "the8020/kernel/execution/programs"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/execution/workers"
	"the8020/kernel/identity"
	"the8020/kernel/logging"
	"the8020/kernel/logging/records"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/ports"
	"the8020/kernel/runtime/protocol"
	"the8020/kernel/sandbox/backend/rootless"
	"the8020/kernel/sandbox/history"
	sandboxmanager "the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/sandbox/mounts"
	sandboxnetwork "the8020/kernel/sandbox/network"
	"the8020/kernel/sandbox/state"
)

// Use the ordinary native sandbox/group/Worker/job owners. Only registration,
// heartbeat and empty database-scope callback acknowledgements are fixture
// transport; evaluator results and activation hooks execute in real Deno Workers.
func analysisSchemaJobs(t *testing.T, m *Manager) (*jobs.Manager, map[string]any) {
	t.Helper()
	root := filepath.Join(m.config.Root, "node/schema-runtime")
	api := filepath.Join(root, "api")
	if err := os.MkdirAll(api, 0700); err != nil {
		t.Fatal(err)
	}
	source, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	imageRoot := filepath.Join(source, ".development/named-terminal-test/node/kernel/runtime/images/rootless")
	imageBytes, err := os.ReadFile(filepath.Join(imageRoot, "image.json"))
	if err != nil {
		t.Fatal(err)
	}
	var image map[string]any
	if err := json.Unmarshal(imageBytes, &image); err != nil {
		t.Fatal(err)
	}
	imageDigest, ok := image["image_digest"].(string)
	if !ok || imageDigest == "" {
		t.Fatal("schema fixture image has no digest")
	}
	logd := filepath.Join(root, "logd")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin/go"), "build", "-o", logd, "../logd")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build logd: %v: %s", err, output)
	}
	logs, err := logging.New(logging.Config{
		Directory: filepath.Join(root, "logs"), Socket: filepath.Join(api, "logs.sock"), Executable: logd,
		// Cover retained candidate sandboxes across all recovery scenarios.
		NodeID: "nod-0123456789", MaxProducers: 64,
		Policy: records.Policy{Enabled: true, Level: "info", SplitBy: "none", SplitPeriod: "day", MaxFileSize: 1 << 20, MaxTotalSize: 8 << 20, MaxAge: time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.Close() })
	for deadline := time.Now().Add(5 * time.Second); !logs.Status().Available; {
		if time.Now().After(deadline) {
			t.Fatal("schema fixture logd did not become available")
		}
		time.Sleep(10 * time.Millisecond)
	}
	listener, err := net.Listen("unix", filepath.Join(api, "kernel.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&envelope); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/v1/runtime/database/scope" {
			envelope["message_type"], envelope["payload"] = "database_result", map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(envelope)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
	native, err := rootless.New(rootless.Config{
		RunscPath: filepath.Join(source, ".development/runtime/gvisor/bin/runsc"),
		RootFS:    filepath.Join(imageRoot, "rootfs"),
		StateRoot: filepath.Join(root, "sandboxes"), RuntimeRoot: filepath.Join(root, "runsc"),
		InstanceUUID: "nod-0123456789", KernelSocketPath: "/run/the8020/kernel.sock",
		SupervisorHeartbeatInterval: time.Second, WorkerStopGrace: time.Second, StartTimeout: 30 * time.Second, Logger: logs.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = native.Close() })
	live, err := state.New(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	archive, err := history.New(history.Config{Root: filepath.Join(root, "history")})
	if err != nil {
		t.Fatal(err)
	}
	network, err := sandboxnetwork.NewLoopback(filepath.Join(root, "network"))
	if err != nil {
		t.Fatal(err)
	}
	leases, err := ports.New(filepath.Join(root, "ports"), false, logs.Logger())
	if err != nil {
		t.Fatal(err)
	}
	client, err := supervisor.New(supervisor.Config{ProtocolVersion: protocol.ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	sandboxes, err := sandboxmanager.New(sandboxmanager.Config{
		InstanceUUID: "nod-0123456789", Store: live, Backend: native, Network: network, Supervisor: client,
		Ports: leases, Logs: logs, History: archive, StartupTimeout: 30 * time.Second, KeepAlive: time.Minute, StopGrace: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := sandboxes.Shutdown(ctx, sandboxmanager.ShutdownDestroy); err != nil {
			t.Error(err)
		}
	})
	workerManager, err := workers.New(sandboxes, client, 0, 64, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	groups, err := coordinator.New(sandboxes, 64)
	if err != nil {
		t.Fatal(err)
	}
	mountPolicy, err := mounts.NewPolicy([]string{m.config.Root}, filepath.Join(m.config.Root, "node/kernel"), "", true)
	if err != nil {
		t.Fatal(err)
	}
	profile := model.RuntimeProfile{
		WorkloadType: model.WorkloadJob, ImageDigest: imageDigest, DependencyMode: model.DependencyOnline,
		Permissions: model.Permissions{ReadPaths: []string{"/opt/runtime", "/workspace/packages", "/tmp", "/runtime-cache"}, WritePaths: []string{"/tmp", "/runtime-cache"}},
		Mounts: []model.Mount{
			{Source: m.config.PackagesRoot, Target: "/workspace/packages", ReadOnly: true, Purpose: "workspace-packages", Persistence: "shared"},
			{Source: api, Target: "/run/the8020", ReadOnly: true, Purpose: "kernel-api", Persistence: "kernel"},
			{Target: "/tmp", MaximumSize: 64 << 20, Purpose: "temporary", Persistence: "ephemeral"},
			{Target: "/runtime-cache", MaximumSize: 64 << 20, Purpose: "temporary", Persistence: "ephemeral"},
		}, NetworkMode: "netstack", EgressAllowed: true, ResourceClass: "job:analysis",
	}
	for _, part := range []string{"npm", "remote", "gen"} {
		cache := filepath.Join(root, "cache", part)
		if err := os.MkdirAll(cache, 0755); err != nil {
			t.Fatal(err)
		}
		profile.Mounts = append(profile.Mounts, model.Mount{Source: cache, Target: "/runtime-cache/" + part, Purpose: "runtime-cache", Persistence: "node"})
	}
	runner, err := jobs.New(groups, workerManager, jobs.Policy{
		NodeID: "nod-0123456789", LogPosition: logs.ReadPosition, Profile: profile, WorkspaceMounts: mountPolicy,
		Resources: model.ResourceLimits{PIDMaximum: 128, TmpfsMaximum: 128 << 20},
		Lifecycle: model.LifecyclePolicy{DestroyWhenIdle: true, StopGracePeriod: time.Second}, ExecutionTimeout: 2 * time.Minute, Logger: logs.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Close() })
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		page, err := logs.Query(ctx, records.Query{Limit: 100, Tail: true})
		if err != nil {
			t.Logf("schema runtime diagnostics: %v", err)
			return
		}
		for _, record := range page.Records {
			t.Logf("%s %s %s", record.Level, record.SandboxID, record.Message)
		}
	})
	return runner, image
}

func TestWorkflowAnalysisSchema(t *testing.T) {
	m, sparse, sandbox, shared := analysisSparseRuntime(t, "schema", 4)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	source, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	// Stage current package source only in the disposable instance.
	stagedHashes := map[string]string{}
	stagedTrees := map[string]string{}
	for _, name := range []string{"db", "packages", "secrets", "system"} {
		from, to := filepath.Join(source, name), filepath.Join(m.config.PackagesRoot, "the8020", name)
		stagedHashes[name] = analysisStagePackage(t, from, to)
		if name == "packages" {
			patch := exec.Command("git", "apply", filepath.Join(source, "kernel/kernel/development/analysis/activation-stage.patch"))
			patch.Dir = to
			if output, err := patch.CombinedOutput(); err != nil {
				t.Fatalf("patch disposable activation stage definition: %v: %s", err, output)
			}
		}
		initializeTestRepository(t, m, "the8020/"+name, "Fixture", "fixture@example.test", "Schema fixture sources")
		tree, err := gitOutput(to, "rev-parse", "HEAD^{tree}")
		if err != nil {
			t.Fatal(err)
		}
		stagedTrees[name] = tree
	}
	const tableBefore = `import {t, table} from "/p/the8020/db/mod.ts";
export default table("the8020__dev_core__labels", {id: t.text().primaryKey(), label: t.text().default("before")});
`
	writeTestFile(t, filepath.Join(shared, "tables/labels.ts"), tableBefore)
	writeTestFile(t, filepath.Join(shared, "programs/workflow-read/program.toml"), "schema = 1\ndescription = \"Published availability probe\"\nentrypoint = \"main.ts\"\n")
	writeTestFile(t, filepath.Join(shared, "programs/workflow-read/main.ts"), "export default () => 'available';\n")
	if _, err := gitCommand(ctx, shared, nil, "add", "tables", "programs/workflow-read"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-m", "Initial table"); err != nil {
		t.Fatal(err)
	}
	if err := sparse.Kill(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err := sparse.Delete(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	sandbox, err = m.Start(ctx, sandbox.UserID)
	if err != nil {
		t.Fatal(err)
	}
	runner, image := analysisSchemaJobs(t, m)
	db := database.New(database.Config{Backend: database.BackendSQLite, Location: filepath.Join(m.config.Root, "database/system.db"), InstanceRoot: m.config.Root, MaximumOpenConnections: 8, MaximumIdleConnections: 2})
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
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
	follower, err := workspacepackages.NewPackageRevisionFollower(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	activation, err := workspacepackages.NewActivationCoordinator(workspacepackages.ActivationCoordinatorConfig{Database: db, Schema: evaluation, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	commits, err := evaluation.PackageSet(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := activation.Bootstrap(ctx, commits); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteInitialization(ctx, commits); err != nil {
		t.Fatal(err)
	}
	if err := evaluation.UseActivatedPackages(store); err != nil {
		t.Fatal(err)
	}
	bootstrapUpdate, err := follower.Poll(ctx)
	if err != nil || len(bootstrapUpdate.Packages) != len(commits) {
		t.Fatalf("follower missed bootstrap publication: %+v: %v", bootstrapUpdate, err)
	}
	if err := follower.Acknowledge(bootstrapUpdate.Revision); err != nil {
		t.Fatal(err)
	}
	var preparationFailure string
	interruptCompletion := true
	m.SetSchemaDeployment(analysisActivationHook{
		prepare: func(ctx context.Context, id string, candidates []deployment.Candidate) error {
			err := activation.Prepare(ctx, id, candidates)
			if err != nil {
				preparationFailure = err.Error()
				t.Logf("actual schema/hook preparation: %v", err)
			}
			return err
		},
		complete: func(ctx context.Context, id string, activated bool) error {
			err := activation.Complete(ctx, id, activated)
			if err != nil {
				t.Logf("actual schema/hook completion (%t): %v", activated, err)
			}
			if err == nil && activated && interruptCompletion {
				interruptCompletion = false
				return errors.New("injected interruption after database completion, before private-file acknowledgement")
			}
			return err
		},
	})
	if _, err := db.ExecContext(ctx, `INSERT INTO "the8020__dev_core__labels" ("id", "label") VALUES ('keep', 'existing data')`); err != nil {
		t.Fatal(err)
	}
	prefix := "set -e; cd /workspace/packages/the8020/dev-core; "
	changed := strings.Replace(tableBefore, `t.text().default("before")`, `t.text().default("after"), extra: t.text().nullable()`, 1)
	analysisExec(t, sparse.RunscDriver, sandbox, prefix+"printf %s "+shellQuote(changed)+" >tables/labels.ts")
	for _, phase := range []string{"pre", "post"} {
		program := "probe-" + phase
		manifest := "schema = 1\ndescription = \"Schema activation probe\"\nentrypoint = \"main.ts\"\ndiscoverable = false\n"
		hook := "hook = \"" + phase + "-activate\"\ndescription = \"Verify candidate table\"\nprogram = \"the8020/dev-core/" + program + "\"\n"
		code := `import labels from "../../tables/labels.ts";
import {descriptorOf} from "/p/the8020/db/mod.ts";
export default function () { if (!descriptorOf(labels).columns.some(c => c.name === "extra")) throw new Error("hook saw old table source"); }
`
		for name, body := range map[string]string{"programs/" + program + "/program.toml": manifest, "programs/" + program + "/main.ts": code, "hooks/" + phase + ".toml": hook} {
			analysisExec(t, sparse.RunscDriver, sandbox, prefix+"mkdir -p "+shellQuote(filepath.Dir(name))+"; printf %s "+shellQuote(body)+" >"+shellQuote(name))
		}
	}
	before, err := gitOutput(shared, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	analysisExec(t, sparse.RunscDriver, sandbox, "printf alive > /tmp/schema-generation")
	started := time.Now()
	output := analysisExec(t, sparse.RunscDriver, sandbox, "activate --json --message 'Rejected default change'; status=$?; test \"$status\" = 3")
	rejectionElapsed := time.Since(started)
	var rejected ActivationResult
	if err := json.Unmarshal([]byte(output), &rejected); err != nil || rejected.Success || !strings.Contains(preparationFailure, "changing column label requires a migration") {
		t.Fatalf("expected real incompatible-schema rejection: %+v; %v; %s", rejected, err, preparationFailure)
	}
	if head, err := gitOutput(shared, "rev-parse", "HEAD"); err != nil || head != before {
		t.Fatalf("rejected schema published source: %s: %v", head, err)
	}
	if pending, err := activation.Pending(ctx); err != nil || pending {
		t.Fatalf("rejected schema left unfinished coordinator state: %t: %v", pending, err)
	}
	if got := analysisExec(t, sparse.RunscDriver, sandbox, prefix+"cat tables/labels.ts"); got != changed {
		t.Fatalf("schema rejection lost the private edit: %q", got)
	}
	var attempt analysisActivationAttempt
	if err := readJSON(filepath.Join(m.sandboxRoot(sandbox), "activation/active.json"), &attempt); err != nil || len(attempt.Packages) != 1 {
		t.Fatalf("schema rejection lost its native attempt: %+v: %v", attempt, err)
	}
	// Fix the retained candidate with ordinary Git. The additive column is
	// supported; changing an existing default requires a migration and is not.
	fixed := strings.Replace(changed, `default("after")`, `default("before")`, 1)
	analysisExec(t, sparse.RunscDriver, sandbox, "set -e; cd "+shellQuote(attempt.Packages[0].Worktree)+"; printf %s "+shellQuote(fixed)+" >tables/labels.ts; git add tables/labels.ts; git -c user.name=Fixture -c user.email=fixture@example.test commit -m 'Keep original default'")
	started = time.Now()
	output = analysisExec(t, sparse.RunscDriver, sandbox, "activate --json --message 'Real schema and hooks'; status=$?; test \"$status\" = 3")
	publicationElapsed := time.Since(started)
	var interrupted ActivationResult
	if err := json.Unmarshal([]byte(output), &interrupted); err != nil || interrupted.Success || interrupted.Status != "committed-finalization-failed" || interruptCompletion {
		t.Fatalf("expected interruption after real schema/hooks: %+v: %v", interrupted, err)
	}
	if err := readJSON(filepath.Join(m.sandboxRoot(sandbox), "activation/active.json"), &attempt); err != nil || attempt.Phase != "published" {
		t.Fatalf("interruption lost publication checkpoint: %+v: %v", attempt, err)
	}
	// Recreate the coordination owners from their durable database records,
	// leaving the development runtime and its saved native Git attempt alive.
	restoredEvaluation, err := evaluator.New(evaluator.Config{Packages: catalog, Jobs: runner, Database: db})
	if err != nil {
		t.Fatal(err)
	}
	if err := restoredEvaluation.UseActivatedPackages(store); err != nil {
		t.Fatal(err)
	}
	activation, err = workspacepackages.NewActivationCoordinator(workspacepackages.ActivationCoordinatorConfig{Database: db, Schema: restoredEvaluation, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	m.SetSchemaDeployment(activation)
	// A different attempt starts before the first helper's finalization retry.
	// Its schema/pre hook run, but its source has not switched. The retry must
	// neither publish nor roll back this other transaction.
	secondID, err := identity.New("act")
	if err != nil {
		t.Fatal(err)
	}
	secondCommit, err := gitOutputContext(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "Second pending activation")
	if err != nil {
		t.Fatal(err)
	}
	if err := activation.Prepare(ctx, secondID, []deployment.Candidate{{PackageID: "the8020/dev-core", Root: shared, Commit: secondCommit}}); err != nil {
		t.Fatal(err)
	}
	update, err := follower.Poll(ctx)
	if err != nil || !slices.Equal(update.Packages, []string{"the8020/dev-core"}) ||
		!slices.Contains(update.Paths, "/workspace/packages/the8020/dev-core/tables/labels.ts") ||
		slices.Contains(update.Paths, "/workspace/packages/the8020/dev-core/") {
		t.Fatalf("preparation hid the previous published source update: %+v: %v", update, err)
	}
	if err := follower.Acknowledge(update.Revision); err != nil {
		t.Fatal(err)
	}
	programs, err := programrunner.New(store, runner)
	if err != nil {
		t.Fatal(err)
	}
	read, err := programs.Run(ctx, "the8020/dev-core/workflow-read", attempt.Packages[0].Published, nil, nil)
	if err != nil || read.Value != "available" || read.WorkerID == "" {
		t.Fatalf("prepared activation hid the published native program: %+v: %v", read, err)
	}
	started = time.Now()
	output = analysisExec(t, sparse.RunscDriver, sandbox, "activate --json --message 'Real schema and hooks'")
	retryElapsed := time.Since(started)
	var result ActivationResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.OverlayReset {
		t.Fatalf("real schema activation: %+v", result)
	}
	var stage, stillActive string
	if err := db.QueryRowContext(ctx, `SELECT "stage" FROM "the8020__packages__activations" WHERE "activationId" = $1`, secondID).Scan(&stage); err != nil || stage != "pre_activated" {
		t.Fatalf("first helper retry finalized a different activation: %s: %v", stage, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT "activeCommit" FROM "the8020__packages__packages" WHERE "packageId" = 'the8020/dev-core'`).Scan(&stillActive); err != nil || stillActive != attempt.Packages[0].Published {
		t.Fatalf("first helper retry published the unswitched second commit: %s: %v", stillActive, err)
	}
	if pending, exists, err := db.PendingDeployment(ctx); err != nil || !exists || pending.ID != secondID {
		t.Fatalf("first helper retry consumed another schema deployment: %+v: %t: %v", pending, exists, err)
	}
	if err := activation.Complete(ctx, secondID, false); err != nil {
		t.Fatal(err)
	}
	if update, err := follower.Poll(ctx); err != nil || update.Revision != 0 {
		t.Fatalf("rollback changed published identity: %+v: %v", update, err)
	}
	if got := analysisExec(t, sparse.RunscDriver, sandbox, "cat /tmp/schema-generation"); got != "alive" {
		t.Fatalf("schema activation replaced the development runtime: %q", got)
	}
	if got := analysisExec(t, sparse.RunscDriver, sandbox, prefix+"cat tables/labels.ts"); got != fixed {
		t.Fatalf("completed activation did not expose the corrected source: %q", got)
	}
	if _, err := os.Stat(filepath.Join(m.sandboxRoot(sandbox), "activation/active.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed activation retained its pending attempt: %v", err)
	}
	var label string
	if err := db.QueryRowContext(ctx, `SELECT "label" FROM "the8020__dev_core__labels" WHERE "id" = 'keep'`).Scan(&label); err != nil || label != "existing data" {
		t.Fatalf("row lost: %q: %v", label, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE "the8020__dev_core__labels" SET "extra" = 'new column' WHERE "id" = 'keep'`); err != nil {
		t.Fatal(err)
	}
	var hooks int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "the8020__packages__hook_runs" WHERE "activationId" = $1 AND "packageId" = 'the8020/dev-core' AND "state" = 'succeeded' AND "attempts" = 1`, attempt.TransactionID).Scan(&hooks); err != nil || hooks != 2 {
		t.Fatalf("native hook results: %d: %v", hooks, err)
	}
	var attempts int
	if err := db.QueryRowContext(ctx, `SELECT SUM("attempts") FROM "the8020__packages__hook_runs" WHERE "activationId" = $1 AND "packageId" = 'the8020/dev-core'`, attempt.TransactionID).Scan(&attempts); err != nil || attempts != 2 {
		t.Fatalf("retry repeated successful hooks: %d: %v", attempts, err)
	}
	if pending, err := activation.Pending(ctx); err != nil || pending {
		t.Fatalf("unfinished coordinator state: %t: %v", pending, err)
	}
	if _, pending, err := db.PendingDeployment(ctx); err != nil || pending {
		t.Fatalf("unfinished schema deployment: %t: %v", pending, err)
	}
	active, err := store.ActivatedPackageCommit(ctx, "the8020/dev-core")
	if err != nil || active != result.Packages[0].ResultingHead {
		t.Fatalf("active catalog mismatch: %q: %v", active, err)
	}
	record := map[string]any{"full_workflow_qualified": false, "real_schema_and_hooks_passed": true, "host_power_loss_qualified": false,
		"publication_before_ack_interrupt_ms": float64(publicationElapsed.Microseconds()) / 1000, "retry_helper_ms": float64(retryElapsed.Microseconds()) / 1000,
		"observed_at": time.Now().UTC(), "successful_hooks": hooks, "hook_attempts": attempts, "service_image": image, "input_source_sha256": stagedHashes, "staged_source_git_trees": stagedTrees,
		"incompatible_schema_rejection_ms": float64(rejectionElapsed.Microseconds()) / 1000, "incompatible_schema_error": preparationFailure,
		"schema_rejection_preserves_private_and_shared": true, "ordinary_git_candidate_fix_and_retry": true,
		"retry_after_database_completion_with_recreated_coordinators": true, "successful_hooks_not_repeated": true,
		"retry_does_not_complete_another_prepared_activation": true, "hook_counts_for_activation_id": attempt.TransactionID,
		"published_revision_following_during_preparation": true, "bootstrap_publication_observed": true,
		"published_program_runs_during_preparation": true,
		"retained_row": label, "result": result, "callback_boundary": "fixture registration/heartbeat/database-scope acknowledgements; real job, Worker, Deno evaluator, SQL and package coordinator"}
	record["environment"] = map[string]string{"os": runtime.GOOS, "architecture": runtime.GOARCH, "go": runtime.Version(), "gofer_sdk": os.Getenv("WORKFLOW_SPARSE_SDK")}
	record["gofer_sdk_fix"] = json.RawMessage(os.Getenv("WORKFLOW_SPARSE_SDK_FIX"))
	// Fail the real handler refresh after source, schema and activation hooks
	// publish. Recreate the owners and retry only that unfinished finalization.
	indexCalls := 0
	indexTimes := []float64{}
	var indexAttempt analysisActivationAttempt
	var publishedRevision int64
	analysisExec(t, sparse.RunscDriver, sandbox, prefix+"printf captured >index-refresh.txt")
	for try := 0; try < 2; try++ {
		evaluation, err := evaluator.New(evaluator.Config{Packages: catalog, Jobs: runner, Database: db})
		if err != nil {
			t.Fatal(err)
		}
		if err := evaluation.UseActivatedPackages(store); err != nil {
			t.Fatal(err)
		}
		owner, err := workspacepackages.NewActivationCoordinator(workspacepackages.ActivationCoordinatorConfig{
			Database: db, Schema: evaluation, Packages: store, Jobs: runner,
			Reindex: func(ctx context.Context, ids []string) error {
				indexCalls++
				if indexCalls == 1 {
					return errors.New("injected handler index refresh failure")
				}
				_, err := store.ReindexHandlers(ctx, ids...)
				return err
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		m.SetSchemaDeployment(owner)
		command := "activate --json --message 'Index completion retry'"
		if try == 0 {
			command += "; status=$?; test \"$status\" = 3"
		}
		started := time.Now()
		output := analysisExec(t, sparse.RunscDriver, sandbox, command)
		indexTimes = append(indexTimes, float64(time.Since(started).Microseconds())/1000)
		var result ActivationResult
		if err := json.Unmarshal([]byte(output), &result); err != nil || result.Success != (try == 1) || result.OverlayReset {
			t.Fatalf("index completion helper: %+v: %v", result, err)
		}
		if try == 0 {
			if err := readJSON(filepath.Join(m.sandboxRoot(sandbox), "activation/active.json"), &indexAttempt); err != nil || indexAttempt.Phase != "published" {
				t.Fatalf("index failure lost native publication: %+v: %v", indexAttempt, err)
			}
			if pending, err := owner.Pending(ctx); err != nil || !pending {
				t.Fatalf("index failure became terminal: pending=%t: %v", pending, err)
			}
			if err := db.QueryRowContext(ctx, `SELECT "stage" FROM "the8020__packages__activations" WHERE "activationId" = $1`, indexAttempt.TransactionID).Scan(&stage); err != nil || stage != "published" {
				t.Fatalf("index failure lost durable stage: %s: %v", stage, err)
			}
			if err := db.QueryRowContext(ctx, `SELECT "revision" FROM "the8020__system__revisions" WHERE "domain" = 'packages'`).Scan(&publishedRevision); err != nil {
				t.Fatal(err)
			}
			analysisExec(t, sparse.RunscDriver, sandbox, prefix+"printf later >index-refresh.txt")
		} else {
			var revision int64
			if err := db.QueryRowContext(ctx, `SELECT "revision" FROM "the8020__system__revisions" WHERE "domain" = 'packages'`).Scan(&revision); err != nil || revision != publishedRevision {
				t.Fatalf("index retry republished source: %d -> %d: %v", publishedRevision, revision, err)
			}
			if pending, err := owner.Pending(ctx); err != nil || pending || indexCalls != 2 {
				t.Fatalf("index retry did not finish: pending=%t calls=%d: %v", pending, indexCalls, err)
			}
			if err := db.QueryRowContext(ctx, `SELECT SUM("attempts") FROM "the8020__packages__hook_runs" WHERE "activationId" = $1`, indexAttempt.TransactionID).Scan(&attempts); err != nil || attempts != 2 {
				t.Fatalf("index retry repeated activation hooks: attempts=%d: %v", attempts, err)
			}
			if len(store.PackageHooks("the8020/dev-core", "post-activate")) != 1 {
				t.Fatal("index retry did not publish handlers")
			}
			if body, err := os.ReadFile(filepath.Join(shared, "index-refresh.txt")); err != nil || string(body) != "captured" {
				t.Fatalf("index retry changed the published capture: %q: %v", body, err)
			}
			if got := analysisExec(t, sparse.RunscDriver, sandbox, prefix+"cat index-refresh.txt; cat /tmp/schema-generation"); got != "lateralive" {
				t.Fatalf("index retry lost later edits or the live process: %q", got)
			}
		}
	}
	record["index_completion_retry"] = map[string]any{"passed": true, "failed_helper_ms": indexTimes[0], "retry_helper_ms": indexTimes[1], "hook_attempts": attempts, "index_calls": indexCalls}
	record["activation_recovery"] = analysisActivationRecovery(t, m, sparse, sandbox, db, func() {
		evaluation, err := evaluator.New(evaluator.Config{Packages: catalog, Jobs: runner, Database: db})
		if err != nil {
			t.Fatal(err)
		}
		if err := evaluation.UseActivatedPackages(store); err != nil {
			t.Fatal(err)
		}
		coordinator, err := workspacepackages.NewActivationCoordinator(workspacepackages.ActivationCoordinatorConfig{Database: db, Schema: evaluation, Packages: store, Jobs: runner})
		if err != nil {
			t.Fatal(err)
		}
		m.SetSchemaDeployment(coordinator)
	})
	// Multiple durable attempts can exist at startup. One live source owner
	// must not keep other packages' abandoned schema preparations pending.
	batchEvaluation, err := evaluator.New(evaluator.Config{Packages: catalog, Jobs: runner, Database: db})
	if err != nil {
		t.Fatal(err)
	}
	if err := batchEvaluation.UseActivatedPackages(store); err != nil {
		t.Fatal(err)
	}
	batchConfig := workspacepackages.ActivationCoordinatorConfig{Database: db, Schema: batchEvaluation, Packages: store, Jobs: runner}
	batchOwner, err := workspacepackages.NewActivationCoordinator(batchConfig)
	if err != nil {
		t.Fatal(err)
	}
	releaseSource, err := workspacepackages.LockSources(ctx, m.config.PackagesRoot, []string{"the8020/dev-core"})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSource()
	batchIDs := []string{}
	for _, name := range []string{"dev-core", "packages", "system"} {
		id, err := identity.New("act")
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(m.config.PackagesRoot, "the8020", name)
		commit, err := gitOutputContext(ctx, path, gitIdentity("Fixture", "fixture@example.test"), "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "Unpublished recovery candidate")
		if err != nil {
			t.Fatal(err)
		}
		if err := batchOwner.Prepare(ctx, id, []deployment.Candidate{{PackageID: "the8020/" + name, Root: path, Commit: commit}}); err != nil {
			t.Fatal(err)
		}
		batchIDs = append(batchIDs, id)
	}
	started = time.Now()
	if err := batchOwner.Recover(ctx); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("batch recovery did not retain the live source owner: %v", err)
	}
	batchElapsed := time.Since(started)
	for i, id := range batchIDs {
		want := "failed"
		if i == 0 {
			want = "pre_activated"
		}
		if err := db.QueryRowContext(ctx, `SELECT "stage" FROM "the8020__packages__activations" WHERE "activationId" = $1`, id).Scan(&stage); err != nil || stage != want {
			t.Fatalf("native batch recovery: id=%s stage=%s want=%s: %v", id, stage, want, err)
		}
		if _, exists, err := db.PendingDeploymentFor(ctx, id); err != nil || exists != (i == 0) {
			t.Fatalf("native batch schema recovery: id=%s exists=%t: %v", id, exists, err)
		}
	}
	releaseSource()
	batchOwner, err = workspacepackages.NewActivationCoordinator(batchConfig)
	if err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	if err := batchOwner.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	releasedElapsed := time.Since(started)
	if pending, err := batchOwner.Pending(ctx); err != nil || pending {
		t.Fatalf("released native owner remains pending: %t: %v", pending, err)
	}
	if _, exists, err := db.PendingDeployment(ctx); err != nil || exists {
		t.Fatalf("batch recovery left schema records: exists=%t: %v", exists, err)
	}
	record["batch_recovery"] = map[string]any{"passed": true, "abandoned_attempts_recovered": 2, "live_attempt_preserved": true, "recovered_after_owner_release": true,
		"recovery_ms": float64(batchElapsed.Microseconds()) / 1000, "released_owner_recovery_ms": float64(releasedElapsed.Microseconds()) / 1000,
		"timing_boundary": "coordinator recovery with real native schema rollback; excludes preparation and helper transport"}
	m.SetSchemaDeployment(batchOwner)
	newID := "the8020/workflow-created"
	newRoot := filepath.Join(m.config.PackagesRoot, newID)
	newTable := `import {table, t} from "/p/the8020/db/mod.ts";
export default table("the8020__workflow_created__labels", {id: t.text().primaryKey(), label: t.text()});
`
	// Ordinary files alone create a package; no manual Git/catalog setup.
	for name, body := range map[string]string{
		"package.toml":                "schema = 1\n",
		"programs/hello/program.toml": "schema = 1\ndescription = \"Created package program\"\nentrypoint = \"main.ts\"\n",
		"programs/hello/main.ts":      "export default () => 'created package';\n",
		"tables/labels.ts":            newTable,
	} {
		analysisExec(t, sparse.RunscDriver, sandbox, "mkdir -p "+shellQuote(path.Dir("/workspace/packages/"+newID+"/"+name))+"; printf %s "+shellQuote(body)+" >"+shellQuote("/workspace/packages/"+newID+"/"+name))
	}
	runLifecycle := func() ActivationResult {
		t.Helper()
		output := analysisExec(t, sparse.RunscDriver, sandbox, "activate --json --message 'Package lifecycle prototype'")
		var result ActivationResult
		if err := json.Unmarshal([]byte(output), &result); err != nil || !result.Success {
			t.Fatalf("lifecycle activation: %s: %v", output, err)
		}
		return result
	}
	runLifecycle()
	newCommit, err := store.ActivatedPackageCommit(ctx, newID)
	if err != nil || newCommit == "" {
		t.Fatalf("new package not registered: %q: %v", newCommit, err)
	}
	if read, err := programs.Run(ctx, newID+"/hello", newCommit, nil, nil); err != nil || read.Value != "created package" {
		t.Fatalf("new package program: %+v: %v", read, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO "the8020__workflow_created__labels" ("id", "label") VALUES ('keep', 'retained after removal')`); err != nil {
		t.Fatal(err)
	}
	analysisExec(t, sparse.RunscDriver, sandbox, "rm -r /workspace/packages/"+newID)
	runLifecycle()
	if _, err := os.Stat(newRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed package source: %v", err)
	}
	if active, err := store.ActivatedPackageCommit(ctx, newID); err == nil && active != "" {
		t.Fatalf("removed package still active: %s", active)
	}
	if _, err := programs.Run(ctx, newID+"/hello", newCommit, nil, nil); err == nil {
		t.Fatal("removed package program still runs")
	}
	if err := db.QueryRowContext(ctx, `SELECT "label" FROM "the8020__workflow_created__labels" WHERE "id" = 'keep'`).Scan(&label); err != nil || label != "retained after removal" {
		t.Fatalf("removal lost physical data: %q: %v", label, err)
	}
	analysisExec(t, sparse.RunscDriver, sandbox, "mkdir -p /workspace/packages/"+newID+"; printf 'schema = 1\\n' >/workspace/packages/"+newID+"/package.toml")
	runLifecycle()
	if active, err := store.ActivatedPackageCommit(ctx, newID); err != nil || active == "" {
		t.Fatalf("recreated package not active: %q: %v", active, err)
	}
	record["ordinary_package_create_delete_recreate_catalog_program_and_retained_data"] = true
	renamedID := "the8020/workflow-renamed"
	beforeRename, err := gitOutput(newRoot, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	analysisExec(t, sparse.RunscDriver, sandbox, "set -e; mv /workspace/packages/"+newID+" /workspace/packages/"+renamedID+"; mkdir -p /workspace/packages/"+renamedID+"/programs/hello; printf 'schema = 1\\ndescription = \"Renamed package program\"\\nentrypoint = \"main.ts\"\\n' >/workspace/packages/"+renamedID+"/programs/hello/program.toml; printf %s "+shellQuote("export default () => 'renamed package';\n")+" >/workspace/packages/"+renamedID+"/programs/hello/main.ts")
	if result := runLifecycle(); len(result.Packages) != 2 {
		t.Fatalf("rename did not publish old and new package together: %+v", result)
	}
	if active, err := store.ActivatedPackageCommit(ctx, newID); err == nil && active != "" {
		t.Fatalf("renamed source package still active: %s", active)
	}
	renamedCommit, err := store.ActivatedPackageCommit(ctx, renamedID)
	if err != nil || renamedCommit == "" {
		t.Fatalf("renamed package not registered: %q: %v", renamedCommit, err)
	}
	if read, err := programs.Run(ctx, renamedID+"/hello", renamedCommit, nil, nil); err != nil || read.Value != "renamed package" {
		t.Fatalf("renamed package program: %+v: %v", read, err)
	}
	if _, err := gitCommand(ctx, filepath.Join(m.config.PackagesRoot, renamedID), nil, "merge-base", "--is-ancestor", beforeRename, "HEAD"); err != nil {
		t.Fatalf("rename lost published Git history: %v", err)
	}
	record["package_rename_catalog_program_index_and_history"] = true
	hashes := map[string]string{}
	for _, name := range []string{"run.py", "runtime_test.go", "sparse_test.go", "sparse_activation_test.go", "sparse_schema_test.go", "gofer_probe.go", "gofer_probe.py", "activation-transaction.patch", "activation-stage.patch", "../model.go", "../manager.go", "../activation.go"} {
		body, err := os.ReadFile(filepath.Join("analysis", name))
		if err != nil {
			t.Fatal(err)
		}
		hashes[name] = fmt.Sprintf("%x", sha256.Sum256(body))
	}
	record["source_sha256"] = hashes
	guidanceTree, err := gitOutput(filepath.Join(m.config.PackagesRoot, "the8020/dev-skills"), "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	record["guidance_source_git_tree"] = guidanceTree
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("analysis/sparse-schema-results.json", append(body, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("PASS actual native evaluator/SQL/package coordinator/pre-post hooks through sparse activation; crash/concurrency gates remain open")
}

func analysisActivationRecovery(t *testing.T, m *Manager, d *analysisSparseDriver, sandbox Sandbox, db *database.Manager, reload func()) map[string]any {
	t.Helper()
	ctx := context.Background()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	fault := filepath.Join(bin, "fault")
	second := filepath.Join(m.config.PackagesRoot, "the8020/secrets")
	wrapper := "#!/bin/sh\nset -eu\nif [ \"${2-}\" = " + shellQuote(second) + " ] && [ \"${3-}\" = reset ] && [ \"${4-}\" = --hard ] && [ -f " + shellQuote(fault) + " ]; then\n" +
		"read -r when < " + shellQuote(fault) + "\nrm " + shellQuote(fault) + "\nif [ \"$when\" = after ]; then " + shellQuote(git) + " \"$@\"; fi\nexit 77\nfi\nexec " + shellQuote(git) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	results := map[string]any{}
	for _, when := range []string{"before-prepare", "after-prepare", "before", "after"} {
		preparation := strings.HasSuffix(when, "-prepare")
		t.Logf("activation recovery scenario: %s", when)
		candidate := "captured recovery " + when + "\n"
		files := map[string]string{"the8020/dev-core": "same.txt", "the8020/secrets": "README.md"}
		for id, name := range files {
			analysisExec(t, d.RunscDriver, sandbox, "printf %s "+shellQuote(candidate)+" >"+shellQuote("/workspace/packages/"+id+"/"+name))
		}
		hook := m.schemaDeployment()
		m.SetSchemaDeployment(analysisActivationHook{prepare: func(ctx context.Context, id string, candidates []deployment.Candidate) error {
			if when != "before-prepare" {
				if err := hook.Prepare(ctx, id, candidates); err != nil {
					t.Logf("recovery preparation: %v", err)
					return err
				}
			}
			if preparation {
				return errors.New("injected lost preparation result")
			}
			return nil
		}, complete: hook.Complete})
		if !preparation {
			if err := os.WriteFile(fault, []byte(when+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		run := func(want int) ActivationResult {
			t.Helper()
			output := analysisExec(t, d.RunscDriver, sandbox, fmt.Sprintf("activate --json --message 'Recovery %s'; status=$?; test \"$status\" = %d", when, want))
			var result ActivationResult
			if err := json.Unmarshal([]byte(output), &result); err != nil {
				t.Fatal(err)
			}
			return result
		}
		started := time.Now()
		failed := run(3)
		failedElapsed := time.Since(started)
		if failed.Success {
			t.Fatal("ignored interrupted native source switch")
		}
		var attempt analysisActivationAttempt
		journal := filepath.Join(m.sandboxRoot(sandbox), "activation/active.json")
		wantPhase := "switching"
		if preparation {
			wantPhase = "preparing"
		}
		if err := readJSON(journal, &attempt); err != nil || attempt.Phase != wantPhase || len(attempt.Packages) != 2 {
			t.Fatalf("%s interruption needs phase %s: %+v: %v", when, wantPhase, attempt, err)
		}
		for _, item := range attempt.Packages {
			want := item.Published
			if preparation || (when == "before" && item.PackageID == "the8020/secrets") {
				want = item.Previous
			}
			if head, err := gitOutput(filepath.Join(m.config.PackagesRoot, item.PackageID), "rev-parse", "HEAD"); err != nil || head != want {
				t.Fatalf("fault missed source boundary %s: %s want %s: %v", item.PackageID, head, want, err)
			}
		}
		later := "later private recovery " + when + "\n"
		analysisExec(t, d.RunscDriver, sandbox, "printf %s "+shellQuote(later)+" >/workspace/packages/the8020/dev-core/same.txt")
		reload()
		if when == "before" {
			sharedFile := filepath.Join(m.config.PackagesRoot, "the8020/dev-core/same.txt")
			writeTestFile(t, sharedFile, "external dirty work\n")
			if refused := run(3); refused.Success {
				t.Fatal("recovery overwrote a dirty shared tree")
			}
			if body, err := os.ReadFile(sharedFile); err != nil || string(body) != "external dirty work\n" {
				t.Fatalf("recovery lost external shared work: %q: %v", body, err)
			}
			writeTestFile(t, sharedFile, candidate)
		}
		started = time.Now()
		completed := run(0)
		recoveredElapsed := time.Since(started)
		if !completed.Success || completed.OverlayReset || len(completed.Packages) != 2 {
			t.Fatalf("source recovery did not complete: %+v", completed)
		}
		for _, item := range completed.Packages {
			var active string
			if err := db.QueryRowContext(ctx, `SELECT "activeCommit" FROM "the8020__packages__packages" WHERE "packageId" = $1`, item.PackageID).Scan(&active); err != nil || active != item.ResultingHead {
				t.Fatalf("recovery catalog differs from source: %s: %v", active, err)
			}
			if body, err := os.ReadFile(filepath.Join(m.config.PackagesRoot, item.PackageID, files[item.PackageID])); err != nil || string(body) != candidate {
				t.Fatalf("recovery published later work: %q: %v", body, err)
			}
		}
		if got := analysisExec(t, d.RunscDriver, sandbox, "cat /workspace/packages/the8020/dev-core/same.txt /tmp/schema-generation"); got != later+"alive" {
			t.Fatalf("recovery lost later work or replaced the runtime: %q", got)
		}
		var stage string
		if err := db.QueryRowContext(ctx, `SELECT COALESCE((SELECT "stage" FROM "the8020__packages__activations" WHERE "activationId" = $1), 'absent')`, attempt.TransactionID).Scan(&stage); err != nil {
			t.Fatal(err)
		}
		wantStage, wantHooks := "failed", 1
		if when == "after" {
			wantStage, wantHooks = "complete", 2
		} else if when == "before-prepare" {
			wantStage, wantHooks = "absent", 0
		}
		var hookCalls int
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM("attempts"), 0) FROM "the8020__packages__hook_runs" WHERE "activationId" = $1`, attempt.TransactionID).Scan(&hookCalls); err != nil || hookCalls != wantHooks || stage != wantStage {
			t.Fatalf("incorrect interrupted transaction outcome: stage=%s hooks=%d: %v", stage, hookCalls, err)
		}
		if _, pending, err := db.PendingDeployment(ctx); err != nil || pending {
			t.Fatalf("recovery left pending schema: %t: %v", pending, err)
		}
		results[when] = map[string]any{"passed": true, "interrupted_transaction_stage": stage, "interrupted_transaction_hook_attempts": hookCalls,
			"failed_helper_ms": float64(failedElapsed.Microseconds()) / 1000, "recovered_helper_ms": float64(recoveredElapsed.Microseconds()) / 1000}
	}
	return results
}
