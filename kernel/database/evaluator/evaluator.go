// Package evaluator bridges activated package table definitions to sandboxed Deno jobs.
package evaluator

import (
	"context"
	"encoding/json"
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
	"the8020/kernel/execution"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/execution/supervisor"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/sandbox/model"
)

const maximumBatch = 256
const packageMountRoot = "/workspace/packages"

type JobRunner interface {
	Run(context.Context, string, string, jobs.Options) (jobs.Record, error)
}

// PackageCatalog is the read-only source view needed to discover and evaluate
// activated table modules. It deliberately excludes package desired state.
type PackageCatalog interface {
	ListPackages() ([]workspacepackages.Package, error)
	ResolvePackage(string) (workspacepackages.Package, error)
}

// activatedPackageCatalog proves that the source mounted for evaluation is the
// clean checkout recorded as ready in the shared package index. The bootstrap
// catalog intentionally does not implement it because no active package set
// exists until first initialization completes.
type activatedPackageCatalog interface {
	ActivatedPackageCommit(context.Context, string) (string, error)
}

type Config struct {
	Packages PackageCatalog
	Jobs     JobRunner
	Database *database.Manager
}

type Evaluator struct {
	packages PackageCatalog
	jobs     JobRunner
	database *database.Manager
	mu       sync.Mutex
}

func New(config Config) (*Evaluator, error) {
	if config.Packages == nil || config.Jobs == nil || config.Database == nil {
		return nil, errors.New("package store, job runner, and database are required")
	}
	return &Evaluator{packages: config.Packages, jobs: config.Jobs, database: config.Database}, nil
}

// UseActivatedPackages switches discovery from the bootstrap filesystem view
// to the database-authoritative package view after initialization succeeds.
func (e *Evaluator) UseActivatedPackages(packages PackageCatalog) error {
	if packages == nil {
		return errors.New("activated package catalog is required")
	}
	e.mu.Lock()
	e.packages = packages
	e.mu.Unlock()
	return nil
}

// PackageSet returns the exact activated source commits without evaluating any
// table module. Bootstrap uses it to publish the initial package set only after
// schemas and hooks have succeeded.
func (e *Evaluator) PackageSet(ctx context.Context) (map[string]string, error) {
	_, commits, err := e.resolvePackages(ctx, nil)
	return commits, err
}

type evaluationRequest struct {
	PackageRoot string           `json:"package_root"`
	Tables      []evaluationItem `json:"tables"`
}

type evaluationItem struct {
	Module          string   `json:"module"`
	ExpectedTableID string   `json:"expected_table_id"`
	PackageID       string   `json:"package_id"`
	PackageCommit   string   `json:"package_commit"`
	Dependencies    []string `json:"dependencies,omitempty"`
}

type evaluationResponse struct {
	Tables []database.EvaluatedTable `json:"tables"`
}

// Evaluate discovers all activated definitions or every definition in a small
// selected package set. It is used by explicit administration and rollback.
func (e *Evaluator) Evaluate(ctx context.Context, selected []string) (database.DefinitionSet, error) {
	packages, commits, err := e.resolvePackages(ctx, selected)
	if err != nil {
		return database.DefinitionSet{}, err
	}
	items, err := discoverPackages(packages, commits)
	if err != nil {
		return database.DefinitionSet{}, err
	}
	result, err := e.evaluateItems(ctx, items, database.PackageSetHash(commits), nil)
	result.Packages = packageIDs(packages)
	result.PackageCommits = commits
	result.PackageSetHash = database.PackageSetHash(commits)
	return result, err
}

// InspectDefinition evaluates exactly one deployed source module for table detail.
func (e *Evaluator) InspectDefinition(ctx context.Context, source database.TableSource) (*database.EvaluatedTable, error) {
	e.mu.Lock()
	catalog := e.packages
	e.mu.Unlock()
	item, err := catalog.ResolvePackage(source.SourcePackage)
	if err != nil {
		return nil, err
	}
	commit, err := packageCommit(ctx, catalog, item)
	if err != nil {
		return nil, err
	}
	discovered, err := discoverPackage(item, commit, map[string]string{})
	if err != nil {
		return nil, err
	}
	for _, candidate := range discovered {
		if candidate.ExpectedTableID != source.TableID ||
			(source.SourceModule != "" && candidate.Module != source.SourceModule) {
			continue
		}
		fingerprint := e.database.Status().PackageSetHash + ":" + commit
		tables, err := e.evaluateBatch(ctx, []evaluationItem{candidate}, fingerprint, nil)
		if err != nil {
			return nil, err
		}
		return &tables[0], nil
	}
	return nil, nil
}

// SynchronizeAll incrementally evaluates and commits bounded batches. A failed
// first boot can resume already synchronized package commits without retaining
// one giant descriptor result in memory.
func (e *Evaluator) SynchronizeAll(ctx context.Context, resume bool) ([]database.SynchronizationResult, error) {
	return e.synchronizeAll(ctx, resume, false, true)
}

// SynchronizeInitialSchemas prepares every bootstrap-package table without
// publishing database readiness. The bootstrap coordinator completes settings,
// package records, and hooks before marking READY.
func (e *Evaluator) SynchronizeInitialSchemas(ctx context.Context, resume bool) ([]database.SynchronizationResult, error) {
	return e.synchronizeAll(ctx, resume, false, false)
}

// RecoverAll aligns an unfinished deployment to the package tree that is
// actually active after a crash. Only rollback-safe reversals are enabled.
func (e *Evaluator) RecoverAll(ctx context.Context) ([]database.SynchronizationResult, error) {
	return e.synchronizeAll(ctx, true, true, true)
}

func (e *Evaluator) synchronizeAll(ctx context.Context, resume, recovery, complete bool) ([]database.SynchronizationResult, error) {
	lockedContext, release, err := e.database.AcquireDeploymentLock(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = lockedContext
	var transactionID string
	if recovery {
		if pending, exists, err := e.database.PendingDeployment(ctx); err != nil {
			return nil, err
		} else if !exists {
			return nil, errors.New("database schema deployment is not pending")
		} else {
			transactionID = pending.ID
		}
	}
	packages, commits, err := e.resolvePackages(ctx, nil)
	if err != nil {
		return nil, err
	}
	items, err := discoverPackages(packages, commits)
	if err != nil {
		return nil, err
	}
	allIDs := make([]string, len(items))
	for index := range items {
		allIDs[index] = items[index].ExpectedTableID
	}
	initializing := !e.database.Status().Initialized
	if initializing {
		e.database.BeginInitialization()
	}
	if resume && initializing {
		completed, completedErr := e.database.CompletedTableIDs(ctx, commits)
		if completedErr != nil {
			e.database.SetInitializationFailure(ctx, completedErr)
			return nil, completedErr
		}
		filtered := items[:0]
		for _, item := range items {
			if !completed[item.ExpectedTableID] {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	results := make([]database.SynchronizationResult, 0, len(items))
	fingerprint := database.PackageSetHash(commits)
	for offset := 0; offset < len(items); offset += maximumBatch {
		end := min(offset+maximumBatch, len(items))
		tables, batchErr := e.evaluateBatch(ctx, items[offset:end], fingerprint, nil)
		if batchErr == nil {
			var batchResults []database.SynchronizationResult
			batchResults, batchErr = e.database.Synchronize(ctx, tables, database.SynchronizationOptions{
				Recovery: recovery, SkipReferenceValidation: true,
			})
			results = append(results, batchResults...)
		}
		if batchErr != nil {
			if initializing {
				e.database.SetInitializationFailure(ctx, batchErr)
			}
			return results, batchErr
		}
	}
	if err := e.database.FinalizeFullSynchronization(ctx, allIDs, commits); err != nil {
		if initializing {
			e.database.SetInitializationFailure(ctx, err)
		}
		return results, err
	}
	if complete {
		if err := e.database.CompleteInitialization(ctx, commits); err != nil {
			if initializing {
				e.database.SetInitializationFailure(ctx, err)
			}
			return results, err
		}
	}
	if recovery {
		if err := e.database.CompleteDeployment(ctx, transactionID, false); err != nil {
			return results, err
		}
	}
	return results, nil
}

func (e *Evaluator) evaluateItems(ctx context.Context, items []evaluationItem, fingerprint string, mounts []model.Mount) (database.DefinitionSet, error) {
	result := database.DefinitionSet{PackageSetHash: fingerprint, Tables: []database.EvaluatedTable{}}
	for offset := 0; offset < len(items); offset += maximumBatch {
		end := min(offset+maximumBatch, len(items))
		tables, err := e.evaluateBatch(ctx, items[offset:end], fingerprint, mounts)
		if err != nil {
			return database.DefinitionSet{}, err
		}
		result.Tables = append(result.Tables, tables...)
	}
	return result, nil
}

func (e *Evaluator) evaluateBatch(ctx context.Context, batch []evaluationItem, fingerprint string, mounts []model.Mount) ([]database.EvaluatedTable, error) {
	if len(batch) == 0 || len(batch) > maximumBatch {
		return nil, errors.New("database table evaluation batch must contain 1..256 modules")
	}
	modules := make([]string, len(batch))
	for index := range batch {
		modules[index] = batch[index].Module
	}
	reuse := true
	record, err := e.jobs.Run(ctx, "database-table-evaluator", "file:///workspace/packages/the8020/db/internal/evaluator.ts", jobs.Options{
		User:    execution.DefaultUser(ctx),
		OwnerID: "the8020/db", Arguments: []any{evaluationRequest{PackageRoot: packageMountRoot, Tables: batch}},
		Namespace: "the8020", Timeout: 2 * time.Minute,
		Parallelism: 1, Reuse: &reuse, ReleaseID: fingerprint, DatabaseAccess: "none", DependencyModules: modules, Mounts: mounts,
		Permissions: &supervisor.WorkerPermissions{Read: []string{"/opt/runtime", packageMountRoot}},
	})
	if err != nil {
		return nil, fmt.Errorf("evaluate database tables: %w", err)
	}
	encoded, err := json.Marshal(record.Result)
	if err != nil {
		return nil, fmt.Errorf("encode evaluator result: %w", err)
	}
	var response evaluationResponse
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil || len(response.Tables) != len(batch) {
		return nil, errors.New("table evaluator returned an invalid batch")
	}
	wanted := make(map[string]evaluationItem, len(batch))
	for _, item := range batch {
		wanted[item.ExpectedTableID] = item
	}
	seen := map[string]bool{}
	for index := range response.Tables {
		table := &response.Tables[index]
		item, exists := wanted[table.Descriptor.TableID]
		if !exists || seen[table.Descriptor.TableID] || table.SourceModule != item.Module || table.SourcePackage != item.PackageID || table.SourceCommit != item.PackageCommit {
			return nil, errors.New("table evaluator returned mismatched source identity")
		}
		seen[table.Descriptor.TableID] = true
		dependencies := record.ModuleDependencies[item.Module]
		if len(dependencies) == 0 {
			dependencies = append([]string{item.Module}, item.Dependencies...)
		}
		table.Dependencies = packageDependencies(dependencies, item.Module)
	}
	sort.Slice(response.Tables, func(i, j int) bool {
		return response.Tables[i].Descriptor.TableID < response.Tables[j].Descriptor.TableID
	})
	return response.Tables, nil
}

func packageDependencies(paths []string, module string) []string {
	seen := map[string]bool{module: true}
	for _, path := range paths {
		if strings.HasPrefix(path, packageMountRoot+"/") {
			seen[filepath.ToSlash(filepath.Clean(path))] = true
		}
	}
	result := make([]string, 0, len(seen))
	for path := range seen {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

// Prepare evaluates every table in only the candidate packages. Package
// activation is intentionally independent of static import analysis.
func (e *Evaluator) Prepare(ctx context.Context, transactionID string, candidates []deployment.Candidate) error {
	ctx, release, err := e.database.AcquireActivationLock(ctx, transactionID)
	if err != nil {
		return err
	}
	defer release()
	if len(candidates) == 0 {
		return nil
	}
	// The service plane is unavailable until the first complete synchronization.
	// Let an operator replace a broken package, then retry that bounded full load.
	if !e.database.Status().Initialized {
		return nil
	}
	databaseCandidates := make([]database.DeploymentCandidate, len(candidates))
	for index, candidate := range candidates {
		if _, err := workspacepackages.ParsePackageID(candidate.PackageID); err != nil || !filepath.IsAbs(candidate.Root) {
			return fmt.Errorf("invalid schema candidate %s", candidate.PackageID)
		}
		databaseCandidates[index] = database.DeploymentCandidate{PackageID: candidate.PackageID, CandidateCommit: candidate.Commit}
	}
	pending, err := e.database.BeginDeployment(ctx, transactionID, databaseCandidates)
	if err != nil {
		return err
	}
	items, retired, mounts, err := e.incrementalItems(ctx, candidates)
	if err == nil {
		err = e.database.UpdatePendingDeployment(ctx, transactionID, "evaluating", nil)
	}
	for offset := 0; err == nil && offset < len(items); offset += maximumBatch {
		end := min(offset+maximumBatch, len(items))
		var tables []database.EvaluatedTable
		tables, err = e.evaluateBatch(ctx, items[offset:end], pending.CandidatePackageSetHash, mounts)
		if err == nil {
			_, err = e.database.Synchronize(ctx, tables, database.SynchronizationOptions{SkipReferenceValidation: true})
		}
	}
	if err == nil && len(retired) > 0 {
		_, err = e.database.Synchronize(ctx, nil, database.SynchronizationOptions{RetireTables: retired})
	}
	if err == nil {
		err = e.database.ValidateCatalogReferences(ctx)
	}
	if err != nil {
		_ = e.database.UpdatePendingDeployment(context.WithoutCancel(ctx), transactionID, "failed", err)
		rollbackErr := e.Complete(context.WithoutCancel(ctx), transactionID, false)
		return errors.Join(err, rollbackErr)
	}
	if err := e.database.UpdatePendingDeployment(ctx, transactionID, "schema_applied", nil); err != nil {
		rollbackErr := e.Complete(context.WithoutCancel(ctx), transactionID, false)
		return errors.Join(err, rollbackErr)
	}
	return nil
}

// Complete finalizes the source switch or restores catalog metadata from the
// still-active package tree. Additive physical structures remain harmless.
func (e *Evaluator) Complete(ctx context.Context, transactionID string, activated bool) error {
	ctx, release, err := e.database.AcquireActivationLock(ctx, transactionID)
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	pending, exists, err := e.database.PendingDeploymentFor(ctx, transactionID)
	if err != nil || !exists {
		return err
	}
	if !activated {
		if err := e.restore(ctx, pending); err != nil {
			_ = e.database.UpdatePendingDeployment(context.WithoutCancel(ctx), transactionID, "rollback_failed", err)
			return err
		}
	}
	return e.database.CompleteDeployment(ctx, transactionID, activated)
}

func (e *Evaluator) restore(ctx context.Context, pending database.PendingDeployment) error {
	packages := make([]string, 0, len(pending.Candidates))
	activePackages := make([]string, 0, len(pending.Candidates))
	for _, candidate := range pending.Candidates {
		packages = append(packages, candidate.PackageID)
		if candidate.PreviousCommit != "" {
			activePackages = append(activePackages, candidate.PackageID)
		}
	}
	definitions := database.DefinitionSet{}
	var err error
	if len(activePackages) > 0 {
		definitions, err = e.database.EvaluateDefinitions(ctx, activePackages)
		if err != nil {
			return err
		}
	}
	_, err = e.database.Synchronize(ctx, definitions.Tables, database.SynchronizationOptions{
		Recovery: true, RetireMissingPackages: packages,
	})
	return err
}

func (e *Evaluator) incrementalItems(ctx context.Context, candidates []deployment.Candidate) ([]evaluationItem, []string, []model.Mount, error) {
	identities := map[string]string{}
	items := []evaluationItem{}
	candidateTableIDs := map[string]bool{}
	retired := map[string]bool{}
	mounts := make([]model.Mount, 0, len(candidates))
	packageIDs := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		packageIDs = append(packageIDs, candidate.PackageID)
		if candidate.Commit == "" {
			continue
		}
		item := workspacepackages.Package{ID: candidate.PackageID, Path: candidate.Root, Valid: true}
		discovered, err := discoverPackage(item, candidate.Commit, identities)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, table := range discovered {
			items = append(items, table)
			candidateTableIDs[table.ExpectedTableID] = true
		}
		parts := strings.Split(candidate.PackageID, "/")
		mounts = append(mounts, model.Mount{
			Source: candidate.Root, Target: packageMountRoot + "/" + parts[0] + "/" + parts[1], ReadOnly: true,
			OwnerScope: "the8020/db", Purpose: "workspace", Persistence: "deployment",
		})
	}
	stored, err := e.database.TableSourcesForPackages(ctx, packageIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, source := range stored {
		if !candidateTableIDs[source.TableID] {
			retired[source.TableID] = true
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ExpectedTableID < items[j].ExpectedTableID })
	retiredIDs := make([]string, 0, len(retired))
	for tableID := range retired {
		retiredIDs = append(retiredIDs, tableID)
	}
	sort.Strings(retiredIDs)
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].Target < mounts[j].Target })
	return items, retiredIDs, mounts, nil
}

func (e *Evaluator) resolvePackages(ctx context.Context, selected []string) ([]workspacepackages.Package, map[string]string, error) {
	e.mu.Lock()
	catalog := e.packages
	e.mu.Unlock()
	var packages []workspacepackages.Package
	var err error
	if len(selected) == 0 {
		packages, err = catalog.ListPackages()
		if err != nil {
			return nil, nil, err
		}
	} else {
		seen := map[string]bool{}
		for _, packageID := range selected {
			packageID = strings.TrimSpace(packageID)
			if seen[packageID] {
				continue
			}
			seen[packageID] = true
			item, resolveErr := catalog.ResolvePackage(packageID)
			if resolveErr != nil {
				return nil, nil, resolveErr
			}
			packages = append(packages, item)
		}
		sort.Slice(packages, func(i, j int) bool { return packages[i].ID < packages[j].ID })
	}
	commits := make(map[string]string, len(packages))
	for _, item := range packages {
		if !item.Valid {
			return nil, nil, fmt.Errorf("package %s is invalid: %s", item.ID, strings.Join(item.ValidationErrors, "; "))
		}
		commit, err := packageCommit(ctx, catalog, item)
		if err != nil {
			return nil, nil, err
		}
		commits[item.ID] = commit
	}
	return packages, commits, nil
}

func packageIDs(packages []workspacepackages.Package) []string {
	result := make([]string, len(packages))
	for index := range packages {
		result[index] = packages[index].ID
	}
	return result
}

func discoverPackages(packages []workspacepackages.Package, commits map[string]string) ([]evaluationItem, error) {
	identities := map[string]string{}
	items := []evaluationItem{}
	for _, item := range packages {
		discovered, err := discoverPackage(item, commits[item.ID], identities)
		if err != nil {
			return nil, err
		}
		items = append(items, discovered...)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ExpectedTableID < items[j].ExpectedTableID })
	return items, nil
}

func packageCommit(ctx context.Context, catalog PackageCatalog, item workspacepackages.Package) (string, error) {
	if activated, ok := catalog.(activatedPackageCatalog); ok {
		return activated.ActivatedPackageCommit(ctx, item.ID)
	}
	if info, err := os.Stat(filepath.Join(item.Path, ".git")); err == nil && info.IsDir() {
		return workspacepackages.CleanRepositoryHead(ctx, item.Path)
	}
	commit, err := workspacepackages.FingerprintPackage(item.Path)
	if err != nil {
		return "", fmt.Errorf("fingerprint package %s: %w", item.ID, err)
	}
	return commit, nil
}

func discoverPackage(item workspacepackages.Package, commit string, identities map[string]string) ([]evaluationItem, error) {
	parts := strings.Split(item.ID, "/")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid package ID %s", item.ID)
	}
	directory := filepath.Join(item.Path, "tables")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read table definitions for %s: %w", item.ID, err)
	}
	result := []evaluationItem{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".ts" || strings.HasSuffix(entry.Name(), "_test.ts") {
			continue
		}
		tableName := strings.TrimSuffix(entry.Name(), ".ts")
		id, err := database.CanonicalTableID(parts[0], parts[1], tableName)
		if err != nil {
			return nil, fmt.Errorf("table %s/%s: %w", item.ID, entry.Name(), err)
		}
		source := item.ID + "/tables/" + entry.Name()
		if previous := identities[id]; previous != "" {
			return nil, fmt.Errorf("canonical table ID collision %s between %s and %s", id, previous, source)
		}
		identities[id] = source
		module := packageMountRoot + "/" + parts[0] + "/" + parts[1] + "/tables/" + entry.Name()
		result = append(result, evaluationItem{Module: module, ExpectedTableID: id, PackageID: item.ID, PackageCommit: commit})
	}
	return result, nil
}
