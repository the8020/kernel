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
	packages      *packages.Store
	jobs          packages.JobRunner
	services      *webservices.Index
	runtime       targetedServiceReconciler
	chainRevision string
	pending       map[string]bool
}

type indexPublicationError struct{ failures []error }

func (e *indexPublicationError) Error() string {
	return "service index updates were not applied: " + errors.Join(e.failures...).Error()
}

func (e *indexPublicationError) Unwrap() []error { return e.failures }

func (i *runtimeIndexer) Reindex(ctx context.Context, packageIDs []string) (core.Result, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
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
	applied, failures, err := i.indexServices(ctx, packageIDs)
	result["indexed_packages"], result["service_diagnostics"] = applied, failures
	return result, err
}

func (i *runtimeIndexer) indexServices(ctx context.Context, packageIDs []string) ([]string, []string, error) {
	chain := i.packages.Hooks("index-services")
	encoded, err := json.Marshal(chain)
	if err != nil {
		return nil, nil, err
	}
	digest := sha256.Sum256(encoded)
	revision := hex.EncodeToString(digest[:])
	// A changed provider can affect every fragment, even if its own package owns
	// no services. Ordinary scoped edits keep their original selection.
	if len(packageIDs) == 0 || i.chainRevision != "" && revision != i.chainRevision {
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
	i.chainRevision = revision
	if i.pending == nil {
		i.pending = map[string]bool{}
	}
	var applied, diagnostics []string
	var failures []error
	fail := func(packageID string, err error) {
		i.pending[packageID] = true
		failure := fmt.Errorf("%s: %w", packageID, err)
		failures = append(failures, failure)
		diagnostics = append(diagnostics, failure.Error())
	}
	scope := serviceIndexScope{Packages: []serviceIndexPackage{}}
	for _, packageID := range packageIDs {
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
		runtimeFailures, err := i.publishServicePackage(ctx, selected, fragments[selected.PackageID])
		if err != nil {
			fail(selected.PackageID, err)
		} else {
			delete(i.pending, selected.PackageID)
			applied = append(applied, selected.PackageID)
			diagnostics = append(diagnostics, runtimeFailures...)
		}
	}
	if len(failures) > 0 {
		return applied, diagnostics, &indexPublicationError{failures}
	}
	return applied, diagnostics, nil
}

// RetryPending uses the existing monitor cadence and cached handler chain.
// Accepted fragments and native declarations are not rediscovered on retries.
func (i *runtimeIndexer) RetryPending(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.pending) == 0 {
		return nil
	}
	ids := make([]string, 0, len(i.pending))
	for id := range i.pending {
		ids = append(ids, id)
	}
	_, _, err := i.indexServices(ctx, ids)
	return err
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
