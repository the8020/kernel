// Package evaluator bridges activated package table definitions to sandboxed Deno jobs.
package evaluator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"the8020/kernel/database"
	"the8020/kernel/deployment"
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

type Config struct {
	Packages *workspacepackages.Store
	Jobs     JobRunner
	Database *database.Manager
}

type Evaluator struct {
	packages          *workspacepackages.Store
	jobs              JobRunner
	database          *database.Manager
	mu                sync.Mutex
	pending           []string
	deploymentContext context.Context
	releaseDeployment func()
}

func New(config Config) (*Evaluator, error) {
	if config.Packages == nil || config.Jobs == nil || config.Database == nil {
		return nil, errors.New("package store, job runner, and database are required")
	}
	return &Evaluator{packages: config.Packages, jobs: config.Jobs, database: config.Database}, nil
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

// InspectSources checks catalog source identities without evaluating TypeScript.
// The deployed-table list therefore scales with packages, not table evaluation.
func (e *Evaluator) InspectSources(ctx context.Context, sources []database.TableSource) (map[string]database.SourceStatus, error) {
	result := make(map[string]database.SourceStatus, len(sources))
	byPackage := map[string][]database.TableSource{}
	for _, source := range sources {
		byPackage[source.SourcePackage] = append(byPackage[source.SourcePackage], source)
	}
	packageIDs := make([]string, 0, len(byPackage))
	for packageID := range byPackage {
		packageIDs = append(packageIDs, packageID)
	}
	sort.Strings(packageIDs)
	for _, packageID := range packageIDs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item, err := e.packages.ResolvePackage(packageID)
		if err != nil {
			for _, source := range byPackage[packageID] {
				result[source.TableID] = database.SourceStatus{Error: err.Error()}
			}
			continue
		}
		commit, err := e.packageCommit(ctx, item)
		if err != nil {
			for _, source := range byPackage[packageID] {
				result[source.TableID] = database.SourceStatus{Error: err.Error()}
			}
			continue
		}
		discovered, err := discoverPackage(item, commit, map[string]string{})
		if err != nil {
			for _, source := range byPackage[packageID] {
				result[source.TableID] = database.SourceStatus{CurrentCommit: commit, Error: err.Error()}
			}
			continue
		}
		modules := make(map[string]string, len(discovered))
		for _, table := range discovered {
			modules[table.ExpectedTableID] = table.Module
		}
		for _, source := range byPackage[packageID] {
			module, exists := modules[source.TableID]
			result[source.TableID] = database.SourceStatus{
				Exists:        exists && module == source.SourceModule,
				CurrentCommit: commit,
			}
		}
	}
	return result, nil
}

// InspectDefinition evaluates exactly one deployed source module for table detail.
func (e *Evaluator) InspectDefinition(ctx context.Context, source database.TableSource) (*database.EvaluatedTable, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	item, err := e.packages.ResolvePackage(source.SourcePackage)
	if err != nil {
		return nil, err
	}
	commit, err := e.packageCommit(ctx, item)
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
	return e.synchronizeAll(ctx, resume, false)
}

// RecoverAll aligns an unfinished deployment to the package tree that is
// actually active after a crash. Only rollback-safe reversals are enabled.
func (e *Evaluator) RecoverAll(ctx context.Context) ([]database.SynchronizationResult, error) {
	return e.synchronizeAll(ctx, true, true)
}

func (e *Evaluator) synchronizeAll(ctx context.Context, resume, recovery bool) ([]database.SynchronizationResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	lockedContext, release, err := e.database.AcquireDeploymentLock(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = lockedContext
	if recovery {
		if _, exists, err := e.database.PendingDeployment(ctx); err != nil {
			return nil, err
		} else if !exists {
			return nil, errors.New("database schema deployment is not pending")
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
	if recovery {
		if err := e.database.CompleteDeployment(ctx, false); err != nil {
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
		OwnerID: "the8020/db", Input: evaluationRequest{PackageRoot: packageMountRoot, Tables: batch},
		GroupKey: "database-table-evaluator", Namespace: "the8020", Timeout: 2 * time.Minute,
		Parallelism: 1, Reuse: &reuse, ReleaseID: fingerprint, DatabaseAccess: "none", CheckModules: modules, Mounts: mounts,
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

// Prepare evaluates only directly changed/new tables and tables whose stored
// static dependency closure contains a changed package file.
func (e *Evaluator) Prepare(ctx context.Context, candidates []deployment.Candidate) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.pending) != 0 {
		return errors.New("schema activation is already prepared")
	}
	if len(candidates) == 0 {
		return nil
	}
	// The service plane is unavailable until the first complete synchronization.
	// Let an operator replace a broken package, then retry that bounded full load.
	if !e.database.Status().Initialized {
		return nil
	}
	lockedContext, release, err := e.database.AcquireDeploymentLock(ctx)
	if err != nil {
		return err
	}
	keepLock := false
	defer func() {
		if !keepLock {
			release()
		}
	}()
	ctx = lockedContext
	databaseCandidates := make([]database.DeploymentCandidate, len(candidates))
	for index, candidate := range candidates {
		if _, err := workspacepackages.ParsePackageID(candidate.PackageID); err != nil || !filepath.IsAbs(candidate.Root) || candidate.Commit == "" {
			return fmt.Errorf("invalid schema candidate %s", candidate.PackageID)
		}
		databaseCandidates[index] = database.DeploymentCandidate{PackageID: candidate.PackageID, CandidateCommit: candidate.Commit}
	}
	pending, err := e.database.BeginDeployment(ctx, databaseCandidates)
	if err != nil {
		return err
	}
	items, retired, rollbackPackages, mounts, err := e.incrementalItems(ctx, candidates, pending)
	if err == nil {
		err = e.database.UpdatePendingDeployment(ctx, "evaluating", nil)
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
		_ = e.database.UpdatePendingDeployment(context.WithoutCancel(ctx), "failed", err)
		rollbackErr := e.rollback(ctx, rollbackPackages)
		return errors.Join(err, rollbackErr)
	}
	if err := e.database.UpdatePendingDeployment(ctx, "schema_applied", nil); err != nil {
		rollbackErr := e.rollback(ctx, rollbackPackages)
		return errors.Join(err, rollbackErr)
	}
	e.pending = rollbackPackages
	e.deploymentContext = lockedContext
	e.releaseDeployment = release
	keepLock = true
	return nil
}

// Complete finalizes the source switch or restores catalog metadata from the
// still-active package tree. Additive physical structures remain harmless.
func (e *Evaluator) Complete(ctx context.Context, activated bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.releaseDeployment != nil {
		release := e.releaseDeployment
		defer func() {
			release()
			e.releaseDeployment = nil
			e.deploymentContext = nil
		}()
		completionContext, cancel := context.WithTimeout(context.WithoutCancel(e.deploymentContext), 2*time.Minute)
		defer cancel()
		ctx = completionContext
	}
	if len(e.pending) == 0 {
		return nil
	}
	if !activated {
		if err := e.restore(ctx, e.pending); err != nil {
			_ = e.database.UpdatePendingDeployment(context.WithoutCancel(ctx), "rollback_failed", err)
			return err
		}
	}
	if err := e.database.CompleteDeployment(ctx, activated); err != nil {
		return err
	}
	e.pending = nil
	return nil
}

func (e *Evaluator) rollback(ctx context.Context, packages []string) error {
	if len(packages) > 0 {
		if err := e.restore(ctx, packages); err != nil {
			_ = e.database.UpdatePendingDeployment(context.WithoutCancel(ctx), "rollback_failed", err)
			e.pending = packages
			return err
		}
	}
	return e.database.CompleteDeployment(ctx, false)
}

func (e *Evaluator) restore(ctx context.Context, packages []string) error {
	lockedContext, release, err := e.database.AcquireDeploymentLock(ctx)
	if err != nil {
		return err
	}
	defer release()
	pending, exists, err := e.database.PendingDeployment(lockedContext)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("database schema deployment is not pending")
	}
	newPackages := map[string]bool{}
	for _, candidate := range pending.Candidates {
		if candidate.PreviousCommit == "" {
			newPackages[candidate.PackageID] = true
		}
	}
	activePackages := make([]string, 0, len(packages))
	for _, packageID := range packages {
		if !newPackages[packageID] {
			activePackages = append(activePackages, packageID)
		}
	}
	definitions := database.DefinitionSet{}
	if len(activePackages) > 0 {
		definitions, err = e.database.EvaluateDefinitions(lockedContext, activePackages)
		if err != nil {
			return err
		}
	}
	_, err = e.database.Synchronize(lockedContext, definitions.Tables, database.SynchronizationOptions{
		Recovery: true, RetireMissingPackages: packages,
	})
	return err
}

func (e *Evaluator) incrementalItems(ctx context.Context, candidates []deployment.Candidate, pending database.PendingDeployment) ([]evaluationItem, []string, []string, []model.Mount, error) {
	identities := map[string]string{}
	itemsByID := map[string]evaluationItem{}
	candidateTables := map[string]evaluationItem{}
	changedModules := []string{}
	retired := map[string]bool{}
	rollback := map[string]bool{}
	mounts := make([]model.Mount, 0, len(candidates))
	packageIDs := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		packageIDs = append(packageIDs, candidate.PackageID)
		rollback[candidate.PackageID] = true
		item := workspacepackages.Package{ID: candidate.PackageID, Path: candidate.Root, Valid: true}
		discovered, err := discoverPackage(item, candidate.Commit, identities)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		for _, table := range discovered {
			candidateTables[table.Module] = table
		}
		parts := strings.Split(candidate.PackageID, "/")
		mounts = append(mounts, model.Mount{
			Source: candidate.Root, Target: packageMountRoot + "/" + parts[0] + "/" + parts[1], ReadOnly: true,
			OwnerScope: "the8020/db", Purpose: "workspace", Persistence: "deployment",
		})
		files, err := changedFiles(ctx, candidate.Root, pending.PreviousPackageCommits[candidate.PackageID], candidate.Commit)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("changed files for %s: %w", candidate.PackageID, err)
		}
		prefix := packageMountRoot + "/" + parts[0] + "/" + parts[1] + "/"
		for _, file := range files {
			changedModules = append(changedModules, prefix+filepath.ToSlash(file))
		}
	}
	stored, err := e.database.TableSourcesForPackages(ctx, packageIDs)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	storedModules := map[string]database.TableSource{}
	for _, source := range stored {
		storedModules[source.SourceModule] = source
		if _, exists := candidateTables[source.SourceModule]; !exists {
			retired[source.TableID] = true
		}
	}
	changed := map[string]bool{}
	for _, module := range changedModules {
		changed[module] = true
	}
	for module, item := range candidateTables {
		if changed[module] || storedModules[module].TableID == "" {
			itemsByID[item.ExpectedTableID] = item
		}
	}
	dependent, err := e.database.TableSourcesForDependencies(ctx, changedModules)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for _, source := range dependent {
		if retired[source.TableID] {
			continue
		}
		rollback[source.SourcePackage] = true
		if item, exists := candidateTables[source.SourceModule]; exists {
			itemsByID[source.TableID] = item
			continue
		}
		commit := pending.CandidatePackageCommits[source.SourcePackage]
		if commit == "" {
			commit = source.SourceCommit
		}
		itemsByID[source.TableID] = evaluationItem{
			Module: source.SourceModule, ExpectedTableID: source.TableID,
			PackageID: source.SourcePackage, PackageCommit: commit,
		}
	}
	items := make([]evaluationItem, 0, len(itemsByID))
	for _, item := range itemsByID {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ExpectedTableID < items[j].ExpectedTableID })
	retiredIDs := make([]string, 0, len(retired))
	for tableID := range retired {
		retiredIDs = append(retiredIDs, tableID)
	}
	sort.Strings(retiredIDs)
	rollbackPackages := make([]string, 0, len(rollback))
	for packageID := range rollback {
		rollbackPackages = append(rollbackPackages, packageID)
	}
	sort.Strings(rollbackPackages)
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].Target < mounts[j].Target })
	return items, retiredIDs, rollbackPackages, mounts, nil
}

func changedFiles(ctx context.Context, root, previous, candidate string) ([]string, error) {
	if previous != "" {
		command := exec.CommandContext(ctx, "git", "-C", root, "diff", "--name-only", "-z", "--no-renames", previous, candidate, "--")
		if output, err := command.Output(); err == nil {
			return cleanRelativePaths(strings.Split(string(output), "\x00"))
		}
	}
	command := exec.CommandContext(ctx, "git", "-C", root, "ls-tree", "-r", "--name-only", "-z", candidate)
	if output, err := command.Output(); err == nil {
		return cleanRelativePaths(strings.Split(string(output), "\x00"))
	}
	paths := []string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cleanRelativePaths(paths)
}

func cleanRelativePaths(paths []string) ([]string, error) {
	result := []string{}
	seen := map[string]bool{}
	for _, path := range paths {
		if path == "" {
			continue
		}
		path = filepath.ToSlash(filepath.Clean(path))
		if path == "." || strings.HasPrefix(path, "../") || filepath.IsAbs(path) {
			return nil, fmt.Errorf("invalid changed package path %q", path)
		}
		if !seen[path] {
			seen[path] = true
			result = append(result, path)
		}
	}
	sort.Strings(result)
	return result, nil
}

func (e *Evaluator) resolvePackages(ctx context.Context, selected []string) ([]workspacepackages.Package, map[string]string, error) {
	var packages []workspacepackages.Package
	var err error
	if len(selected) == 0 {
		packages, err = e.packages.ListPackages()
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
			item, resolveErr := e.packages.ResolvePackage(packageID)
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
		commit, err := e.packageCommit(ctx, item)
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

func (e *Evaluator) packageCommit(ctx context.Context, item workspacepackages.Package) (string, error) {
	command := exec.CommandContext(ctx, "git", "-C", item.Path, "rev-parse", "--verify", "HEAD^{commit}")
	if output, err := command.Output(); err == nil {
		return strings.TrimSpace(string(output)), nil
	}
	hash := sha256.New()
	walkErr := filepath.WalkDir(item.Path, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(item.Path, path)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(hash, filepath.ToSlash(relative)+"\x00"); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		return errors.Join(copyErr, closeErr)
	})
	if walkErr != nil {
		return "", fmt.Errorf("fingerprint package %s: %w", item.ID, walkErr)
	}
	return "filesystem-" + hex.EncodeToString(hash.Sum(nil)), nil
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
