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
	for position, packageID := range packageIDs {
		if err := ctx.Err(); err != nil {
			for _, pendingID := range packageIDs[position:] {
				i.pending[pendingID] = true
				failure := fmt.Errorf("%s: %w", pendingID, err)
				failures = append(failures, failure)
				diagnostics = append(diagnostics, failure.Error())
			}
			break
		}
		runtimeFailures, err := i.indexServicePackage(ctx, packageID, chain)
		if err != nil {
			i.pending[packageID] = true
			failure := fmt.Errorf("%s: %w", packageID, err)
			failures = append(failures, failure)
			diagnostics = append(diagnostics, failure.Error())
		} else {
			delete(i.pending, packageID)
			applied = append(applied, packageID)
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

func (i *runtimeIndexer) indexServicePackage(ctx context.Context, packageID string, chain []packages.HookDefinition) ([]string, error) {
	entry, err := i.packages.InspectPackageIndex(packageID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil && entry.State != "ready" && entry.State != "retired" {
		return nil, fmt.Errorf("%w: %s", packages.ErrPackageNotReady, packageID)
	}
	scope := struct {
		PackageID     string `json:"package_id"`
		PackageCommit string `json:"package_commit"`
		Active        bool   `json:"active"`
	}{packageID, entry.ActiveCommit, err == nil && entry.State == "ready"}
	state := map[string]any{"services": []webservices.Specification{}}
	record, err := i.packages.RunHookChain(ctx, i.jobs, packageID, "index-services", chain, scope, state, nil)
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
		Services json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, fmt.Errorf("read hook result: %w", err)
	}
	if len(result.Services) == 0 || bytes.Equal(result.Services, []byte("null")) {
		return nil, errors.New("index-services result must contain a services array")
	}
	var specifications []webservices.Specification
	decoder := json.NewDecoder(bytes.NewReader(result.Services))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&specifications); err != nil {
		return nil, fmt.Errorf("decode runtime specifications: %w", err)
	}
	// Activation may have changed source while the ordinary job was running.
	current, currentErr := i.packages.InspectPackageIndex(packageID)
	if currentErr != nil && !errors.Is(currentErr, os.ErrNotExist) {
		return nil, currentErr
	}
	if current.State != entry.State || current.ActiveCommit != entry.ActiveCommit {
		return nil, errors.New("package changed during indexing")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	removed, err := i.services.ReplacePackage(packageID, specifications, record.ReleaseID)
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
