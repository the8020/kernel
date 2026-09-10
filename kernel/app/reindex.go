package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"the8020/kernel/cbus/core"
	"the8020/kernel/cbus/discovery"
	"the8020/kernel/packages"
	"the8020/kernel/webservices"
)

// runtimeIndexer is the shared boot, activation, convergence, and CBus entry point.
type runtimeIndexer struct {
	mu       sync.Mutex
	handlers interface {
		ReindexHandlers(context.Context, ...string) (packages.HandlerReport, error)
	}
	commands interface {
		Reindex(context.Context, ...string) (discovery.Report, error)
	}
	packages       *packages.Store
	jobs           packages.JobRunner
	services       *webservices.Index
	runtime        targetedServiceReconciler
	chainRevision  string
	pending        map[string]bool
	running        map[string]*serviceIndexRun
	background     context.Context
	cancel         context.CancelFunc
	closed         bool
	backgroundJobs int
	backgroundWait sync.WaitGroup
	reportFailure  func(error)
}

type serviceIndexRun struct {
	newer bool
	done  chan struct{}
}

type indexPublicationError struct{ failures []error }

func (e *indexPublicationError) Error() string {
	return "service index updates were not applied: " + errors.Join(e.failures...).Error()
}

func (e *indexPublicationError) Unwrap() []error { return e.failures }

func (i *runtimeIndexer) Reindex(ctx context.Context, packageIDs []string) (core.Result, error) {
	return i.reindex(ctx, packageIDs, false)
}

// Queue refreshes native declarations now and retains service work in this
// indexer's existing per-package pending set. Its jobs outlive a polling call.
func (i *runtimeIndexer) Queue(ctx context.Context, packageIDs []string) (core.Result, error) {
	return i.reindex(ctx, packageIDs, true)
}

func (i *runtimeIndexer) reindex(ctx context.Context, packageIDs []string, background bool) (core.Result, error) {
	handlers, err := i.handlers.ReindexHandlers(ctx, packageIDs...)
	if err != nil {
		return nil, fmt.Errorf("index package handlers: %w", err)
	}
	commands, err := i.commands.Reindex(ctx, packageIDs...)
	if err != nil {
		return nil, fmt.Errorf("index package commands: %w", err)
	}
	result := core.Result{
		"revision": commands.Revision, "packages": commands.Packages,
		"commands": commands.Commands, "diagnostics": commands.Diagnostics,
		"events": handlers.Events, "hooks": handlers.Hooks,
	}
	if i.services == nil {
		return result, nil
	}
	applied, failures, err := i.indexServices(ctx, packageIDs, background)
	result["indexed_packages"], result["service_diagnostics"] = applied, failures
	return result, err
}

func (i *runtimeIndexer) indexServices(ctx context.Context, packageIDs []string, background bool) ([]string, []string, error) {
	var applied, diagnostics []string
	var failures []error
	fail := func(packageID string, err error) {
		failure := fmt.Errorf("%s: %w", packageID, err)
		failures = append(failures, failure)
		diagnostics = append(diagnostics, failure.Error())
	}
refresh:
	chain := i.packages.Hooks("index-services")
	encoded, err := json.Marshal(chain)
	if err != nil {
		return nil, nil, err
	}
	digest := sha256.Sum256(encoded)
	revision := hex.EncodeToString(digest[:])
	i.mu.Lock()
	previousRevision := i.chainRevision
	i.mu.Unlock()
	// A changed provider can affect every fragment, even if its own package owns
	// no services. Ordinary scoped edits keep their original selection.
	if len(packageIDs) == 0 || previousRevision != "" && revision != previousRevision {
		entries, err := i.packages.ListPackageIndexes()
		if err != nil {
			return nil, nil, err
		}
		packageIDs = i.services.PackageIDs()
		for _, entry := range entries {
			if entry.State == "ready" {
				packageIDs = append(packageIDs, entry.PackageID)
			}
		}
	}
	packageIDs = slices.Clone(packageIDs)
	slices.Sort(packageIDs)
	packageIDs = slices.Compact(packageIDs)
	waiting := map[string]<-chan struct{}{}
	i.mu.Lock()
	if i.pending == nil {
		i.pending = map[string]bool{}
		i.running = map[string]*serviceIndexRun{}
	}
	changed := i.chainRevision != previousRevision && i.chainRevision != revision
	claimed := make([]string, 0, len(packageIDs))
	for _, id := range packageIDs {
		i.pending[id] = true
		if running := i.running[id]; running != nil {
			running.newer = true
			if background {
				fail(id, errors.New("indexing is already running; newer request retained"))
			} else {
				waiting[id] = running.done
			}
		} else if changed {
			fail(id, errors.New("index provider changed while preparing refresh; request retained"))
		} else {
			i.running[id] = &serviceIndexRun{done: make(chan struct{})}
			claimed = append(claimed, id)
		}
	}
	if !changed {
		i.chainRevision = revision
	}
	i.mu.Unlock()
	completed := map[string]bool{}
	release := func() {
		i.mu.Lock()
		defer i.mu.Unlock()
		for _, id := range claimed {
			running := i.running[id]
			if completed[id] && !running.newer {
				delete(i.pending, id)
			}
			delete(i.running, id)
			close(running.done)
		}
	}
	run := func(ctx context.Context) ([]string, []string, error) {
		defer release()
		scope := serviceIndexScope{Packages: []serviceIndexPackage{}}
		for _, packageID := range claimed {
			entry, err := i.packages.InspectPackageIndex(packageID)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				fail(packageID, err)
				continue
			}
			if err == nil && entry.State != "ready" && entry.State != "retired" {
				fail(packageID, fmt.Errorf("%w: %s", packages.ErrPackageNotReady, packageID))
				continue
			}
			scope.Packages = append(scope.Packages, serviceIndexPackage{PackageID: packageID, PackageCommit: entry.ActiveCommit, Active: err == nil && entry.State == "ready", state: entry.State})
		}
		fragments, batchErr := i.runServiceIndex(ctx, scope, chain)
		for _, selected := range scope.Packages {
			if batchErr != nil {
				fail(selected.PackageID, batchErr)
				continue
			}
			i.mu.Lock()
			providerChanged := i.chainRevision != revision
			i.mu.Unlock()
			if providerChanged {
				fail(selected.PackageID, errors.New("index provider changed during indexing"))
				continue
			}
			runtimeFailures, err := i.publishServicePackage(ctx, selected, fragments[selected.PackageID])
			if err != nil {
				fail(selected.PackageID, err)
			} else {
				completed[selected.PackageID] = true
				applied = append(applied, selected.PackageID)
				diagnostics = append(diagnostics, runtimeFailures...)
			}
		}
		if len(failures) > 0 {
			return applied, diagnostics, &indexPublicationError{failures}
		}
		return applied, diagnostics, nil
	}
	if !background {
		// Publish available packages before waiting for selected busy owners.
		// Release all claims first; neither a package nor the mutex spans a wait.
		_, _, err := run(ctx)
		if len(waiting) == 0 {
			return applied, diagnostics, err
		}
		packageIDs = make([]string, 0, len(waiting))
		for id, done := range waiting {
			packageIDs = append(packageIDs, id)
			select {
			case <-done:
			case <-ctx.Done():
				fail(id, ctx.Err())
				return applied, diagnostics, &indexPublicationError{failures}
			}
		}
		goto refresh
	}
	if len(claimed) == 0 {
		return run(ctx)
	}
	i.mu.Lock()
	var admissionErr error
	switch {
	case i.closed || i.background == nil || i.background.Err() != nil:
		admissionErr = errors.New("background indexing is unavailable; request retained")
	case ctx.Err() != nil:
		admissionErr = ctx.Err()
	case i.backgroundJobs >= 16:
		// ponytail: at most 16 background batches; raise only after measured
		// demand. Excess owners stay pending for the ordinary monitor cadence.
		admissionErr = errors.New("background indexing capacity reached; request retained")
	default:
		i.backgroundJobs++
		i.backgroundWait.Add(1)
	}
	i.mu.Unlock()
	if admissionErr != nil {
		release()
		for _, id := range claimed {
			fail(id, admissionErr)
		}
		return nil, diagnostics, &indexPublicationError{failures}
	}
	queuedDiagnostics := slices.Clone(diagnostics)
	var queuedError error
	if len(failures) > 0 {
		queuedError = &indexPublicationError{slices.Clone(failures)}
	}
	go func() {
		defer i.backgroundWait.Done()
		defer func() {
			i.mu.Lock()
			i.backgroundJobs--
			i.mu.Unlock()
		}()
		jobContext, cancel := context.WithTimeout(i.background, 5*time.Minute)
		defer cancel()
		_, _, err := run(jobContext)
		if i.reportFailure != nil {
			i.reportFailure(err)
		}
	}()
	return nil, queuedDiagnostics, queuedError
}

// RetryPending uses the existing monitor cadence and cached handler chain.
// Accepted fragments and native declarations are not rediscovered on retries.
func (i *runtimeIndexer) RetryPending(ctx context.Context) error {
	return i.retryPending(ctx, false)
}

func (i *runtimeIndexer) QueuePending(ctx context.Context) error {
	return i.retryPending(ctx, true)
}

func (i *runtimeIndexer) retryPending(ctx context.Context, background bool) error {
	i.mu.Lock()
	ids := make([]string, 0, len(i.pending))
	for id := range i.pending {
		if _, running := i.running[id]; !running {
			ids = append(ids, id)
		}
	}
	i.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	_, _, err := i.indexServices(ctx, ids, background)
	return err
}

func (i *runtimeIndexer) Close() error {
	i.mu.Lock()
	i.closed = true
	if i.cancel != nil {
		i.cancel()
	}
	i.mu.Unlock()
	i.backgroundWait.Wait()
	return nil
}

type serviceIndexPackage struct {
	state         string
	PackageID     string `json:"package_id"`
	PackageCommit string `json:"package_commit"`
	Active        bool   `json:"active"`
}

type serviceIndexScope struct {
	Packages []serviceIndexPackage `json:"packages"`
}

func (i *runtimeIndexer) runServiceIndex(ctx context.Context, scope serviceIndexScope, chain []packages.HookDefinition) (map[string]json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(scope.Packages) == 0 {
		return nil, nil
	}
	drafts := make(map[string]any, len(scope.Packages))
	for _, selected := range scope.Packages {
		drafts[selected.PackageID] = map[string]any{"services": []webservices.Specification{}}
	}
	record, err := i.packages.RunHookChain(ctx, i.jobs, "hooks", "index-services", chain, scope, map[string]any{"packages": drafts}, nil)
	if err != nil {
		return nil, err
	}
	if record.State != "SUCCEEDED" && record.State != "IDLE" {
		return nil, fmt.Errorf("index-services job %s: %s", record.State, record.Failure)
	}
	encoded, err := json.Marshal(record.Result)
	if err != nil {
		return nil, fmt.Errorf("encode hook result: %w", err)
	}
	var result struct {
		Packages map[string]json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, fmt.Errorf("read hook result: %w", err)
	}
	if len(result.Packages) != len(drafts) {
		return nil, errors.New("index-services result must contain every selected package")
	}
	for id := range result.Packages {
		if _, ok := drafts[id]; !ok {
			return nil, fmt.Errorf("index-services result contains unselected package %s", id)
		}
	}
	return result.Packages, nil
}

func (i *runtimeIndexer) publishServicePackage(ctx context.Context, selected serviceIndexPackage, fragment json.RawMessage) ([]string, error) {
	packageID := selected.PackageID
	var result struct {
		Services []webservices.Specification `json:"services"`
		Error    *string                     `json:"error,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(fragment))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("decode runtime specifications: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("index-services provider failed: %s", *result.Error)
	}
	if result.Services == nil {
		return nil, errors.New("index-services package result must contain a services array")
	}
	specifications := result.Services
	// Activation may have changed source while the ordinary job was running.
	current, currentErr := i.packages.InspectPackageIndex(packageID)
	if currentErr != nil && !errors.Is(currentErr, os.ErrNotExist) {
		return nil, currentErr
	}
	if current.State != selected.state || current.ActiveCommit != selected.PackageCommit {
		return nil, errors.New("package changed during indexing")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	removed, err := i.services.ReplacePackage(packageID, specifications)
	if err != nil {
		return nil, err
	}
	var diagnostics []string
	if i.runtime != nil {
		for _, serviceID := range removed {
			if err := i.runtime.Retire(ctx, serviceID); err != nil {
				diagnostics = append(diagnostics, fmt.Sprintf("%s: fragment accepted; retire %s: %v", packageID, serviceID, err))
			}
		}
		for _, spec := range specifications {
			if _, err := i.runtime.Reconcile(ctx, spec.ServiceID); err != nil {
				diagnostics = append(diagnostics, fmt.Sprintf("%s: fragment accepted; reconcile %s: %v", packageID, spec.ServiceID, err))
			}
		}
	}
	return diagnostics, nil
}
