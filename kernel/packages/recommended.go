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

type RecommendedPackage struct {
	ID             string `toml:"id" json:"id"`
	Source         string `toml:"source" json:"source"`
	Version        string `toml:"version" json:"version"`
	originalSource string
	sourceChanged  bool
}

// RecommendedSource contains transport references, never credential values.
type RecommendedSource struct {
	PackageID string `json:"package_id"`
	Source    string `json:"source"`
	Secret    string `json:"secret,omitempty"`
	Username  string `json:"username,omitempty"`
	Skip      string `json:"skip,omitempty"`
}

type RecommendedPackageResult struct {
	PackageID string `json:"package_id"`
	Status    string `json:"status"`
	Commit    string `json:"commit,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// RecommendedPackages reads the installed list for application source selection.
func (s *Store) RecommendedPackages() ([]RecommendedPackage, error) {
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
		Packages []RecommendedPackage `toml:"packages"`
	}
	config.Packages = []RecommendedPackage{}
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
	return config.Packages, nil
}

// InstallRecommendedPackages retains the direct native installation mechanism.
func (s *Store) InstallRecommendedPackages(ctx context.Context) ([]RecommendedPackageResult, error) {
	return s.installRecommendedPackages(ctx, nil)
}

// InstallRecommendedPackagesFrom applies a complete application-selected transport map.
func (s *Store) InstallRecommendedPackagesFrom(ctx context.Context, sources []RecommendedSource) ([]RecommendedPackageResult, error) {
	if sources == nil {
		return nil, errors.New("recommended package sources are required")
	}
	return s.installRecommendedPackages(ctx, sources)
}

func (s *Store) installRecommendedPackages(ctx context.Context, sources []RecommendedSource) (results []RecommendedPackageResult, err error) {
	items, err := s.RecommendedPackages()
	if err != nil {
		return nil, err
	}
	routes := make(map[string]RecommendedSource, len(sources))
	if sources != nil {
		if len(sources) != len(items) {
			return nil, errors.New("recommended sources must match the complete installed list")
		}
		for _, route := range sources {
			if _, exists := routes[route.PackageID]; exists || len(route.Skip) > 300 {
				return nil, errors.New("invalid or duplicate recommended source")
			}
			identity, parseErr := ParsePackageID(route.PackageID)
			if parseErr != nil {
				return nil, parseErr
			}
			if route.Skip == "" {
				entry := PackageIndex{Author: identity.Namespace, Repository: identity.Repository, Source: route.Source, Secret: route.Secret}
				if err := validatePackageIndex(&entry); err != nil {
					return nil, err
				}
				route.Source, route.Secret = entry.Source, entry.Secret
				if route.Username != "" && route.Secret == "" {
					return nil, errors.New("recommended source username requires a named secret")
				}
			}
			routes[route.PackageID] = route
		}
		for _, item := range items {
			if _, exists := routes[item.ID]; !exists {
				return nil, fmt.Errorf("missing recommended source for %s", item.ID)
			}
		}
	}
	ids := make([]string, len(items))
	for i, item := range items {
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
	results = make([]RecommendedPackageResult, 0, len(items))
	candidates := []deployment.Candidate{}
	for _, item := range items {
		route, routed := routes[item.ID]
		if route.Skip != "" {
			results = append(results, RecommendedPackageResult{PackageID: item.ID, Status: "skipped", Reason: route.Skip})
			continue
		}
		var token string
		var stage stagedSynchronization
		var stageErr error
		if routed {
			item.originalSource, item.Source = item.Source, route.Source
			if route.Secret != "" {
				if s.secrets == nil {
					stageErr = errors.New("recommended source credentials are unavailable")
				} else {
					token, stageErr = s.secrets.SecretValue(route.Secret)
					if stageErr != nil || token == "" {
						stageErr = errors.New("recommended source credentials are unavailable")
					}
				}
			}
		}
		if stageErr == nil {
			stage, stageErr = s.stagePackageSynchronization(ctx, item.ID, token, route.Username, &item)
			if stageErr == nil && routed && route.Source != item.originalSource && stage.root != "" {
				existing, found, readErr := s.index.Get(ctx, item.ID)
				if readErr != nil {
					return results, readErr
				}
				stage.entry.Source = item.originalSource
				if found && (s.nodeID == "" || existing.Source != route.Source) {
					stage.entry.Source, stage.entry.Secret = existing.Source, existing.Secret
				}
				if s.nodeID != "" {
					stage.entry.Commit, stage.entry.Tag = stage.Commit, ""
				}
			}
		}
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
	if coordinator, ok := hook.(*ActivationCoordinator); ok && s.NodeInstallations() {
		intents := make([]synchronizationIntent, 0, len(stages))
		for _, stage := range stages {
			previous, _, err := coordinator.synchronizationConfiguration(ctx, stage.PackageID)
			if err != nil {
				return results, err
			}
			intent := synchronizationIntent{PackageID: stage.PackageID, Previous: previous, Desired: configuration(stage.entry)}
			if route := routes[stage.PackageID]; route.Source != "" && route.Source != stage.entry.Source {
				intent.Transport = route.Source
			}
			intents = append(intents, intent)
		}
		ctx = context.WithValue(ctx, synchronizationKey{}, intents)
	} else {
		for _, stage := range stages {
			if err := s.index.Put(ctx, stage.entry); err != nil {
				return results, err
			}
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
