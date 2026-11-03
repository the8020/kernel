package packages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"the8020/kernel/deployment"
)

var packageVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type recommendedPackage struct {
	ID      string `toml:"id"`
	Source  string `toml:"source"`
	Version string `toml:"version"`
}

type RecommendedPackageResult struct {
	PackageID string `json:"package_id"`
	Status    string `json:"status"`
	Commit    string `json:"commit,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// InstallRecommendedPackages stages the active kernel's compatible recommendations
// before evaluating and publishing one activation. List order affects reporting only.
func (s *Store) InstallRecommendedPackages(ctx context.Context) (results []RecommendedPackageResult, err error) {
	path := filepath.Join(s.workspaceRoot, "node/kernel/scripts/bootstrap-packages.toml")
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read recommended packages: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > manifestLimit {
		return nil, errors.New("recommended packages must be a regular file no larger than 1 MiB")
	}
	var config struct {
		Schema   int                  `toml:"schema"`
		Packages []recommendedPackage `toml:"packages"`
	}
	if err := decodeTOMLFile(path, &config); err != nil {
		return nil, fmt.Errorf("read recommended packages: %w", err)
	}
	if config.Schema != 1 || len(config.Packages) > 256 {
		return nil, errors.New("recommended packages require schema 1 and at most 256 entries")
	}
	seen := map[string]bool{}
	for i := range config.Packages {
		item := &config.Packages[i]
		identity, err := ParsePackageID(item.ID)
		if err != nil {
			return nil, err
		}
		entry := PackageIndex{Author: identity.Namespace, Repository: identity.Repository, Source: item.Source, Tag: item.Version}
		if err := validatePackageIndex(&entry); err != nil {
			return nil, fmt.Errorf("recommended package %s: %w", item.ID, err)
		}
		item.ID, item.Source = entry.PackageID, entry.Source
		if item.Version != "" && !packageVersionPattern.MatchString(item.Version) {
			return nil, fmt.Errorf("recommended package %s version must be major.minor.patch", item.ID)
		}
		if seen[item.ID] {
			return nil, fmt.Errorf("duplicate recommended package %s", item.ID)
		}
		seen[item.ID] = true
	}
	ids := make([]string, len(config.Packages))
	for i, item := range config.Packages {
		ids[i] = item.ID
	}
	if len(ids) == 0 {
		return []RecommendedPackageResult{}, nil
	}
	hook := s.schemaDeployment()
	if recovery, ok := hook.(interface {
		RecoverPackages(context.Context, []string) error
	}); ok {
		if err := recovery.RecoverPackages(ctx, ids); err != nil {
			return nil, fmt.Errorf("resume recommended package activation: %w", err)
		}
	}
	unlock, err := LockSources(ctx, s.packagesRoot, ids)
	if err != nil {
		return nil, err
	}
	defer unlock()
	stages := []stagedSynchronization{}
	var transactionID string
	retain := false
	defer func() {
		if retain {
			return
		}
		for _, stage := range stages {
			err = errors.Join(err, os.RemoveAll(stage.stageRoot))
		}
	}()
	results = make([]RecommendedPackageResult, 0, len(config.Packages))
	candidates := []deployment.Candidate{}
	for _, item := range config.Packages {
		stage, stageErr := s.stagePackageSynchronization(ctx, item.ID, "", "", &item)
		row := RecommendedPackageResult{PackageID: item.ID, Commit: stage.Commit, Status: "unchanged"}
		switch {
		case stageErr != nil:
			row.Status, row.Reason = "failed", stageErr.Error()
		case stage.skipped != "":
			row.Status, row.Reason, row.Commit = "skipped", stage.skipped, stage.PreviousCommit
		case stage.root != "":
			row.Status = "not_applied"
			stages = append(stages, stage)
			candidates = append(candidates, deployment.Candidate{PackageID: item.ID, Root: stage.root, Commit: stage.Commit})
		}
		results = append(results, row)
		if stageErr != nil {
			return results, fmt.Errorf("stage recommended package %s: %w", item.ID, stageErr)
		}
	}
	if len(candidates) == 0 {
		return results, nil
	}
	if hook == nil {
		return results, errors.New("recommended package activation requires a schema coordinator")
	}
	for _, stage := range stages {
		if err := s.index.Put(ctx, stage.entry); err != nil {
			return results, err
		}
	}
	transactionID, err = activationID()
	if err != nil {
		return results, err
	}
	// A committed schema receipt or a partial source switch belongs to durable recovery.
	defer func() {
		if err == nil {
			return
		}
		if !retain {
			err = errors.Join(err, hook.Complete(context.WithoutCancel(ctx), transactionID, false))
		}
		forward, checkErr := deployment.NeedsForward(context.WithoutCancel(ctx), hook, transactionID)
		retain = retain || forward || checkErr != nil
		for i := range results {
			if results[i].Status == "not_applied" {
				results[i].Status, results[i].Reason = "failed", fmt.Sprintf("activation %s: %v; retained/skipped peers must be compatible; repair and retry kernel.packages.recommended", transactionID, err)
			}
		}
	}()
	if err := hook.Prepare(ctx, transactionID, candidates); err != nil {
		return results, fmt.Errorf("prepare recommended package set: %w", err)
	}
	for _, stage := range stages {
		switched, switchErr := replacePackageDirectory(stage.destination, stage.root)
		retain = retain || switched
		if switchErr != nil {
			return results, fmt.Errorf("activate recommended package %s: %w", stage.PackageID, switchErr)
		}
	}
	if err := hook.Complete(ctx, transactionID, true); err != nil {
		return results, fmt.Errorf("complete recommended package set: %w", err)
	}
	for _, stage := range stages {
		if err := finalizePackageDirectory(stage.destination); err != nil {
			return results, err
		}
		for i := range results {
			if results[i].PackageID == stage.PackageID {
				results[i].Status = "updated"
				if !stage.exists {
					results[i].Status = "installed"
				}
			}
		}
	}
	retain = false
	return results, nil
}

func comparePackageVersions(left, right string) int {
	a, b := strings.Split(left, "."), strings.Split(right, ".")
	for i := range a {
		if len(a[i]) > len(b[i]) {
			return 1
		}
		if len(a[i]) < len(b[i]) {
			return -1
		}
		if order := strings.Compare(a[i], b[i]); order != 0 {
			return order
		}
	}
	return 0
}

func (s *Store) recommendationSkip(ctx context.Context, path, previous, commit, version string) (string, error) {
	if previous == commit {
		return "", nil
	}
	if version != "" {
		tags, err := s.gitValue(ctx, path, "tag", "--points-at", previous)
		if err != nil {
			return "", err
		}
		for _, tag := range strings.Fields(tags) {
			if packageVersionPattern.MatchString(tag) && comparePackageVersions(tag, version) >= 0 {
				return "installed version already meets the recommendation", nil
			}
		}
	}
	_, err := s.runGit(ctx, path, nil, "merge-base", "--is-ancestor", previous, commit)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "local commits or a newer installed revision; update is not a fast-forward", nil
	}
	return "", err
}
