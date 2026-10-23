package packages

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

const bootstrapRequestedTagConfig = "the8020.requestedTag"

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
			if parsed, parseErr := url.Parse(strings.TrimSpace(source)); parseErr == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
				entry.Source, entry.Commit, entry.Local = source, commit, false
				if requestedTag, tagErr := s.gitValue(ctx, item.Path, "config", "--local", "--get", bootstrapRequestedTagConfig); tagErr == nil {
					requestedTag = strings.TrimSpace(requestedTag)
					if !safeGitTag(requestedTag) {
						return nil, fmt.Errorf("bootstrap package %s has invalid requested tag %q", item.ID, requestedTag)
					}
					tagCommit, resolveErr := s.gitValue(ctx, item.Path, "rev-parse", "--verify", "refs/tags/"+requestedTag+"^{commit}")
					if resolveErr != nil {
						return nil, fmt.Errorf("bootstrap package %s cannot resolve requested tag %s: %w", item.ID, requestedTag, resolveErr)
					}
					if tagCommit != commit {
						return nil, fmt.Errorf("bootstrap package %s requested tag %s resolves to %s instead of %s", item.ID, requestedTag, tagCommit, commit)
					}
					entry.Tag, entry.Commit = requestedTag, ""
				}
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

// ValidateInstalled proves that the node-local checkouts exactly implement
// the package set the shared database marked active.
func (s *Store) ValidateInstalled(ctx context.Context, commits map[string]string) error {
	entries, err := s.index.List(ctx)
	if err != nil {
		return err
	}
	installed := 0
	for _, entry := range entries {
		commit := commits[entry.PackageID]
		// A settled first-install failure retains intent, not an active checkout.
		if commit == "" && entry.ActiveCommit == "" && (entry.State == "desired" || entry.State == "failed") {
			repository, err := s.InspectPackageRepository(ctx, entry.PackageID)
			if err != nil {
				return err
			}
			if repository.Status == "absent" || repository.Status == "empty" {
				continue
			}
		}
		if entry.State != "ready" || entry.ActiveCommit == "" || commit != entry.ActiveCommit {
			return fmt.Errorf("package %s checkout %q does not match ready database commit %q", entry.PackageID, commit, entry.ActiveCommit)
		}
		installed++
	}
	if installed != len(commits) {
		return fmt.Errorf("installed package count %d does not match active database package count %d", len(commits), installed)
	}
	return nil
}
