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
// revision. Every node observes the same mutable package filesystem.
type PackageSetUpdate struct {
	Revision uint64
	Packages []string
	Paths    []string
	Restarts []string
}

// PackageRevisionFollower observes commits published into the shared filesystem.
// It never fetches or replaces node-local source copies. The common no-change path is
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

// NewPackageRevisionFollower captures the published baseline immediately before
// the caller builds its initial full local index. Later publications remain visible.
func NewPackageRevisionFollower(ctx context.Context, store *Store) (*PackageRevisionFollower, error) {
	if store == nil {
		return nil, errors.New("package store is required")
	}
	revision, commits, err := store.index.Published(ctx)
	if err != nil {
		return nil, fmt.Errorf("read published package set: %w", err)
	}
	return &PackageRevisionFollower{store: store, revision: revision, commits: commits}, nil
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
	publishedRevision, target, err := f.store.index.Published(ctx)
	if err != nil {
		return PackageSetUpdate{}, fmt.Errorf("read published package set: %w", err)
	}
	if publishedRevision < revision {
		return PackageSetUpdate{}, fmt.Errorf("package-set revision moved backwards from %d to %d", revision, publishedRevision)
	}
	revision = publishedRevision
	changed := changedPackageIDs(f.commits, target)
	update := PackageSetUpdate{Revision: revision, Packages: changed}
	for _, packageID := range changed {
		paths, err := f.store.changedSourcePaths(ctx, packageID, f.commits[packageID], target[packageID])
		if err != nil {
			return PackageSetUpdate{}, err
		}
		update.Paths = append(update.Paths, paths...)
	}
	f.pendingRevision, f.pendingCommits = revision, target
	return update, nil
}

// Acknowledge advances after targeted work completes or enters its owning retry
// queue. Older completions never consume a newer pending snapshot.
func (f *PackageRevisionFollower) Acknowledge(revision uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if revision > 0 && (revision <= f.revision || revision < f.pendingRevision) {
		return nil
	}
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

func (s *Store) installedCommit(ctx context.Context, path string) (string, error) {
	if info, err := os.Stat(filepath.Join(path, ".git")); err == nil && info.IsDir() {
		return s.cleanRepositoryHead(ctx, path)
	}
	return FingerprintPackage(path)
}

// changedSourcePaths reads bounded Git metadata from the authoritative shared
// repository. Deletions and both sides of renames must intersect old imports.
func (s *Store) changedSourcePaths(ctx context.Context, packageID, previous, current string) ([]string, error) {
	if _, err := ParsePackageID(packageID); err != nil {
		return nil, err
	}
	if current == "" {
		return []string{packageSandboxRoot + "/" + packageID + "/"}, nil
	}
	for _, commit := range []string{previous, current} {
		if commit != "" && !isCommitID(commit) {
			return nil, errors.New("source update commits must be hexadecimal object IDs")
		}
	}
	root := s.packagePath(packageID)
	arguments := []string{"diff", "--name-only", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", previous, current, "--"}
	if previous == "" || current == "" {
		commit := current
		if commit == "" {
			commit = previous
		}
		arguments = []string{"ls-tree", "-r", "--name-only", "-z", commit, "--"}
	}
	output, err := s.runGit(ctx, root, nil, arguments...)
	if err != nil {
		return nil, fmt.Errorf("read changed source paths for %s: %w", packageID, err)
	}
	var paths []string
	for _, name := range strings.Split(output, "\x00") {
		if name == "" {
			continue
		}
		if !filepath.IsLocal(name) {
			return nil, fmt.Errorf("invalid changed source path in %s", packageID)
		}
		paths = append(paths, filepath.ToSlash(filepath.Join(packageSandboxRoot, packageID, name)))
	}
	return paths, nil
}

// ReactToSourceUpdate owns source-update orchestration. The runtime supplies
// observed-import inspection and a generic idempotent soft-restart primitive.
func ReactToSourceUpdate(ctx context.Context, update PackageSetUpdate, matching func(context.Context, []string) ([]string, error), restart func(context.Context, string, uint64) error) error {
	if len(update.Paths) == 0 {
		return nil
	}
	matched, err := matching(ctx, update.Paths)
	if err != nil {
		return err
	}
	var failures error
	for _, id := range uniqueSorted(matched) {
		failures = errors.Join(failures, restart(ctx, id, update.Revision))
	}
	return failures
}
