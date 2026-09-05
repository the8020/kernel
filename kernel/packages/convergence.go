package packages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// PackageSetUpdate is the targeted local work caused by one published shared
// package-set revision. Source has already converged when Poll returns it.
type PackageSetUpdate struct {
	Revision uint64
	Packages []string
}

// PackageRevisionFollower keeps one node's package checkouts aligned with the
// exact commits published in the shared database. The common no-change path is
// one scalar query; package rows are read only after revision
// advancement.
type PackageRevisionFollower struct {
	store *Store

	mu              sync.Mutex
	revision        uint64
	commits         map[string]string
	pendingRevision uint64
	pendingCommits  map[string]string
}

func NewPackageRevisionFollower(ctx context.Context, store *Store, installed map[string]string) (*PackageRevisionFollower, error) {
	if store == nil {
		return nil, errors.New("package store is required")
	}
	revision, err := store.index.Revision(ctx)
	if err != nil {
		return nil, fmt.Errorf("read package-set revision: %w", err)
	}
	return &PackageRevisionFollower{store: store, revision: revision, commits: cloneCommits(installed)}, nil
}

func (f *PackageRevisionFollower) Poll(ctx context.Context) (PackageSetUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	revision, err := f.store.index.Revision(ctx)
	if err != nil {
		return PackageSetUpdate{}, fmt.Errorf("read package-set revision: %w", err)
	}
	if revision < f.revision {
		return PackageSetUpdate{}, fmt.Errorf("package-set revision moved backwards from %d to %d", f.revision, revision)
	}
	if revision == f.revision {
		return PackageSetUpdate{}, nil
	}
	entries, err := f.store.index.List(ctx)
	if err != nil {
		return PackageSetUpdate{}, fmt.Errorf("load active package set: %w", err)
	}
	byID := make(map[string]PackageIndex, len(entries))
	target := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.State != "ready" || entry.ActiveCommit == "" {
			continue
		}
		byID[entry.PackageID] = entry
		target[entry.PackageID] = entry.ActiveCommit
	}
	changed := changedPackageIDs(f.commits, target)
	for _, packageID := range changed {
		entry, exists := byID[packageID]
		if !exists {
			continue
		}
		if err := f.store.convergePackageCommit(ctx, entry); err != nil {
			return PackageSetUpdate{}, fmt.Errorf("converge package %s: %w", packageID, err)
		}
	}
	update := PackageSetUpdate{Revision: revision, Packages: changed}
	f.pendingRevision, f.pendingCommits = revision, target
	return update, nil
}

// Acknowledge advances local observation only after the caller has completed
// every targeted service action. A failed action therefore retries safely.
func (f *PackageRevisionFollower) Acknowledge(revision uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if revision == 0 || revision != f.pendingRevision || f.pendingCommits == nil {
		return fmt.Errorf("package-set revision %d is not pending", revision)
	}
	f.revision = revision
	f.commits = f.pendingCommits
	f.pendingRevision, f.pendingCommits = 0, nil
	return nil
}

func changedPackageIDs(previous, current map[string]string) []string {
	changed := map[string]bool{}
	for packageID, commit := range previous {
		if current[packageID] != commit {
			changed[packageID] = true
		}
	}
	for packageID, commit := range current {
		if previous[packageID] != commit {
			changed[packageID] = true
		}
	}
	result := make([]string, 0, len(changed))
	for packageID := range changed {
		result = append(result, packageID)
	}
	sort.Strings(result)
	return result
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

// convergePackageCommit switches only node-local source. Schema changes,
// hooks, database publication, and revision advancement belong exclusively to
// the activation coordinator that published this exact commit.
func (s *Store) convergePackageCommit(ctx context.Context, entry PackageIndex) error {
	s.repositoryMu.Lock()
	defer s.repositoryMu.Unlock()
	unlock, err := s.lockPackage(ctx, entry.PackageID)
	if err != nil {
		return err
	}
	defer unlock()
	destination, exists, err := s.packageDestination(entry.PackageID)
	if err != nil {
		return err
	}
	if entry.Local {
		if !exists {
			return errors.New("node-local package checkout does not exist")
		}
		commit, err := s.installedCommit(ctx, destination)
		if err != nil {
			return err
		}
		if commit != entry.ActiveCommit {
			return fmt.Errorf("node-local package commit %q does not match shared active commit %q", commit, entry.ActiveCommit)
		}
		return nil
	}
	if exists {
		commit, err := s.cleanRepositoryHead(ctx, destination)
		if err != nil {
			return err
		}
		if commit == entry.ActiveCommit {
			return finalizePackageDirectory(destination)
		}
	}
	stageRoot, stage, err := s.stageExactCommit(ctx, entry)
	if err != nil {
		return err
	}
	defer os.RemoveAll(stageRoot)
	if _, err := replacePackageDirectory(destination, stage); err != nil {
		return err
	}
	return finalizePackageDirectory(destination)
}

func (s *Store) installedCommit(ctx context.Context, path string) (string, error) {
	if info, err := os.Stat(filepath.Join(path, ".git")); err == nil && info.IsDir() {
		return s.cleanRepositoryHead(ctx, path)
	}
	return FingerprintPackage(path)
}

func (s *Store) stageExactCommit(ctx context.Context, entry PackageIndex) (string, string, error) {
	if strings.TrimSpace(entry.Source) == "" || strings.TrimSpace(entry.ActiveCommit) == "" {
		return "", "", errors.New("remote package source and active commit are required")
	}
	identity, err := ParsePackageID(entry.PackageID)
	if err != nil {
		return "", "", err
	}
	namespaceRoot := filepath.Join(s.packagesRoot, identity.Namespace)
	if err := os.MkdirAll(namespaceRoot, 0o755); err != nil {
		return "", "", err
	}
	stageRoot, err := os.MkdirTemp(namespaceRoot, "."+identity.Repository+"-revision-")
	if err != nil {
		return "", "", err
	}
	fail := func(err error) (string, string, error) {
		_ = os.RemoveAll(stageRoot)
		return "", "", err
	}
	stage := filepath.Join(stageRoot, "repository")
	authentication, err := s.repositoryAuthentication(entry.PackageID, entry.Source)
	if err != nil {
		return fail(err)
	}
	if output, err := s.runGit(ctx, "", authentication, "clone", "--quiet", "--no-checkout", "--origin", "origin", entry.Source, stage); err != nil {
		return fail(fmt.Errorf("clone package: %w: %s", err, cleanGitOutput(output)))
	}
	commit, err := s.gitValue(ctx, stage, "rev-parse", "--verify", entry.ActiveCommit+"^{commit}")
	if err != nil {
		if output, fetchErr := s.runGit(ctx, stage, authentication, "fetch", "--quiet", "origin", entry.ActiveCommit); fetchErr != nil {
			return fail(fmt.Errorf("fetch active package commit: %w: %s", fetchErr, cleanGitOutput(output)))
		}
		commit, err = s.gitValue(ctx, stage, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	}
	if err != nil || !strings.EqualFold(commit, entry.ActiveCommit) {
		return fail(fmt.Errorf("remote does not provide active commit %s", entry.ActiveCommit))
	}
	if output, err := s.runGit(ctx, stage, nil, "checkout", "--quiet", "--detach", commit); err != nil {
		return fail(fmt.Errorf("check out active package commit: %w: %s", err, cleanGitOutput(output)))
	}
	if err := validateStagedPackage(stage); err != nil {
		return fail(err)
	}
	return stageRoot, stage, nil
}
