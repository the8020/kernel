package packages

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"the8020/kernel/database"
	"the8020/kernel/deployment"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/sandbox/model"
)

const (
	activationsTable        = `"the8020__packages__activations"`
	activationPackagesTable = `"the8020__packages__activation_packages"`
	hookRunsTable           = `"the8020__packages__hook_runs"`
)

type ActivationJobRunner interface {
	Run(context.Context, string, string, jobs.Options) (jobs.Record, error)
}

type activationDatabase interface {
	database.Store
	AcquireDeploymentLock(context.Context) (context.Context, func(), error)
}

type ActivationCoordinatorConfig struct {
	Database           activationDatabase
	Schema             deployment.SchemaHook
	Packages           *Store
	Jobs               ActivationJobRunner
	ValidateCandidates func(context.Context, []deployment.Candidate) error
	RefreshCommands    func(context.Context) error
	Now                func() time.Time
}

// ActivationCoordinator is the single package schema/hook publication
// boundary. Source owners call Prepare before their atomic switch and Complete
// afterwards; no package becomes ready until its post hook succeeds.
type ActivationCoordinator struct {
	database           activationDatabase
	schema             deployment.SchemaHook
	packages           *Store
	jobs               ActivationJobRunner
	validateCandidates func(context.Context, []deployment.Candidate) error
	refreshCommands    func(context.Context) error
	now                func() time.Time
	mu                 sync.Mutex
	current            *activationRun
	release            func()
}

type activationRun struct {
	id         string
	candidates []activationCandidate
	failure    error
}

type activationCandidate struct {
	deployment.Candidate
	previous string
	first    bool
}

type ActivationHookContext struct {
	PackageID       string `json:"package_id"`
	PreviousCommit  string `json:"previous_commit,omitempty"`
	CandidateCommit string `json:"candidate_commit"`
	FirstActivation bool   `json:"first_activation"`
	ActivationID    string `json:"activation_id"`
}

func NewActivationCoordinator(config ActivationCoordinatorConfig) (*ActivationCoordinator, error) {
	if config.Database == nil || config.Schema == nil || config.Packages == nil || config.Jobs == nil {
		return nil, errors.New("activation database, schema coordinator, package store, and job runner are required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &ActivationCoordinator{
		database: config.Database, schema: config.Schema, packages: config.Packages,
		jobs: config.Jobs, validateCandidates: config.ValidateCandidates,
		refreshCommands: config.RefreshCommands, now: config.Now,
	}, nil
}

func (c *ActivationCoordinator) Prepare(ctx context.Context, candidates []deployment.Candidate) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current != nil {
		return errors.New("package activation is already in progress")
	}
	if c.validateCandidates != nil {
		if err := c.validateCandidates(ctx, candidates); err != nil {
			return fmt.Errorf("validate package commands: %w", err)
		}
	}
	lockedContext, release, err := c.database.AcquireDeploymentLock(ctx)
	if err != nil {
		return err
	}
	keepLock := false
	defer func() {
		if !keepLock {
			release()
		}
	}()
	run, err := c.begin(lockedContext, candidates)
	if err != nil {
		return err
	}
	c.current = run
	if err := c.schema.Prepare(lockedContext, candidates); err != nil {
		_ = c.rollback(context.WithoutCancel(lockedContext), run, err)
		c.current = nil
		return err
	}
	if err := c.setStage(lockedContext, run.id, "schema_synchronized", nil); err != nil {
		_ = c.rollback(context.WithoutCancel(lockedContext), run, err)
		c.current = nil
		return err
	}
	if err := c.runHooks(lockedContext, run, "pre-activate", true); err != nil {
		_ = c.rollback(context.WithoutCancel(lockedContext), run, err)
		c.current = nil
		return err
	}
	if err := c.setStage(lockedContext, run.id, "pre_activated", nil); err != nil {
		_ = c.rollback(context.WithoutCancel(lockedContext), run, err)
		c.current = nil
		return err
	}
	for _, candidate := range run.candidates {
		if err := c.packages.index.SetActivation(lockedContext, candidate.PackageID, "activating", "", nil); err != nil {
			_ = c.rollback(context.WithoutCancel(lockedContext), run, err)
			c.current = nil
			return err
		}
	}
	c.release = release
	keepLock = true
	return nil
}

func (c *ActivationCoordinator) Complete(ctx context.Context, activated bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current == nil {
		run, err := c.loadPending(ctx)
		if err != nil || run == nil {
			return err
		}
		c.current = run
	}
	run := c.current
	defer func() {
		c.current = nil
		if c.release != nil {
			c.release()
			c.release = nil
		}
	}()
	if !activated {
		cause := run.failure
		if cause == nil {
			cause = errors.New("package source activation did not complete")
		}
		err := c.rollback(ctx, run, cause)
		return err
	}
	if err := c.setStage(ctx, run.id, "code_switched", nil); err != nil {
		return err
	}
	if err := c.runHooks(ctx, run, "post-activate", false); err != nil {
		c.incomplete(ctx, run, err)
		return err
	}
	if err := c.setStage(ctx, run.id, "post_activated", nil); err != nil {
		return err
	}
	for _, candidate := range run.candidates {
		if _, err := c.packages.SynchronizePackageDefinitions(ctx, candidate.PackageID, candidate.Commit); err != nil {
			c.incomplete(ctx, run, err)
			return err
		}
	}
	if err := c.schema.Complete(ctx, true); err != nil {
		c.incomplete(ctx, run, err)
		return err
	}
	if err := c.publish(ctx, run); err != nil {
		c.incomplete(ctx, run, err)
		return err
	}
	if c.refreshCommands != nil {
		if err := c.refreshCommands(ctx); err != nil {
			return fmt.Errorf("refresh package commands: %w", err)
		}
	}
	return nil
}

// Bootstrap runs both hooks for the source set already staged by the
// installer, then publishes its package and service records. Schema tables
// have already been synchronized, so there is no second schema pass.
func (c *ActivationCoordinator) Bootstrap(ctx context.Context, commits map[string]string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current != nil {
		return errors.New("package activation is already in progress")
	}
	lockedContext, release, err := c.database.AcquireDeploymentLock(ctx)
	if err != nil {
		return err
	}
	defer release()
	ctx = lockedContext
	items, err := c.packages.stageInstalled(ctx, commits)
	if err != nil {
		return err
	}
	candidates := make([]deployment.Candidate, 0, len(items))
	for _, item := range items {
		candidates = append(candidates, deployment.Candidate{PackageID: item.ID, Root: item.Path, Commit: commits[item.ID]})
	}
	if c.validateCandidates != nil {
		if err := c.validateCandidates(ctx, candidates); err != nil {
			return fmt.Errorf("validate package commands: %w", err)
		}
	}
	run, err := c.begin(ctx, candidates)
	if err != nil {
		return err
	}
	c.current = run
	defer func() { c.current = nil }()
	if err := c.setStage(ctx, run.id, "schema_synchronized", nil); err != nil {
		return err
	}
	if err := c.runHooks(ctx, run, "pre-activate", true); err != nil {
		c.fail(ctx, run, err)
		return err
	}
	if err := c.setStage(ctx, run.id, "pre_activated", nil); err != nil {
		return err
	}
	if err := c.setStage(ctx, run.id, "code_switched", nil); err != nil {
		return err
	}
	if err := c.runHooks(ctx, run, "post-activate", false); err != nil {
		c.incomplete(ctx, run, err)
		return err
	}
	if err := c.setStage(ctx, run.id, "post_activated", nil); err != nil {
		return err
	}
	for _, candidate := range run.candidates {
		if _, err := c.packages.SynchronizePackageDefinitions(ctx, candidate.PackageID, candidate.Commit); err != nil {
			c.incomplete(ctx, run, err)
			return err
		}
	}
	if err := c.publish(ctx, run); err != nil {
		c.incomplete(ctx, run, err)
		return err
	}
	if c.refreshCommands != nil {
		if err := c.refreshCommands(ctx); err != nil {
			return fmt.Errorf("refresh package commands: %w", err)
		}
	}
	return nil
}

// Recover resumes a switched activation and safely abandons an unswitched or
// partially switched one. Hook completion records make every retry targeted.
func (c *ActivationCoordinator) Recover(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	lockedContext, release, err := c.database.AcquireDeploymentLock(ctx)
	if err != nil {
		return err
	}
	defer release()
	ctx = lockedContext
	run, err := c.loadPending(ctx)
	if err != nil || run == nil {
		return err
	}
	if c.validateCandidates != nil {
		candidates := make([]deployment.Candidate, len(run.candidates))
		for index := range run.candidates {
			candidates[index] = run.candidates[index].Candidate
		}
		if err := c.validateCandidates(ctx, candidates); err != nil {
			return fmt.Errorf("validate package commands: %w", err)
		}
	}
	c.current = run
	defer func() { c.current = nil }()
	var stage string
	if err := c.database.QueryRowContext(ctx, `SELECT "stage" FROM `+activationsTable+` WHERE "activationId" = $1`, run.id).Scan(&stage); err != nil {
		return err
	}
	codeSwitched := stage == "code_switched" || stage == "post_activated"
	if !codeSwitched {
		switched := make([]activationCandidate, 0, len(run.candidates))
		for _, candidate := range run.candidates {
			active, err := c.sourceIsCandidate(ctx, candidate)
			if err != nil {
				return err
			}
			if active {
				switched = append(switched, candidate)
			}
		}
		if len(switched) > 0 && len(switched) < len(run.candidates) {
			if err := c.restoreSources(ctx, switched); err != nil {
				return err
			}
			return c.rollback(ctx, run, errors.New("activation interrupted during source switch"))
		}
		codeSwitched = len(switched) == len(run.candidates)
		if !codeSwitched {
			return c.rollback(ctx, run, errors.New("activation interrupted before source switch"))
		}
		// Fresh bootstrap sources are already in their final paths. If a crash
		// preceded a durable phase update, resume the missing schema/pre work
		// from those exact candidate commits before exposing them.
		if stage == "staged" {
			candidates := make([]deployment.Candidate, len(run.candidates))
			for index := range run.candidates {
				candidates[index] = run.candidates[index].Candidate
			}
			if err := c.schema.Prepare(ctx, candidates); err != nil {
				c.incomplete(ctx, run, err)
				return err
			}
			if err := c.setStage(ctx, run.id, "schema_synchronized", nil); err != nil {
				return err
			}
			stage = "schema_synchronized"
		}
		if stage == "schema_synchronized" {
			if err := c.runHooks(ctx, run, "pre-activate", false); err != nil {
				c.incomplete(ctx, run, err)
				return err
			}
			if err := c.setStage(ctx, run.id, "pre_activated", nil); err != nil {
				return err
			}
		}
		if err := c.setStage(ctx, run.id, "code_switched", nil); err != nil {
			return err
		}
	}
	// Complete inline while retaining the mutex.
	if err := c.runHooks(ctx, run, "post-activate", false); err != nil {
		c.incomplete(ctx, run, err)
		return err
	}
	if err := c.setStage(ctx, run.id, "post_activated", nil); err != nil {
		return err
	}
	for _, candidate := range run.candidates {
		if _, err := c.packages.SynchronizePackageDefinitions(ctx, candidate.PackageID, candidate.Commit); err != nil {
			c.incomplete(ctx, run, err)
			return err
		}
	}
	if err := c.schema.Complete(ctx, true); err != nil {
		c.incomplete(ctx, run, err)
		return err
	}
	if err := c.publish(ctx, run); err != nil {
		c.incomplete(ctx, run, err)
		return err
	}
	for _, candidate := range run.candidates {
		if err := finalizePackageDirectory(c.packages.packagePath(candidate.PackageID)); err != nil {
			return err
		}
	}
	if c.refreshCommands != nil {
		if err := c.refreshCommands(ctx); err != nil {
			return fmt.Errorf("refresh package commands: %w", err)
		}
	}
	return nil
}

// Pending reports whether startup has an unfinished activation to recover.
func (c *ActivationCoordinator) Pending(ctx context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	run, err := c.loadPending(ctx)
	return run != nil, err
}

func (c *ActivationCoordinator) sourceIsCandidate(ctx context.Context, candidate activationCandidate) (bool, error) {
	path := c.packages.packagePath(candidate.PackageID)
	if _, err := os.Lstat(path + ".previous"); err == nil {
		if _, destinationErr := os.Lstat(path); errors.Is(destinationErr, os.ErrNotExist) {
			if renameErr := os.Rename(path+".previous", path); renameErr != nil {
				return false, fmt.Errorf("restore interrupted package switch: %w", renameErr)
			}
			if syncErr := syncPackageDirectory(filepath.Dir(path)); syncErr != nil {
				return false, syncErr
			}
			return false, nil
		} else if destinationErr != nil {
			return false, destinationErr
		}
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) && candidate.previous == "" {
		return false, nil
	} else if err != nil {
		return false, err
	}
	commit, err := c.packages.installedCommit(ctx, path)
	if err != nil {
		return false, err
	}
	if commit == candidate.Commit {
		return true, nil
	}
	if commit == candidate.previous {
		return false, nil
	}
	return false, fmt.Errorf("package %s is at commit %s, expected %s or %s", candidate.PackageID, commit, candidate.previous, candidate.Commit)
}

func (c *ActivationCoordinator) restoreSources(ctx context.Context, candidates []activationCandidate) error {
	var joined error
	for _, candidate := range candidates {
		path := c.packages.packagePath(candidate.PackageID)
		if _, err := os.Lstat(path + ".previous"); err == nil {
			joined = errors.Join(joined, rollbackPackageDirectory(path))
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			joined = errors.Join(joined, err)
			continue
		}
		if candidate.previous == "" {
			joined = errors.Join(joined, fmt.Errorf("cannot restore first activation of %s", candidate.PackageID))
			continue
		}
		if output, err := c.packages.runGit(ctx, path, nil, "reset", "--hard", candidate.previous); err != nil {
			joined = errors.Join(joined, fmt.Errorf("restore %s: %w: %s", candidate.PackageID, err, cleanGitOutput(output)))
		}
	}
	return joined
}

func (c *ActivationCoordinator) begin(ctx context.Context, raw []deployment.Candidate) (*activationRun, error) {
	if len(raw) == 0 {
		return nil, errors.New("activation candidates are required")
	}
	var pending string
	if err := c.database.QueryRowContext(ctx, `SELECT "activationId" FROM `+activationsTable+` WHERE "stage" NOT IN ('complete', 'failed') ORDER BY "startedAt" LIMIT 1`).Scan(&pending); err == nil {
		return nil, fmt.Errorf("package activation %s must be recovered first", pending)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	candidates := make([]activationCandidate, 0, len(raw))
	seen := map[string]bool{}
	for _, item := range raw {
		if seen[item.PackageID] || item.PackageID == "" || item.Commit == "" || !filepath.IsAbs(item.Root) {
			return nil, fmt.Errorf("invalid activation candidate %q", item.PackageID)
		}
		seen[item.PackageID] = true
		entry, exists, err := c.packages.index.Get(ctx, item.PackageID)
		if err != nil {
			return nil, err
		}
		previous := ""
		if exists {
			previous = entry.ActiveCommit
		}
		candidates = append(candidates, activationCandidate{Candidate: item, previous: previous, first: previous == ""})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].PackageID < candidates[j].PackageID })
	id, err := activationID()
	if err != nil {
		return nil, err
	}
	previousSet, err := c.activeCommits(ctx)
	if err != nil {
		return nil, err
	}
	candidateSet := cloneCommits(previousSet)
	for _, item := range candidates {
		candidateSet[item.PackageID] = item.Commit
	}
	tx, err := c.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := database.EncodeTime(c.database, c.now().UTC())
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+activationsTable+` ("activationId", "stage", "error", "previousPackageSetHash", "candidatePackageSetHash", "startedAt", "updatedAt", "completedAt") VALUES ($1, 'staged', NULL, $2, $3, $4, $4, NULL)`, id, database.PackageSetHash(previousSet), database.PackageSetHash(candidateSet), now); err != nil {
		return nil, err
	}
	for _, item := range candidates {
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+activationPackagesTable+` ("activationId", "packageId", "previousCommit", "candidateCommit", "firstActivation") VALUES ($1, $2, $3, $4, $5)`, id, item.PackageID, nullableText(item.previous), item.Commit, item.first); err != nil {
			return nil, err
		}
		for _, hook := range []string{"pre-activate", "post-activate"} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO `+hookRunsTable+` ("activationId", "packageId", "hook", "state", "attempts", "error", "startedAt", "completedAt") VALUES ($1, $2, $3, 'pending', 0, NULL, NULL, NULL)`, id, item.PackageID, hook); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE `+packagesTable+` SET "state" = CASE WHEN "activeCommit" IS NULL THEN 'activating' ELSE "state" END, "error" = NULL, "updatedAt" = $1 WHERE "packageId" = $2`, now, item.PackageID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &activationRun{id: id, candidates: candidates}, nil
}

func (c *ActivationCoordinator) runHooks(ctx context.Context, run *activationRun, hook string, staged bool) error {
	for _, candidate := range run.candidates {
		var state string
		if err := c.database.QueryRowContext(ctx, `SELECT "state" FROM `+hookRunsTable+` WHERE "activationId" = $1 AND "packageId" = $2 AND "hook" = $3`, run.id, candidate.PackageID, hook).Scan(&state); err != nil {
			return err
		}
		if state == "succeeded" {
			continue
		}
		root := c.packages.packagePath(candidate.PackageID)
		mounts := []model.Mount(nil)
		if staged {
			root = candidate.Root
			parts := strings.Split(candidate.PackageID, "/")
			mounts = []model.Mount{{
				Source: candidate.Root, Target: "/workspace/packages/" + parts[0] + "/" + parts[1], ReadOnly: true,
				OwnerScope: candidate.PackageID, Purpose: "workspace", Persistence: "activation",
			}}
		}
		path := filepath.Join(root, "hooks", hook+".ts")
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if err := c.finishHook(ctx, run.id, candidate.PackageID, hook, nil); err != nil {
				return err
			}
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			if err == nil {
				err = errors.New("hook must be a regular TypeScript file")
			}
			_ = c.finishHook(ctx, run.id, candidate.PackageID, hook, err)
			return fmt.Errorf("%s %s: %w", candidate.PackageID, hook, err)
		}
		canonical, err := canonicalWithin(path, root)
		if err != nil || canonical != path {
			if err == nil {
				err = errors.New("hook resolves through a symbolic link")
			}
			_ = c.finishHook(ctx, run.id, candidate.PackageID, hook, err)
			return fmt.Errorf("%s %s: %w", candidate.PackageID, hook, err)
		}
		if err := c.startHook(ctx, run.id, candidate.PackageID, hook); err != nil {
			return err
		}
		parts := strings.Split(candidate.PackageID, "/")
		entrypoint := "file:///workspace/packages/" + parts[0] + "/" + parts[1] + "/hooks/" + hook + ".ts"
		reuse := false
		hookInput := ActivationHookContext{
			PackageID: candidate.PackageID, PreviousCommit: candidate.previous, CandidateCommit: candidate.Commit,
			FirstActivation: candidate.first, ActivationID: run.id,
		}
		_, runErr := c.jobs.Run(ctx, "package-"+hook, entrypoint, jobs.Options{
			OwnerID: candidate.PackageID, Arguments: []any{hookInput},
			GroupKey: "package-activation", Namespace: parts[0], Timeout: 5 * time.Minute,
			Parallelism: 1, Reuse: &reuse, ReleaseID: candidate.Commit, DatabaseAccess: "full",
			CheckModules: []string{entrypoint}, Mounts: mounts,
			Permissions: &supervisor.WorkerPermissions{Read: []string{"/opt/runtime", "/workspace/packages"}},
		})
		if err := c.finishHook(context.WithoutCancel(ctx), run.id, candidate.PackageID, hook, runErr); err != nil {
			return errors.Join(runErr, err)
		}
		if runErr != nil {
			return fmt.Errorf("%s %s hook: %w", candidate.PackageID, hook, runErr)
		}
	}
	return nil
}

func (c *ActivationCoordinator) startHook(ctx context.Context, activationID, packageID, hook string) error {
	now := database.EncodeTime(c.database, c.now().UTC())
	_, err := c.database.ExecContext(ctx, `UPDATE `+hookRunsTable+` SET "state" = 'running', "attempts" = "attempts" + 1, "error" = NULL, "startedAt" = $1, "completedAt" = NULL WHERE "activationId" = $2 AND "packageId" = $3 AND "hook" = $4`, now, activationID, packageID, hook)
	return err
}

func (c *ActivationCoordinator) finishHook(ctx context.Context, activationID, packageID, hook string, failure error) error {
	state := "succeeded"
	var message any
	if failure != nil {
		state, message = "failed", failure.Error()
	}
	now := database.EncodeTime(c.database, c.now().UTC())
	_, err := c.database.ExecContext(ctx, `UPDATE `+hookRunsTable+` SET "state" = $1, "error" = $2, "completedAt" = $3 WHERE "activationId" = $4 AND "packageId" = $5 AND "hook" = $6`, state, message, now, activationID, packageID, hook)
	return err
}

func (c *ActivationCoordinator) rollback(ctx context.Context, run *activationRun, cause error) error {
	var joined error
	if err := c.schema.Complete(ctx, false); err != nil {
		joined = errors.Join(joined, err)
	}
	for _, candidate := range run.candidates {
		if candidate.previous == "" {
			if err := c.packages.index.SetActivation(ctx, candidate.PackageID, "failed", "", cause); err != nil {
				joined = errors.Join(joined, err)
			}
			continue
		}
		if _, err := c.packages.SynchronizePackageDefinitions(ctx, candidate.PackageID, candidate.previous); err != nil {
			joined = errors.Join(joined, err)
		}
		if err := c.packages.index.SetActivation(ctx, candidate.PackageID, "ready", candidate.previous, nil); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	c.fail(ctx, run, cause)
	return joined
}

func (c *ActivationCoordinator) fail(ctx context.Context, run *activationRun, failure error) {
	run.failure = failure
	_ = c.setStage(context.WithoutCancel(ctx), run.id, "failed", failure)
}

func (c *ActivationCoordinator) incomplete(ctx context.Context, run *activationRun, failure error) {
	run.failure = failure
	_, _ = c.database.ExecContext(context.WithoutCancel(ctx), `UPDATE `+activationsTable+` SET "error" = $1, "updatedAt" = $2 WHERE "activationId" = $3`, failure.Error(), database.EncodeTime(c.database, c.now().UTC()), run.id)
}

func (c *ActivationCoordinator) setStage(ctx context.Context, activationID, stage string, failure error) error {
	var message any
	if failure != nil {
		message = failure.Error()
	}
	now := database.EncodeTime(c.database, c.now().UTC())
	completed := any(nil)
	if stage == "complete" || stage == "failed" {
		completed = now
	}
	_, err := c.database.ExecContext(ctx, `UPDATE `+activationsTable+` SET "stage" = $1, "error" = $2, "updatedAt" = $3, "completedAt" = $4 WHERE "activationId" = $5`, stage, message, now, completed, activationID)
	return err
}

func (c *ActivationCoordinator) activeCommits(ctx context.Context) (map[string]string, error) {
	entries, err := c.packages.index.List(ctx)
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for _, entry := range entries {
		if entry.ActiveCommit != "" {
			result[entry.PackageID] = entry.ActiveCommit
		}
	}
	return result, nil
}

// publish exposes every candidate, advances the package-set revision once,
// and marks the activation complete in one transaction.
func (c *ActivationCoordinator) publish(ctx context.Context, run *activationRun) error {
	tx, err := c.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := database.EncodeTime(c.database, c.now().UTC())
	for _, candidate := range run.candidates {
		result, err := tx.ExecContext(ctx, `UPDATE `+packagesTable+` SET "state" = 'ready', "activeCommit" = $1,
			"error" = NULL, "revision" = "revision" + 1, "updatedAt" = $2 WHERE "packageId" = $3`,
			candidate.Commit, now, candidate.PackageID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return fmt.Errorf("package %s disappeared during activation", candidate.PackageID)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO "the8020__system__revisions" ("domain", "revision", "updatedAt") VALUES ('packages', 1, $1) ON CONFLICT ("domain") DO UPDATE SET "revision" = "the8020__system__revisions"."revision" + 1, "updatedAt" = excluded."updatedAt"`, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+activationsTable+` SET "stage" = 'complete', "error" = NULL,
		"updatedAt" = $1, "completedAt" = $1 WHERE "activationId" = $2`, now, run.id); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *ActivationCoordinator) loadPending(ctx context.Context) (*activationRun, error) {
	var id string
	err := c.database.QueryRowContext(ctx, `SELECT "activationId" FROM `+activationsTable+` WHERE "stage" NOT IN ('complete', 'failed') ORDER BY "startedAt" DESC LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := c.database.QueryContext(ctx, `SELECT "packageId", "previousCommit", "candidateCommit", "firstActivation" FROM `+activationPackagesTable+` WHERE "activationId" = $1 ORDER BY "packageId"`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	run := &activationRun{id: id}
	for rows.Next() {
		var packageID, commit string
		var previous sql.NullString
		var first bool
		if err := rows.Scan(&packageID, &previous, &commit, &first); err != nil {
			return nil, err
		}
		run.candidates = append(run.candidates, activationCandidate{
			Candidate: deployment.Candidate{PackageID: packageID, Root: c.packages.packagePath(packageID), Commit: commit},
			previous:  previous.String, first: first,
		})
	}
	return run, rows.Err()
}

func (s *Store) packagePath(packageID string) string {
	identity, _ := ParsePackageID(packageID)
	return filepath.Join(s.packagesRoot, identity.Namespace, identity.Repository)
}

func activationID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "activation-" + hex.EncodeToString(value), nil
}

func cloneCommits(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
