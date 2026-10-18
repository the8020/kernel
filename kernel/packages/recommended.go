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

// InstallRecommendedPackages uses the active kernel's list in declaration order.
// Each package uses the ordinary source lock and schema/activation transaction.
func (s *Store) InstallRecommendedPackages(ctx context.Context) ([]RecommendedPackageResult, error) {
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
	results := make([]RecommendedPackageResult, 0, len(config.Packages))
	for _, item := range config.Packages {
		result, err := s.synchronizePackage(ctx, item.ID, "", "", &item)
		row := RecommendedPackageResult{PackageID: item.ID, Commit: result.Commit, Status: "unchanged"}
		switch {
		case err != nil:
			row.Status, row.Reason = "failed", err.Error()
		case result.skipped != "":
			row.Status, row.Reason, row.Commit = "skipped", result.skipped, result.PreviousCommit
		case result.Cloned:
			row.Status = "installed"
		case result.Changed:
			row.Status = "updated"
		}
		results = append(results, row)
		if err != nil {
			return results, fmt.Errorf("recommended package %s: %w", item.ID, err)
		}
	}
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
