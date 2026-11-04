package packages

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"the8020/kernel/database"
	"the8020/kernel/deployment"
	idgen "the8020/kernel/identity"
	"the8020/kernel/sandbox/model"
)

const (
	activationsTable           = `"the8020__packages__activations"`
	activationPackagesTable    = `"the8020__packages__activation_packages"`
	hookRunsTable              = `"the8020__packages__hook_runs"`
	unfinishedActivationStages = `('staged', 'schema_synchronized', 'pre_activated', 'code_switched', 'post_activated', 'published')`
)

type activationDatabase interface {
	database.Store
	AcquireDeploymentLock(context.Context) (context.Context, func(), error)
	AcquireActivationLock(context.Context, string) (context.Context, func(), error)
}

type ActivationCoordinatorConfig struct {
	Database           activationDatabase
	Schema             deployment.SchemaHook
	Packages           *Store
	Jobs               JobRunner
	ValidateCandidates func(context.Context, []deployment.Candidate) error
	Reindex            func(context.Context, []string) error
	Now                func() time.Time
}

// ActivationCoordinator is the single package schema/hook publication
// boundary. Source owners call Prepare before their atomic switch and Complete
// afterwards; no package becomes ready until its post hook succeeds.
type ActivationCoordinator struct {
	database           activationDatabase
	schema             deployment.SchemaHook
	packages           *Store
	jobs               JobRunner
	validateCandidates func(context.Context, []deployment.Candidate) error
	reindex            func(context.Context, []string) error
	now                func() time.Time
}

type activationRun struct {
	id         string
	candidates []activationCandidate
	failure    error
	handlers   map[string]packageHandlers
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
		reindex: config.Reindex, now: config.Now,
	}, nil
}

func (c *ActivationCoordinator) Prepare(ctx context.Context, transactionID string, candidates []deployment.Candidate) error {
	ctx = deployment.WithCandidates(ctx, candidates)
	ctx, release, err := c.database.AcquireActivationLock(ctx, transactionID)
	if err != nil {
		return err
	}
	defer release()
	handlers, err := c.validate(ctx, candidates)
	if err != nil {
		return err
	}
	stage := ""
	err = c.database.QueryRowContext(ctx, `SELECT "stage" FROM `+activationsTable+` WHERE "activationId" = $1`, transactionID).Scan(&stage)
	var run *activationRun
	if errors.Is(err, sql.ErrNoRows) {
		run, err = c.begin(ctx, transactionID, candidates)
		stage = "staged"
	} else if err == nil {
		run, err = c.loadActivation(ctx, transactionID)
		if err == nil {
			if len(run.candidates) != len(candidates) {
				return fmt.Errorf("unfinished activation %s candidate set changed", transactionID)
			}
			remaining := make(map[string]string, len(run.candidates))
			for _, saved := range run.candidates {
				remaining[saved.PackageID] = saved.Commit
			}
			for _, candidate := range candidates {
				commit, found := remaining[candidate.PackageID]
				if !found || commit != candidate.Commit {
					return fmt.Errorf("unfinished activation %s candidate changed", transactionID)
				}
				delete(remaining, candidate.PackageID)
			}
		}
	}
	if err != nil {
		return err
	}
	if stage == "failed" {
		return errors.New("activation already failed; use a new activation identity")
	}
	if stage != "staged" && stage != "schema_synchronized" {
		return nil
	}
	run.handlers = handlers
	if stage == "staged" {
		if err := c.schema.Prepare(ctx, run.id, candidates); err != nil {
			rollbackErr := c.rollback(context.WithoutCancel(ctx), run, err)
			return errors.Join(err, rollbackErr)
		}
		if err := c.setStage(ctx, run.id, "schema_synchronized", nil); err != nil {
			rollbackErr := c.rollback(context.WithoutCancel(ctx), run, err)
			return errors.Join(err, rollbackErr)
		}
	}
	if err := c.runHooks(ctx, run, "pre-activate", true); err != nil {
		rollbackErr := c.rollback(context.WithoutCancel(ctx), run, err)
		return errors.Join(err, rollbackErr)
	}
	if err := c.setStage(ctx, run.id, "pre_activated", nil); err != nil {
		rollbackErr := c.rollback(context.WithoutCancel(ctx), run, err)
		return errors.Join(err, rollbackErr)
	}
	// Existing packages retain their published availability. The activation row
	// owns pending work; shared source remains mutable during publication.
	return nil
}

func (c *ActivationCoordinator) Plan(ctx context.Context, candidates []deployment.Candidate) (json.RawMessage, error) {
	if _, err := c.validate(ctx, candidates); err != nil {
		return nil, err
	}
	planner, ok := c.schema.(deployment.PlanningHook)
	if !ok {
		return nil, errors.New("schema preview requires an updated database coordinator")
	}
	return planner.Plan(ctx, candidates)
}

func (c *ActivationCoordinator) SchemaOutcome(ctx context.Context, id string) (json.RawMessage, error) {
	run, err := c.loadActivation(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx = deployment.WithCandidates(ctx, run.rawCandidates())
	if owner, ok := c.schema.(deployment.OutcomeHook); ok {
		return owner.SchemaOutcome(ctx, id)
	}
	return nil, nil
}

func (c *ActivationCoordinator) ForwardRequired(ctx context.Context, id string) (bool, error) {
	run, err := c.loadActivation(ctx, id)
	if err != nil {
		return false, err
	}
	ctx = deployment.WithCandidates(ctx, run.rawCandidates())
	forward, err := deployment.NeedsForward(ctx, c.schema, id)
	if err != nil || forward {
		return forward, err
	}
	var stage string
	err = c.database.QueryRowContext(ctx, `SELECT "stage" FROM `+activationsTable+` WHERE "activationId" = $1`, id).Scan(&stage)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return stage == "code_switched" || stage == "post_activated" || stage == "published" || stage == "complete", err
}

func (c *ActivationCoordinator) Complete(ctx context.Context, transactionID string, activated bool) error {
	ctx, release, err := c.database.AcquireActivationLock(ctx, transactionID)
	if err != nil {
		return err
	}
	defer release()
	var stage string
	if err := c.database.QueryRowContext(ctx, `SELECT "stage" FROM `+activationsTable+` WHERE "activationId" = $1`, transactionID).Scan(&stage); err != nil {
		// A caller may lose execution before Prepare creates its durable row.
		// Aborting that exact unused ID has no work to undo.
		if errors.Is(err, sql.ErrNoRows) && !activated {
			return nil
		}
		return err
	}
	if stage == "complete" || stage == "failed" {
		if activated != (stage == "complete") {
			return fmt.Errorf("activation %s already ended as %s", transactionID, stage)
		}
		return nil
	}
	run, err := c.loadActivation(ctx, transactionID)
	if err != nil {
		return err
	}
	ctx = deployment.WithCandidates(ctx, run.rawCandidates())
	if stage == "published" {
		if !activated {
			return fmt.Errorf("activation %s is already published; finish activation instead of rolling it back", transactionID)
		}
		if err := c.verifyInstalledCandidates(ctx, run); err != nil {
			return err
		}
		return c.finish(deployment.WithCandidates(ctx, run.rawCandidates()), run)
	}
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
	if err := c.schema.Complete(ctx, run.id, true); err != nil {
		c.incomplete(ctx, run, err)
		return err
	}
	if err := c.publish(ctx, run); err != nil {
		c.incomplete(ctx, run, err)
		return err
	}
	return c.finish(ctx, run)
}

// Bootstrap runs both hooks for the source set already staged by the
// installer, then publishes its package and service records. Schema tables
// have already been synchronized, so there is no second schema pass.
func (c *ActivationCoordinator) Bootstrap(ctx context.Context, commits map[string]string) error {
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
	handlers, err := c.validate(ctx, candidates)
	if err != nil {
		return err
	}
	transactionID, err := activationID()
	if err != nil {
		return err
	}
	ctx, releaseActivation, err := c.database.AcquireActivationLock(ctx, transactionID)
	if err != nil {
		return err
	}
	defer releaseActivation()
	run, err := c.begin(ctx, transactionID, candidates)
	if err != nil {
		return err
	}
	run.handlers = handlers
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
	if err := c.publish(ctx, run); err != nil {
		c.incomplete(ctx, run, err)
		return err
	}
	return c.finish(ctx, run)
}

// Recover visits a bounded snapshot of unfinished attempts. Busy or failed
// attempts remain visible without preventing recovery of unrelated packages.
func (c *ActivationCoordinator) Recover(ctx context.Context) error {
	return c.RecoverPackages(ctx, nil)
}

// RecoverPackages resumes unfinished activations intersecting an explicit selection.
// An empty selection recovers every unfinished activation at startup.
func (c *ActivationCoordinator) RecoverPackages(ctx context.Context, packageIDs []string) error {
	selection := ""
	arguments := make([]any, len(packageIDs))
	if len(packageIDs) > 0 {
		parameters := make([]string, len(packageIDs))
		for i, id := range packageIDs {
			parameters[i], arguments[i] = fmt.Sprintf("$%d", i+1), id
		}
		selection = ` AND EXISTS (SELECT 1 FROM ` + activationPackagesTable + ` p WHERE p."activationId" = a."activationId" AND p."packageId" IN (` + strings.Join(parameters, ",") + `))`
	}
	rows, err := c.database.QueryContext(ctx, `SELECT "activationId" FROM `+activationsTable+` a WHERE "stage" IN `+unfinishedActivationStages+selection+` ORDER BY "startedAt", "activationId" LIMIT 257`, arguments...)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	if len(ids) > 256 {
		return errors.New("activation recovery exceeds the 256 unfinished-attempt limit")
	}
	var failures error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		if err := c.recoverActivation(ctx, id); err != nil {
			failures = errors.Join(failures, fmt.Errorf("recover activation %s: %w", id, err))
		}
	}
	return failures
}

func (c *ActivationCoordinator) recoverActivation(ctx context.Context, id string) error {
	run, err := c.loadActivation(ctx, id)
	if err != nil {
		return err
	}
	ctx = deployment.WithCandidates(ctx, run.rawCandidates())
	releaseSources, err := LockSources(ctx, c.packages.packagesRoot, run.packageIDs())
	if err != nil {
		return err
	}
	defer releaseSources()
	ctx, release, err := c.database.AcquireActivationLock(ctx, run.id)
	if err != nil {
		return err
	}
	defer release()
	var stage string
	if err := c.database.QueryRowContext(ctx, `SELECT "stage" FROM `+activationsTable+` WHERE "activationId" = $1`, run.id).Scan(&stage); err != nil {
		return err
	}
	if stage == "complete" || stage == "failed" {
		return nil
	}
	if stage == "published" {
		if err := c.verifyInstalledCandidates(ctx, run); err != nil {
			return err
		}
		return c.finish(deployment.WithCandidates(ctx, run.rawCandidates()), run)
	}
	if err := c.verifySynchronization(ctx, id); err != nil {
		return err
	}
	candidates := make([]deployment.Candidate, len(run.candidates))
	for i := range run.candidates {
		candidates[i] = run.candidates[i].Candidate
	}
	forward, err := c.ForwardRequired(ctx, run.id)
	if err != nil {
		return err
	}
	codeSwitched := stage == "code_switched" || stage == "post_activated"
	if codeSwitched {
		if err := c.verifyInstalledCandidates(ctx, run); err != nil {
			return err
		}
		candidates = run.rawCandidates()
		ctx = deployment.WithCandidates(ctx, candidates)
	}
	switched := make([]activationCandidate, 0, len(run.candidates))
	if !codeSwitched {
		for _, candidate := range run.candidates {
			active, err := c.sourceIsCandidate(ctx, candidate)
			if err != nil {
				return err
			}
			if active {
				switched = append(switched, candidate)
			}
		}
		if !forward && len(switched) < len(run.candidates) {
			if len(switched) == 0 {
				return c.rollback(ctx, run, errors.New("activation interrupted before source switch"))
			}
			if err := c.restoreSources(ctx, switched); err != nil {
				return err
			}
			return c.rollback(ctx, run, errors.New("activation interrupted during source switch"))
		}
	}
	// Recovery must not run schema or hooks against an older installed fallback.
	for _, candidate := range run.candidates {
		if candidate.Commit == "" {
			continue
		}
		if candidate.Root == c.packages.packagePath(candidate.PackageID) {
			commit, err := c.packages.installedCommit(ctx, candidate.Root)
			if err != nil {
				return fmt.Errorf("inspect recovery candidate %s: %w", candidate.PackageID, err)
			}
			if commit != candidate.Commit {
				return fmt.Errorf("recovery candidate %s changed or staged source is missing: expected %s, found %s", candidate.PackageID, candidate.Commit, commit)
			}
			continue
		}
		if _, err := os.Stat(filepath.Join(candidate.Root, ".git")); err == nil {
			commit, err := CleanRepositoryHead(ctx, candidate.Root)
			if err != nil {
				return fmt.Errorf("inspect forward candidate %s: %w", candidate.PackageID, err)
			}
			if commit != candidate.Commit {
				return fmt.Errorf("forward candidate %s changed: expected %s, found %s", candidate.PackageID, candidate.Commit, commit)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else if filepath.Base(candidate.Root) == "repository" &&
			filepath.Dir(filepath.Dir(candidate.Root)) == filepath.Dir(c.packages.packagePath(candidate.PackageID)) &&
			strings.HasPrefix(filepath.Base(filepath.Dir(candidate.Root)), "."+filepath.Base(c.packages.packagePath(candidate.PackageID))+"-sync-") {
			return fmt.Errorf("forward candidate %s staged Git metadata is missing", candidate.PackageID)
		}
	}
	handlers, err := c.validate(ctx, candidates)
	if err != nil {
		return err
	}
	run.handlers = handlers
	if !codeSwitched {
		if forward && len(switched) < len(run.candidates) {
			if stage == "staged" {
				if err := c.schema.Prepare(ctx, run.id, candidates); err != nil {
					c.incomplete(ctx, run, err)
					return err
				}
				if err := c.setStage(ctx, run.id, "schema_synchronized", nil); err != nil {
					return err
				}
				stage = "schema_synchronized"
			}
			if stage == "schema_synchronized" {
				if err := c.runHooks(ctx, run, "pre-activate", true); err != nil {
					c.incomplete(ctx, run, err)
					return err
				}
				if err := c.setStage(ctx, run.id, "pre_activated", nil); err != nil {
					return err
				}
				stage = "pre_activated"
			}
			for _, candidate := range run.candidates {
				active, err := c.sourceIsCandidate(ctx, candidate)
				if err != nil {
					return err
				}
				if active {
					continue
				}
				destination := c.packages.packagePath(candidate.PackageID)
				if candidate.Commit == "" {
					return errors.New("forward recovery cannot infer a removed candidate")
				}
				if _, err := os.Stat(filepath.Join(candidate.Root, ".git")); err == nil {
					if commit, err := CleanRepositoryHead(ctx, candidate.Root); err != nil || commit != candidate.Commit {
						return fmt.Errorf("forward candidate changed: %s: %v", candidate.PackageID, err)
					}
					if _, err := replacePackageDirectory(destination, candidate.Root); err != nil {
						return err
					}
				} else if errors.Is(err, os.ErrNotExist) {
					if _, err := c.packages.runGit(ctx, destination, nil, "reset", "--hard", candidate.Commit); err != nil {
						return err
					}
				} else {
					return err
				}
			}
			switched = run.candidates
		}
		// Fresh bootstrap sources are already in their final paths. If a crash
		// preceded a durable phase update, resume the missing schema/pre work
		// from those exact candidate commits before exposing them.
		if stage == "staged" {
			candidates := make([]deployment.Candidate, len(run.candidates))
			for index := range run.candidates {
				candidates[index] = run.candidates[index].Candidate
			}
			if err := c.schema.Prepare(ctx, run.id, candidates); err != nil {
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
	// Complete while retaining this activation's operation ownership.
	if err := c.runHooks(ctx, run, "post-activate", false); err != nil {
		c.incomplete(ctx, run, err)
		return err
	}
	if err := c.setStage(ctx, run.id, "post_activated", nil); err != nil {
		return err
	}
	if err := c.schema.Complete(ctx, run.id, true); err != nil {
		c.incomplete(ctx, run, err)
		return err
	}
	if err := c.publish(ctx, run); err != nil {
		c.incomplete(ctx, run, err)
		return err
	}
	return c.finish(ctx, run)
}

// verifyInstalledCandidates checks source identity before resuming published work.
func (c *ActivationCoordinator) verifyInstalledCandidates(ctx context.Context, run *activationRun) error {
	for i, candidate := range run.candidates {
		path := c.packages.packagePath(candidate.PackageID)
		if candidate.Commit == "" {
			if _, err := os.Lstat(path); err == nil {
				return fmt.Errorf("removed candidate %s is still present", candidate.PackageID)
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("inspect removed candidate %s: %w", candidate.PackageID, err)
			}
			continue
		}
		commit, err := c.packages.installedCommit(ctx, path)
		if err != nil {
			return fmt.Errorf("inspect recovery candidate %s: %w", candidate.PackageID, err)
		}
		if commit != candidate.Commit {
			return fmt.Errorf("recovery candidate %s changed: expected %s, found %s", candidate.PackageID, candidate.Commit, commit)
		}
		run.candidates[i].Root = path
	}
	return nil
}

// Pending reports whether startup has an unfinished activation to recover.
func (c *ActivationCoordinator) Pending(ctx context.Context) (bool, error) {
	run, err := c.loadPending(ctx)
	return run != nil, err
}

func (c *ActivationCoordinator) sourceIsCandidate(ctx context.Context, candidate activationCandidate) (bool, error) {
	path := c.packages.packagePath(candidate.PackageID)
	if candidate.Commit == "" {
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
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
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if candidate.previous == "" {
		if removed, err := removeEmptyPackagePlaceholder(path); err != nil || removed {
			return false, err
		}
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

func (c *ActivationCoordinator) begin(ctx context.Context, id string, raw []deployment.Candidate) (*activationRun, error) {
	if !idgen.Is(id, "act") {
		return nil, errors.New("invalid activation identity")
	}
	if len(raw) == 0 || len(raw) > 256 {
		return nil, errors.New("activation requires 1..256 candidates")
	}
	seen := map[string]bool{}
	parameters := make([]string, len(raw))
	arguments := make([]any, len(raw))
	for index, item := range raw {
		_, identityErr := ParsePackageID(item.PackageID)
		if seen[item.PackageID] || identityErr != nil || !filepath.IsAbs(item.Root) {
			return nil, fmt.Errorf("invalid activation candidate %q", item.PackageID)
		}
		seen[item.PackageID] = true
		parameters[index], arguments[index] = fmt.Sprintf("$%d", index+1), item.PackageID
	}
	ctx, release, err := c.database.AcquireDeploymentLock(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	// ponytail: cap unfinished activations at 256; normalize active package
	// claims if deployments outgrow this bounded, stage-indexed lookup.
	var pendingCount int
	if err := c.database.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM `+activationsTable+` WHERE "stage" IN `+unfinishedActivationStages+` LIMIT 256) pending`).Scan(&pendingCount); err != nil {
		return nil, err
	}
	if pendingCount == 256 {
		return nil, errors.New("too many unfinished package activations (maximum 256)")
	}
	var pending, packageID string
	if err := c.database.QueryRowContext(ctx, `SELECT a."activationId", p."packageId" FROM `+activationsTable+` a JOIN `+activationPackagesTable+` p ON p."activationId" = a."activationId"
		WHERE a."stage" IN `+unfinishedActivationStages+` AND p."packageId" IN (`+strings.Join(parameters, ",")+`) LIMIT 1`, arguments...).Scan(&pending, &packageID); err == nil {
		return nil, fmt.Errorf("package %s belongs to unfinished activation %s", packageID, pending)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	candidates := make([]activationCandidate, 0, len(raw))
	for _, item := range raw {
		entry, exists, err := c.packages.index.Get(ctx, item.PackageID)
		if err != nil {
			return nil, err
		}
		if item.Commit == "" && (!exists || item.Root != c.packages.packagePath(item.PackageID)) {
			return nil, fmt.Errorf("invalid package deletion %q", item.PackageID)
		}
		previous := ""
		if exists {
			previous = entry.ActiveCommit
		}
		candidates = append(candidates, activationCandidate{Candidate: item, previous: previous, first: previous == ""})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].PackageID < candidates[j].PackageID })
	previousSet, err := c.activeCommits(ctx)
	if err != nil {
		return nil, err
	}
	candidateSet := cloneCommits(previousSet)
	for _, item := range candidates {
		if item.Commit == "" {
			delete(candidateSet, item.PackageID)
		} else {
			candidateSet[item.PackageID] = item.Commit
		}
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
		if item.first && item.Commit != "" {
			identity, _ := ParsePackageID(item.PackageID)
			_, synchronizing := ctx.Value(synchronizationKey{}).([]synchronizationIntent)
			if _, err := tx.ExecContext(ctx, `INSERT INTO `+packagesTable+` ("packageId", "author", "repository", "local", "state", "revision", "createdAt", "updatedAt")
				VALUES ($1, $2, $3, $4, 'desired', 0, $5, $5)
				ON CONFLICT ("packageId") DO NOTHING`, item.PackageID, identity.Namespace, identity.Repository, !synchronizing, now); err != nil {
				return nil, err
			}
		}
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
	if intents, ok := ctx.Value(synchronizationKey{}).([]synchronizationIntent); ok {
		encoded, err := database.EncodeJSON(c.database.Backend(), intents)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+synchronizationsTable+` ("activationId", "packages") VALUES ($1, $2)`, id, encoded); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &activationRun{id: id, candidates: candidates}, nil
}

func (run *activationRun) packageIDs() []string {
	ids := make([]string, len(run.candidates))
	for i, candidate := range run.candidates {
		ids[i] = candidate.PackageID
	}
	return ids
}

func (c *ActivationCoordinator) validate(ctx context.Context, candidates []deployment.Candidate) (map[string]packageHandlers, error) {
	for _, candidate := range candidates {
		if candidate.Commit == "" {
			continue
		}
		info, err := os.Stat(candidate.Root)
		if err != nil {
			return nil, fmt.Errorf("candidate %s source: %w", candidate.PackageID, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("candidate %s source is not a directory", candidate.PackageID)
		}
	}
	handlers, err := c.packages.indexCandidateHandlers(ctx, candidates)
	if err != nil {
		return nil, fmt.Errorf("validate package handlers: %w", err)
	}
	if c.validateCandidates != nil {
		if err := c.validateCandidates(ctx, candidates); err != nil {
			return nil, fmt.Errorf("validate package commands: %w", err)
		}
	}
	return handlers, nil
}

func (c *ActivationCoordinator) runHooks(ctx context.Context, run *activationRun, hook string, staged bool) error {
	selected := make(map[string]deployment.Candidate, len(run.candidates))
	var mounts []model.Mount
	for _, candidate := range run.candidates {
		item := candidate.Candidate
		if item.Commit == "" {
			continue
		}
		installedRoot := c.packages.packagePath(item.PackageID)
		if staged && item.Root != installedRoot {
			mounts = append(mounts, model.Mount{
				Source: item.Root, Target: packageSandboxRoot + "/" + item.PackageID, ReadOnly: true,
				OwnerScope: item.PackageID, Purpose: "workspace", Persistence: "activation",
			})
		} else if !staged {
			item.Root = installedRoot
		}
		selected[item.PackageID] = item
	}
	if run.handlers == nil {
		candidates := make([]deployment.Candidate, 0, len(selected))
		for _, candidate := range run.candidates {
			if candidate.Commit != "" {
				candidates = append(candidates, selected[candidate.PackageID])
			}
		}
		handlers, err := c.packages.indexCandidateHandlers(ctx, candidates)
		if err != nil {
			return err
		}
		run.handlers = handlers
	}
	for _, candidate := range run.candidates {
		var state string
		if err := c.database.QueryRowContext(ctx, `SELECT "state" FROM `+hookRunsTable+` WHERE "activationId" = $1 AND "packageId" = $2 AND "hook" = $3`, run.id, candidate.PackageID, hook).Scan(&state); err != nil {
			return err
		}
		if state == "succeeded" {
			continue
		}
		handlers := run.handlers[candidate.PackageID].hooks[hook]
		if len(handlers) == 0 {
			if err := c.finishHook(ctx, run.id, candidate.PackageID, hook, nil); err != nil {
				return err
			}
			continue
		}
		if err := c.startHook(ctx, run.id, candidate.PackageID, hook); err != nil {
			return err
		}
		hookInput := ActivationHookContext{
			PackageID: candidate.PackageID, PreviousCommit: candidate.previous, CandidateCommit: candidate.Commit,
			FirstActivation: candidate.first, ActivationID: run.id,
		}
		_, runErr := c.packages.RunHookChain(ctx, c.jobs, candidate.PackageID, hook, handlers, hookInput, map[string]any{}, mounts)
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
	forward, err := c.ForwardRequired(ctx, run.id)
	if err != nil {
		return err
	}
	if forward {
		failure := fmt.Errorf("schema/data committed for %s; activation incomplete, resume the same candidate forward: %w", run.id, cause)
		c.incomplete(ctx, run, failure)
		return failure
	}
	var joined error
	for _, candidate := range run.candidates {
		if candidate.previous == "" {
			if err := c.packages.index.SetActivation(ctx, candidate.PackageID, "failed", "", cause); err != nil {
				joined = errors.Join(joined, err)
			}
			continue
		}
		if err := c.packages.index.SetActivation(ctx, candidate.PackageID, "ready", candidate.previous, nil); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	// Schema restoration reads the previous source through the ready catalog.
	if err := c.schema.Complete(ctx, run.id, false); err != nil {
		joined = errors.Join(joined, err)
	}
	run.failure = cause
	if joined != nil {
		c.incomplete(ctx, run, errors.Join(cause, joined))
		return joined
	}
	return c.setStage(context.WithoutCancel(ctx), run.id, "failed", cause)
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
// and records the remaining local finalization in one transaction.
func (c *ActivationCoordinator) publish(ctx context.Context, run *activationRun) error {
	intents, err := c.synchronizationIntent(ctx, run.id)
	if err != nil {
		return err
	}
	tx, err := c.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := database.EncodeTime(c.database, c.now().UTC())
	for _, intent := range intents {
		desired, previous := intent.Desired, intent.Previous
		result, err := tx.ExecContext(ctx, `UPDATE `+packagesTable+` SET "source" = $1, "requestedCommit" = $2, "requestedTag" = $3, "local" = $4
			WHERE "packageId" = $5 AND COALESCE("source", '') = $6 AND COALESCE("requestedCommit", '') = $7
			AND COALESCE("requestedTag", '') = $8 AND COALESCE("secretName", '') = $9 AND "local" = $10`,
			nullableText(desired.Source), nullableText(desired.Commit), nullableText(desired.Tag), desired.Local, intent.PackageID,
			previous.Source, previous.Commit, previous.Tag, previous.Secret, previous.Local)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return fmt.Errorf("%s source configuration changed during activation %s", intent.PackageID, run.id)
		}
	}
	for _, candidate := range run.candidates {
		state := "ready"
		if candidate.Commit == "" {
			state = "retired"
		}
		result, err := tx.ExecContext(ctx, `UPDATE `+packagesTable+` SET "state" = $4, "activeCommit" = $1,
			"error" = NULL, "revision" = "revision" + 1, "updatedAt" = $2 WHERE "packageId" = $3`,
			nullableText(candidate.Commit), now, candidate.PackageID, state)
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
	if _, err := tx.ExecContext(ctx, `UPDATE `+activationsTable+` SET "stage" = 'published', "error" = NULL,
		"updatedAt" = $1, "completedAt" = NULL WHERE "activationId" = $2`, now, run.id); err != nil {
		return err
	}
	return tx.Commit()
}

// finish resumes after durable publication without repeating schema work, hooks
// or the package revision. Failed indexing and backup cleanup remain retryable.
func (c *ActivationCoordinator) finish(ctx context.Context, run *activationRun) (err error) {
	defer func() {
		if err != nil {
			c.incomplete(ctx, run, err)
		}
	}()
	if c.reindex != nil {
		if err := c.reindex(ctx, run.packageIDs()); err != nil {
			return fmt.Errorf("reindex packages: %w", err)
		}
	}
	for _, candidate := range run.candidates {
		if err := finalizePackageDirectory(c.packages.packagePath(candidate.PackageID)); err != nil {
			return err
		}
	}
	return c.setStage(ctx, run.id, "complete", nil)
}

func (c *ActivationCoordinator) loadPending(ctx context.Context) (*activationRun, error) {
	var id string
	err := c.database.QueryRowContext(ctx, `SELECT "activationId" FROM `+activationsTable+` WHERE "stage" IN `+unfinishedActivationStages+` ORDER BY "startedAt", "activationId" LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c.loadActivation(ctx, id)
}

func (c *ActivationCoordinator) loadActivation(ctx context.Context, id string) (*activationRun, error) {
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
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if recovery, ok := c.schema.(deployment.RecoveryHook); ok {
		candidates, err := recovery.RecoveryCandidates(ctx, id)
		if err != nil {
			return nil, err
		}
		for i := range run.candidates {
			for _, candidate := range candidates {
				if candidate.PackageID == run.candidates[i].PackageID && candidate.Commit == run.candidates[i].Commit {
					if _, err := os.Stat(candidate.Root); err == nil {
						run.candidates[i].Root = candidate.Root
					}
				}
			}
		}
	}
	return run, nil
}

func (run *activationRun) rawCandidates() []deployment.Candidate {
	result := make([]deployment.Candidate, len(run.candidates))
	for i, candidate := range run.candidates {
		result[i] = candidate.Candidate
	}
	return result
}

func (s *Store) packagePath(packageID string) string {
	identity, _ := ParsePackageID(packageID)
	return filepath.Join(s.packagesRoot, identity.Namespace, identity.Repository)
}

func activationID() (string, error) { return idgen.New("act") }

func cloneCommits(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
