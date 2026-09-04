package packages

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

func (s *Store) stageInstalled(ctx context.Context, commits map[string]string) ([]Package, error) {
	items, err := s.catalog.ListPackages()
	if err != nil {
		return nil, err
	}
	if len(items) != len(commits) {
		return nil, errors.New("bootstrap package catalog changed during schema synchronization")
	}
	for _, item := range items {
		if !item.Valid {
			return nil, fmt.Errorf("bootstrap package %s is invalid: %s", item.ID, strings.Join(item.ValidationErrors, "; "))
		}
		commit := commits[item.ID]
		if commit == "" {
			return nil, fmt.Errorf("bootstrap package %s has no synchronized commit", item.ID)
		}
		identity, _ := ParsePackageID(item.ID)
		entry := PackageIndex{
			Author: identity.Namespace, Repository: identity.Repository,
			PackageID: item.ID, Local: true,
		}
		if source, sourceErr := s.gitValue(ctx, item.Path, "remote", "get-url", "origin"); sourceErr == nil {
			if parsed, parseErr := url.Parse(strings.TrimSpace(source)); parseErr == nil && parsed.Scheme == "https" {
				entry.Source, entry.Commit, entry.Local = source, commit, false
			}
		}
		if err := validatePackageIndex(&entry); err != nil {
			return nil, fmt.Errorf("bootstrap package %s: %w", item.ID, err)
		}
		if err := s.index.Put(ctx, entry); err != nil {
			return nil, fmt.Errorf("record bootstrap package %s: %w", item.ID, err)
		}
	}
	return items, nil
}

// SynchronizePackageDefinitions stores every service declaration in one
// activated package and retires declarations no longer present.
func (s *Store) SynchronizePackageDefinitions(ctx context.Context, packageID, commit string) ([]string, error) {
	item, err := s.catalog.ResolvePackage(packageID)
	if err != nil {
		return nil, err
	}
	serviceIDs, err := serviceIDsAt(item.Path, packageID)
	if err != nil {
		return nil, err
	}
	definitions, ok := s.state.(ServiceDefinitionStore)
	if !ok {
		return nil, errors.New("service state store cannot synchronize package declarations")
	}
	for _, serviceID := range serviceIDs {
		identity, _ := ParseServiceID(serviceID)
		definition, err := s.readPortableService(identity)
		if err != nil {
			return nil, err
		}
		state, exists, err := s.state.Get(serviceID)
		if err != nil {
			return nil, err
		}
		if !exists {
			state, err = initialDesiredState(s.defaults, definition.Service, serviceID)
			if err != nil {
				return nil, err
			}
		}
		effective, err := calculateEffective(s.defaults, definition.Service, state)
		if err != nil {
			return nil, err
		}
		if err := definitions.InstallDefinition(ctx, definition, state, effective, commit); err != nil {
			return nil, fmt.Errorf("synchronize service %s: %w", serviceID, err)
		}
	}
	if err := definitions.RetirePackage(ctx, packageID, serviceIDs); err != nil {
		return nil, err
	}
	sort.Strings(serviceIDs)
	return serviceIDs, nil
}

// ValidateInstalled proves that the node-local checkouts exactly implement
// the package set the shared database marked active.
func (s *Store) ValidateInstalled(ctx context.Context, commits map[string]string) error {
	entries, err := s.index.List(ctx)
	if err != nil {
		return err
	}
	if len(entries) != len(commits) {
		return fmt.Errorf("installed package count %d does not match database package count %d", len(commits), len(entries))
	}
	for _, entry := range entries {
		commit := commits[entry.PackageID]
		if entry.State != "ready" || entry.ActiveCommit == "" || commit != entry.ActiveCommit {
			return fmt.Errorf("package %s checkout %q does not match ready database commit %q", entry.PackageID, commit, entry.ActiveCommit)
		}
	}
	return nil
}
