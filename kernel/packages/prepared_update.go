package packages

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type preparedRoute struct {
	PackageID string `json:"package_id"`
	Source    string `json:"source"`
	Skip      bool   `json:"skip"`
}

func (s *Store) preparedRoutes(build string) (map[string]preparedRoute, error) {
	root := filepath.Join(s.workspaceRoot, "node/kernel/update")
	var state struct{ Phase, Target string }
	read := func(name string, target any) error {
		file, err := os.Open(filepath.Join(root, name))
		if err != nil {
			return err
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, manifestLimit+1))
		if err != nil {
			return err
		}
		if len(data) > manifestLimit {
			return errors.New("oversized update metadata")
		}
		return json.Unmarshal(data, target)
	}
	if err := read("state.json", &state); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if state.Phase != "installed" {
		return nil, nil
	}
	var receipt struct {
		Target, Commit, Manifest string
		Routes                   []preparedRoute
	}
	if err := read("prerequisites.json", &receipt); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if receipt.Routes == nil || state.Target != receipt.Target || build != receipt.Target+"+"+receipt.Commit {
		return nil, nil
	}
	items, err := s.RecommendedPackages()
	if err != nil {
		return nil, err
	}
	if len(receipt.Routes) != len(items) {
		return nil, errors.New("prepared selection differs from installed manifest")
	}
	content, err := os.ReadFile(filepath.Join(s.workspaceRoot, "node/kernel/scripts/bootstrap-packages.toml"))
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(content)) != receipt.Manifest {
		return nil, errors.New("prepared recommendation manifest changed")
	}
	routes := make(map[string]preparedRoute, len(items))
	for _, route := range receipt.Routes {
		if !route.Skip {
			source, author, repository, err := normalizePackageSource(route.Source)
			if err != nil || author+"/"+repository != route.PackageID {
				return nil, errors.New("invalid prepared transport")
			}
			route.Source = source
		}
		routes[route.PackageID] = route
	}
	for _, item := range items {
		if _, ok := routes[item.ID]; !ok {
			return nil, fmt.Errorf("missing prepared route for %s", item.ID)
		}
	}
	return routes, nil
}

// Restore only previously published bytes, before the ordinary full-catalog startup check.
func (s *Store) RestorePreparedBaseline(ctx context.Context, build string) error {
	routes, err := s.preparedRoutes(build)
	if err != nil || len(routes) == 0 {
		return err
	}
	items, err := s.RecommendedPackages()
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if !routes[item.ID].Skip {
			ids = append(ids, item.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	unlock, err := LockSources(ctx, s.packagesRoot, ids)
	if err != nil {
		return err
	}
	defer unlock()
	for _, item := range items {
		route, ok := routes[item.ID]
		if !ok || route.Skip {
			continue
		}
		entry, found, err := s.runtimeIndex.Get(ctx, item.ID)
		if err != nil {
			return err
		}
		if !found || entry.Local || entry.State != "ready" || !fullCommit(entry.ActiveCommit) || (entry.Source != item.Source && entry.Source != route.Source) {
			continue
		}
		path, exists, err := s.packageDestination(item.ID)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if err := s.restorePublishedTree(ctx, path, entry.ActiveCommit); err != nil {
			return fmt.Errorf("%s: %w", item.ID, err)
		}
	}
	return nil
}

func (s *Store) validatePreparedCheckout(ctx context.Context, path string) error {
	metadata := filepath.Join(path, ".git")
	if err := filepath.WalkDir(metadata, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 || name == metadata && !entry.IsDir() || name == filepath.Join(metadata, "commondir") || name == filepath.Join(metadata, "worktrees") || name == filepath.Join(metadata, "objects/info/alternates") {
			return errors.New("prepared update requires self-contained Git metadata")
		}
		return nil
	}); err != nil {
		return err
	}
	if top, err := s.gitValue(ctx, path, "rev-parse", "--show-toplevel"); err != nil || top != path {
		return errors.New("prepared update requires a self-contained working tree")
	}
	return nil
}

func (s *Store) restorePublishedTree(ctx context.Context, path, commit string) (err error) {
	if err := s.validatePreparedCheckout(ctx, path); err != nil {
		return err
	}
	metadata := filepath.Join(path, ".git")
	stage, err := os.MkdirTemp(filepath.Dir(path), ".update-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(stage)) }()
	tree := filepath.Join(stage, "tree")
	if _, err := s.runGit(ctx, "", nil, "init", "--quiet", "--template=", tree); err != nil {
		return err
	}
	rootInfo, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := os.Chmod(tree, rootInfo.Mode().Perm()); err != nil {
		return err
	}
	env := []string{"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + filepath.Join(metadata, "objects"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_REPLACE_OBJECTS=1"}
	if _, err := s.runGit(ctx, tree, env, "-c", "core.hooksPath=/dev/null", "checkout", "--quiet", "--detach", commit); err != nil {
		return err
	}
	gitDir := filepath.Join(tree, ".git")
	priorHead, err := s.gitValue(ctx, path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err
	}
	if priorHead == commit {
		cleanHead, err := s.cleanRepositoryHead(ctx, path)
		if err != nil && !errors.Is(err, errDirtyRepository) {
			return err
		}
		if cleanHead == commit {
			extra, err := s.runGit(ctx, tree, env, "--work-tree", path, "ls-files", "--others")
			if err != nil {
				return err
			}
			if extra == "" {
				if _, err := s.runGit(ctx, tree, env, "--work-tree", path, "diff", "--quiet", commit, "--"); err == nil {
					return nil
				}
			}
		}
	}
	if err := os.Rename(gitDir, filepath.Join(stage, "clean")); err != nil {
		return err
	}
	if err := copyRepository(metadata, gitDir); err != nil {
		return err
	}
	for _, name := range []string{"HEAD", "index"} {
		if name == "HEAD" && priorHead == commit {
			continue
		}
		if err := os.Rename(filepath.Join(stage, "clean", name), filepath.Join(gitDir, name)); err != nil {
			return err
		}
	}
	if head, err := s.cleanRepositoryHead(ctx, tree); err != nil || head != commit {
		return fmt.Errorf("could not verify restored package at %s: %v", commit, err)
	}
	if err := renamePublished(tree, path, true); err != nil {
		return err
	}
	return syncPackageDirectory(filepath.Dir(path))
}
