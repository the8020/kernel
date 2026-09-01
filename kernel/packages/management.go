package packages

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	secretstore "the8020/kernel/secrets"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"
)

const (
	packageIndexSchema = 1
	gitOutputLimit     = 2 << 20
	maximumSourceRefs  = 200
	maximumVersions    = 200
)

// PackageIndex is the durable desired source and version of one package.
// Package identity is repeated in the document so an index file remains
// understandable independently of its path.
type PackageIndex struct {
	Schema          int    `toml:"schema" json:"schema"`
	Author          string `toml:"author" json:"author"`
	Repository      string `toml:"repository" json:"repository"`
	Source          string `toml:"source,omitempty" json:"source,omitempty"`
	Commit          string `toml:"commit,omitempty" json:"commit,omitempty"`
	Tag             string `toml:"tag,omitempty" json:"tag,omitempty"`
	Secret          string `toml:"secret,omitempty" json:"secret,omitempty"`
	Local           bool   `toml:"local,omitempty" json:"local"`
	PackageID       string `toml:"-" json:"package_id"`
	Path            string `toml:"-" json:"path"`
	Valid           bool   `toml:"-" json:"valid"`
	ValidationError string `toml:"-" json:"validation_error,omitempty"`
}

type SourceReference struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Commit string `json:"commit"`
}

type SourceInspection struct {
	Source        string            `json:"source"`
	Author        string            `json:"author"`
	Repository    string            `json:"repository"`
	PackageID     string            `json:"package_id"`
	DefaultBranch string            `json:"default_branch,omitempty"`
	References    []SourceReference `json:"references"`
}

type PackageVersion struct {
	Commit      string   `json:"commit"`
	ShortCommit string   `json:"short_commit"`
	AuthoredAt  string   `json:"authored_at"`
	Author      string   `json:"author"`
	Subject     string   `json:"subject"`
	Tags        []string `json:"tags"`
	Current     bool     `json:"current"`
	Selected    bool     `json:"selected"`
}

type PackageVersions struct {
	PackageID      string           `json:"package_id"`
	Source         string           `json:"source,omitempty"`
	CurrentCommit  string           `json:"current_commit,omitempty"`
	SelectedCommit string           `json:"selected_commit,omitempty"`
	Versions       []PackageVersion `json:"versions"`
}

type PackageSynchronization struct {
	PackageID         string   `json:"package_id"`
	Source            string   `json:"source,omitempty"`
	Requested         string   `json:"requested"`
	PreviousCommit    string   `json:"previous_commit,omitempty"`
	Commit            string   `json:"commit,omitempty"`
	Changed           bool     `json:"changed"`
	Cloned            bool     `json:"cloned"`
	Local             bool     `json:"local"`
	PreviousServices  []string `json:"-"`
	Services          []string `json:"services"`
	RestartedServices []string `json:"restarted_services,omitempty"`
	RetiredServices   []string `json:"retired_services,omitempty"`
	Success           bool     `json:"success"`
	Error             string   `json:"error,omitempty"`
}

type LocalPackage struct {
	Index      PackageIndex `json:"index"`
	Package    Package      `json:"package"`
	Commit     string       `json:"commit"`
	Repository string       `json:"repository_path"`
}

func (s *Store) ListPackageIndexes() ([]PackageIndex, error) {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	namespaces, err := os.ReadDir(s.indexRoot)
	if err != nil {
		return nil, fmt.Errorf("read package index: %w", err)
	}
	result := []PackageIndex{}
	for _, namespace := range namespaces {
		if !namespace.IsDir() || strings.HasPrefix(namespace.Name(), ".") {
			continue
		}
		files, readErr := os.ReadDir(filepath.Join(s.indexRoot, namespace.Name()))
		if readErr != nil {
			return nil, fmt.Errorf("read package index namespace %s: %w", namespace.Name(), readErr)
		}
		for _, file := range files {
			if file.IsDir() || file.Type()&os.ModeSymlink != 0 || filepath.Ext(file.Name()) != ".toml" {
				continue
			}
			repository := strings.TrimSuffix(file.Name(), ".toml")
			id := namespace.Name() + "/" + repository
			entry, entryErr := s.readPackageIndexUnlocked(id)
			if entryErr != nil {
				entry = PackageIndex{PackageID: id, Author: namespace.Name(), Repository: repository, Path: filepath.Join(s.indexRoot, namespace.Name(), file.Name()), ValidationError: entryErr.Error()}
			}
			result = append(result, entry)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].PackageID < result[j].PackageID })
	return result, nil
}

func (s *Store) InspectPackageIndex(packageID string) (PackageIndex, error) {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	return s.readPackageIndexUnlocked(packageID)
}

func (s *Store) SetPackageIndex(ctx context.Context, entry PackageIndex) (PackageIndex, error) {
	entry.Schema = packageIndexSchema
	if err := validatePackageIndex(&entry); err != nil {
		return PackageIndex{}, err
	}
	s.repositoryMu.Lock()
	defer s.repositoryMu.Unlock()
	unlock, err := s.lockPackage(ctx, entry.PackageID)
	if err != nil {
		return PackageIndex{}, err
	}
	defer unlock()
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	if err := s.writePackageIndexUnlocked(entry); err != nil {
		return PackageIndex{}, err
	}
	return s.readPackageIndexUnlocked(entry.PackageID)
}

func (s *Store) InspectPackageSource(ctx context.Context, source string) (SourceInspection, error) {
	normalized, author, repository, err := normalizePackageSource(source)
	if err != nil {
		return SourceInspection{}, err
	}
	output, err := s.runGit(ctx, "", nil, "ls-remote", "--symref", normalized, "HEAD", "refs/heads/*", "refs/tags/*")
	if err != nil {
		return SourceInspection{}, fmt.Errorf("inspect Git source: %w: %s", err, cleanGitOutput(output))
	}
	defaultBranch := ""
	references := map[string]SourceReference{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "ref: ") {
			fields := strings.Fields(line)
			if len(fields) == 3 && fields[2] == "HEAD" && strings.HasPrefix(fields[1], "refs/heads/") {
				defaultBranch = strings.TrimPrefix(fields[1], "refs/heads/")
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		commit, reference := fields[0], fields[1]
		kind, name := "", ""
		switch {
		case strings.HasPrefix(reference, "refs/heads/"):
			kind, name = "branch", strings.TrimPrefix(reference, "refs/heads/")
		case strings.HasPrefix(reference, "refs/tags/"):
			kind, name = "tag", strings.TrimSuffix(strings.TrimPrefix(reference, "refs/tags/"), "^{}")
		default:
			continue
		}
		key := kind + "\x00" + name
		if _, ok := references[key]; !ok || strings.HasSuffix(reference, "^{}") {
			references[key] = SourceReference{Kind: kind, Name: name, Commit: commit}
		}
	}
	items := make([]SourceReference, 0, len(references))
	for _, item := range references {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].Name < items[j].Name
	})
	if len(items) > maximumSourceRefs {
		items = items[:maximumSourceRefs]
	}
	return SourceInspection{Source: normalized, Author: author, Repository: repository, PackageID: author + "/" + repository, DefaultBranch: defaultBranch, References: items}, nil
}

func (s *Store) ListPackageVersions(ctx context.Context, packageID string, limit int) (PackageVersions, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > maximumVersions {
		limit = maximumVersions
	}
	s.repositoryMu.Lock()
	defer s.repositoryMu.Unlock()
	unlock, err := s.lockPackage(ctx, packageID)
	if err != nil {
		return PackageVersions{}, err
	}
	defer unlock()
	s.indexMu.RLock()
	entry, err := s.readPackageIndexUnlocked(packageID)
	s.indexMu.RUnlock()
	if err != nil {
		return PackageVersions{}, err
	}
	repositoryPath, err := s.installedPackagePath(packageID)
	if err != nil {
		return PackageVersions{}, err
	}
	if !entry.Local {
		authentication, authErr := s.repositoryAuthentication(packageID, entry.Source)
		if authErr != nil {
			return PackageVersions{}, authErr
		}
		if output, commandErr := s.runGit(ctx, repositoryPath, nil, "remote", "set-url", "origin", entry.Source); commandErr != nil {
			return PackageVersions{}, fmt.Errorf("configure package source: %w: %s", commandErr, cleanGitOutput(output))
		}
		if output, commandErr := s.runGit(ctx, repositoryPath, authentication, "fetch", "--quiet", "--prune", "--tags", "origin"); commandErr != nil {
			return PackageVersions{}, fmt.Errorf("fetch package versions: %w: %s", commandErr, cleanGitOutput(output))
		}
	}
	current, err := s.gitValue(ctx, repositoryPath, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return PackageVersions{}, fmt.Errorf("read installed package commit: %w", err)
	}
	selected, err := s.resolveDesiredCommit(ctx, repositoryPath, entry, false)
	if err != nil {
		return PackageVersions{}, err
	}
	format := "%H%x1f%h%x1f%aI%x1f%an%x1f%s%x1f%D%x1e"
	output, err := s.runGit(ctx, repositoryPath, nil, "log", "--all", "--max-count="+strconv.Itoa(limit), "--date=iso-strict", "--pretty=format:"+format)
	if err != nil {
		return PackageVersions{}, fmt.Errorf("list package versions: %w: %s", err, cleanGitOutput(output))
	}
	versions := []PackageVersion{}
	for _, raw := range strings.Split(output, "\x1e") {
		fields := strings.Split(strings.TrimSpace(raw), "\x1f")
		if len(fields) != 6 {
			continue
		}
		tags := []string{}
		for _, decoration := range strings.Split(fields[5], ",") {
			decoration = strings.TrimSpace(decoration)
			if strings.HasPrefix(decoration, "tag: ") {
				tags = append(tags, strings.TrimPrefix(decoration, "tag: "))
			}
		}
		sort.Strings(tags)
		versions = append(versions, PackageVersion{Commit: fields[0], ShortCommit: fields[1], AuthoredAt: fields[2], Author: fields[3], Subject: fields[4], Tags: tags, Current: fields[0] == current, Selected: fields[0] == selected})
	}
	return PackageVersions{PackageID: packageID, Source: entry.Source, CurrentCommit: current, SelectedCommit: selected, Versions: versions}, nil
}

func (s *Store) SynchronizePackages(ctx context.Context, packageIDs []string) ([]PackageSynchronization, error) {
	if len(packageIDs) == 0 {
		entries, err := s.ListPackageIndexes()
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			packageIDs = append(packageIDs, entry.PackageID)
		}
	}
	unique := map[string]bool{}
	ids := make([]string, 0, len(packageIDs))
	for _, packageID := range packageIDs {
		packageID = strings.TrimSpace(packageID)
		if packageID == "" || unique[packageID] {
			continue
		}
		if _, err := ParsePackageID(packageID); err != nil {
			return nil, err
		}
		unique[packageID] = true
		ids = append(ids, packageID)
	}
	sort.Strings(ids)
	results := make([]PackageSynchronization, 0, len(ids))
	for _, packageID := range ids {
		result, err := s.synchronizePackage(ctx, packageID)
		if err != nil {
			result.PackageID, result.Error, result.Success = packageID, err.Error(), false
		} else {
			result.Success = true
		}
		results = append(results, result)
		if ctx.Err() != nil {
			break
		}
	}
	return results, nil
}

func (s *Store) synchronizePackage(ctx context.Context, packageID string) (PackageSynchronization, error) {
	s.repositoryMu.Lock()
	defer s.repositoryMu.Unlock()
	unlock, err := s.lockPackage(ctx, packageID)
	if err != nil {
		return PackageSynchronization{}, err
	}
	defer unlock()
	s.indexMu.RLock()
	entry, err := s.readPackageIndexUnlocked(packageID)
	s.indexMu.RUnlock()
	if err != nil {
		return PackageSynchronization{}, err
	}
	result := PackageSynchronization{PackageID: packageID, Source: entry.Source, Requested: entry.selector(), Local: entry.Local}
	destination, exists, err := s.packageDestination(packageID)
	if err != nil {
		return result, err
	}
	if exists {
		result.PreviousCommit, err = s.cleanRepositoryHead(ctx, destination)
		if err != nil {
			return result, err
		}
		result.PreviousServices, _ = serviceIDsAt(destination, packageID)
	}
	if entry.Local {
		if !exists {
			return result, errors.New("local package repository does not exist")
		}
		result.Commit = result.PreviousCommit
		result.Services = append([]string(nil), result.PreviousServices...)
		return result, nil
	}
	identity, _ := ParsePackageID(packageID)
	namespaceRoot := filepath.Join(s.packagesRoot, identity.Namespace)
	if err := os.MkdirAll(namespaceRoot, 0o755); err != nil {
		return result, fmt.Errorf("create package namespace: %w", err)
	}
	stageRoot, err := os.MkdirTemp(namespaceRoot, "."+identity.Repository+"-sync-")
	if err != nil {
		return result, fmt.Errorf("create package staging directory: %w", err)
	}
	defer os.RemoveAll(stageRoot)
	stage := filepath.Join(stageRoot, "repository")
	authentication, err := s.repositoryAuthentication(packageID, entry.Source)
	if err != nil {
		return result, err
	}
	if output, commandErr := s.runGit(ctx, "", authentication, "clone", "--quiet", "--no-checkout", "--origin", "origin", entry.Source, stage); commandErr != nil {
		return result, fmt.Errorf("clone package: %w: %s", commandErr, cleanGitOutput(output))
	}
	commit, err := s.resolveDesiredCommit(ctx, stage, entry, true)
	if err != nil {
		return result, err
	}
	if output, commandErr := s.runGit(ctx, stage, nil, "checkout", "--quiet", "--detach", commit); commandErr != nil {
		return result, fmt.Errorf("check out package commit: %w: %s", commandErr, cleanGitOutput(output))
	}
	if err := validateStagedPackage(stage); err != nil {
		return result, err
	}
	result.Commit = commit
	result.Services, err = serviceIDsAt(stage, packageID)
	if err != nil {
		return result, err
	}
	if exists && result.PreviousCommit == result.Commit {
		if output, commandErr := s.runGit(ctx, destination, nil, "remote", "set-url", "origin", entry.Source); commandErr != nil {
			return result, fmt.Errorf("update package remote: %w: %s", commandErr, cleanGitOutput(output))
		}
		return result, nil
	}
	backup := filepath.Join(namespaceRoot, "."+identity.Repository+"-previous")
	if _, statErr := os.Lstat(backup); statErr == nil {
		return result, fmt.Errorf("package backup path already exists: %s", backup)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return result, statErr
	}
	if exists {
		if err := os.Rename(destination, backup); err != nil {
			return result, fmt.Errorf("stage previous package: %w", err)
		}
	}
	if err := os.Rename(stage, destination); err != nil {
		if exists {
			_ = os.Rename(backup, destination)
		}
		return result, fmt.Errorf("activate synchronized package: %w", err)
	}
	if exists {
		if err := os.RemoveAll(backup); err != nil {
			return result, fmt.Errorf("remove previous package: %w", err)
		}
	}
	if err := syncPackageDirectory(namespaceRoot); err != nil {
		return result, err
	}
	result.Changed, result.Cloned = true, !exists
	if s.logger != nil {
		s.logger.Log(ctx, slog.LevelInfo, "package synchronized", "package_id", packageID, "source", entry.Source, "requested", result.Requested, "previous_commit", result.PreviousCommit, "commit", result.Commit, "cloned", result.Cloned)
	}
	return result, nil
}

func (s *Store) CreateLocalPackage(ctx context.Context, author, repository, description string) (LocalPackage, error) {
	entry := PackageIndex{Schema: packageIndexSchema, Author: strings.TrimSpace(author), Repository: strings.TrimSpace(repository), Local: true}
	if err := validatePackageIndex(&entry); err != nil {
		return LocalPackage{}, err
	}
	s.repositoryMu.Lock()
	defer s.repositoryMu.Unlock()
	unlock, err := s.lockPackage(ctx, entry.PackageID)
	if err != nil {
		return LocalPackage{}, err
	}
	defer unlock()
	destination, exists, err := s.packageDestination(entry.PackageID)
	if err != nil {
		return LocalPackage{}, err
	}
	if exists {
		return LocalPackage{}, fmt.Errorf("package already exists: %s", entry.PackageID)
	}
	s.indexMu.RLock()
	_, indexErr := s.readPackageIndexUnlocked(entry.PackageID)
	s.indexMu.RUnlock()
	if indexErr == nil {
		return LocalPackage{}, fmt.Errorf("package index already exists: %s", entry.PackageID)
	}
	if !errors.Is(indexErr, os.ErrNotExist) {
		return LocalPackage{}, indexErr
	}
	identity, _ := ParsePackageID(entry.PackageID)
	namespaceRoot := filepath.Join(s.packagesRoot, identity.Namespace)
	if err := os.MkdirAll(namespaceRoot, 0o755); err != nil {
		return LocalPackage{}, err
	}
	stage, err := os.MkdirTemp(namespaceRoot, "."+identity.Repository+"-local-")
	if err != nil {
		return LocalPackage{}, err
	}
	defer os.RemoveAll(stage)
	manifest := PackageManifest{Schema: packageManifestSchema, Description: strings.TrimSpace(description)}
	if manifest.Description == "" {
		manifest.Description = "Local 80|20 package " + entry.PackageID
	}
	data, err := toml.Marshal(manifest)
	if err != nil {
		return LocalPackage{}, err
	}
	if err := os.WriteFile(filepath.Join(stage, "package.toml"), data, 0o644); err != nil {
		return LocalPackage{}, err
	}
	if output, commandErr := s.runGit(ctx, stage, nil, "init", "-q", "-b", "main"); commandErr != nil {
		return LocalPackage{}, fmt.Errorf("initialize local package repository: %w: %s", commandErr, cleanGitOutput(output))
	}
	if output, commandErr := s.runGit(ctx, stage, nil, "add", "package.toml"); commandErr != nil {
		return LocalPackage{}, fmt.Errorf("stage local package manifest: %w: %s", commandErr, cleanGitOutput(output))
	}
	identityEnvironment := []string{"GIT_AUTHOR_NAME=80|20 Package Manager", "GIT_AUTHOR_EMAIL=packages@the8020.local", "GIT_COMMITTER_NAME=80|20 Package Manager", "GIT_COMMITTER_EMAIL=packages@the8020.local"}
	if output, commandErr := s.runGit(ctx, stage, identityEnvironment, "commit", "-q", "-m", "Initialize "+entry.PackageID); commandErr != nil {
		return LocalPackage{}, fmt.Errorf("commit local package manifest: %w: %s", commandErr, cleanGitOutput(output))
	}
	commit, err := s.gitValue(ctx, stage, "rev-parse", "HEAD")
	if err != nil {
		return LocalPackage{}, err
	}
	if err := os.Rename(stage, destination); err != nil {
		return LocalPackage{}, fmt.Errorf("activate local package: %w", err)
	}
	s.indexMu.Lock()
	writeErr := s.writePackageIndexUnlocked(entry)
	s.indexMu.Unlock()
	if writeErr != nil {
		_ = os.RemoveAll(destination)
		return LocalPackage{}, fmt.Errorf("write local package index: %w", writeErr)
	}
	indexed, err := s.InspectPackageIndex(entry.PackageID)
	if err != nil {
		return LocalPackage{}, err
	}
	installed := s.inspectPackage(identity)
	return LocalPackage{Index: indexed, Package: installed, Commit: commit, Repository: destination}, nil
}

func (s *Store) readPackageIndexUnlocked(packageID string) (PackageIndex, error) {
	identity, err := ParsePackageID(packageID)
	if err != nil {
		return PackageIndex{}, err
	}
	path := filepath.Join(s.indexRoot, identity.Namespace, identity.Repository+".toml")
	var entry PackageIndex
	if err := decodeTOMLFile(path, &entry); err != nil {
		return PackageIndex{}, err
	}
	entry.PackageID, entry.Path = packageID, path
	if entry.Author != identity.Namespace || entry.Repository != identity.Repository {
		return PackageIndex{}, errors.New("package index identity does not match its path")
	}
	if err := validatePackageIndex(&entry); err != nil {
		return PackageIndex{}, err
	}
	entry.Valid = true
	return entry, nil
}

func (s *Store) writePackageIndexUnlocked(entry PackageIndex) error {
	identity, _ := ParsePackageID(entry.PackageID)
	directory := filepath.Join(s.indexRoot, identity.Namespace)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create package index namespace: %w", err)
	}
	data, err := toml.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode package index: %w", err)
	}
	temporary, err := os.CreateTemp(directory, "."+identity.Repository+"-*.toml")
	if err != nil {
		return fmt.Errorf("create package index temporary file: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	path := filepath.Join(directory, identity.Repository+".toml")
	if err == nil {
		err = os.Rename(name, path)
	}
	if err == nil {
		err = syncPackageDirectory(directory)
	}
	if err != nil {
		return fmt.Errorf("write package index: %w", err)
	}
	return nil
}

func validatePackageIndex(entry *PackageIndex) error {
	entry.Author = strings.TrimSpace(entry.Author)
	entry.Repository = strings.TrimSpace(entry.Repository)
	entry.Source = strings.TrimSpace(entry.Source)
	entry.Commit = strings.ToLower(strings.TrimSpace(entry.Commit))
	entry.Tag = strings.TrimSpace(entry.Tag)
	entry.Secret = strings.TrimSpace(entry.Secret)
	if entry.Schema != packageIndexSchema {
		return fmt.Errorf("package index schema must equal %d", packageIndexSchema)
	}
	if err := ValidateName(entry.Author); err != nil {
		return fmt.Errorf("author: %w", err)
	}
	if err := ValidateName(entry.Repository); err != nil {
		return fmt.Errorf("repository: %w", err)
	}
	entry.PackageID = entry.Author + "/" + entry.Repository
	if entry.Commit != "" && entry.Tag != "" {
		return errors.New("package index may select a commit or tag, not both")
	}
	if entry.Secret != "" {
		if err := secretstore.ValidateName(entry.Secret); err != nil {
			return fmt.Errorf("secret: %w", err)
		}
	}
	if entry.Commit != "" && !isCommitID(entry.Commit) {
		return errors.New("package commit must be a 7- to 64-character hexadecimal object ID")
	}
	if entry.Tag != "" && !safeGitTag(entry.Tag) {
		return errors.New("package tag is not a safe Git tag name")
	}
	if entry.Local {
		if entry.Source != "" || entry.Commit != "" || entry.Tag != "" {
			return errors.New("local package indexes cannot declare a source, commit, or tag")
		}
		return nil
	}
	normalized, author, repository, err := normalizePackageSource(entry.Source)
	if err != nil {
		return err
	}
	if author != entry.Author || repository != entry.Repository {
		return fmt.Errorf("Git source resolves to %s/%s instead of %s", author, repository, entry.PackageID)
	}
	entry.Source = normalized
	return nil
}

func normalizePackageSource(raw string) (string, string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", "", errors.New("package source must be an HTTPS Git URL without credentials, query, or fragment")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", "", errors.New("package source path must end with <author>/<repository>")
	}
	author := parts[len(parts)-2]
	repository := strings.TrimSuffix(parts[len(parts)-1], ".git")
	if err := ValidateName(author); err != nil {
		return "", "", "", fmt.Errorf("source author: %w", err)
	}
	if err := ValidateName(repository); err != nil {
		return "", "", "", fmt.Errorf("source repository: %w", err)
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	if !strings.HasSuffix(parsed.Path, ".git") {
		parsed.Path += ".git"
	}
	return parsed.String(), author, repository, nil
}

func (entry PackageIndex) selector() string {
	if entry.Local {
		return "local"
	}
	if entry.Commit != "" {
		return "commit:" + entry.Commit
	}
	if entry.Tag != "" {
		return "tag:" + entry.Tag
	}
	return "latest"
}

func (s *Store) resolveDesiredCommit(ctx context.Context, repositoryPath string, entry PackageIndex, allowFetchCommit bool) (string, error) {
	revision := "refs/remotes/origin/HEAD^{commit}"
	if entry.Tag != "" {
		revision = "refs/tags/" + entry.Tag + "^{commit}"
	}
	if entry.Commit != "" {
		revision = entry.Commit + "^{commit}"
	}
	commit, err := s.gitValue(ctx, repositoryPath, "rev-parse", "--verify", revision)
	if err == nil {
		return commit, nil
	}
	if entry.Commit != "" && allowFetchCommit {
		authentication, authErr := s.repositoryAuthentication(entry.PackageID, entry.Source)
		if authErr != nil {
			return "", authErr
		}
		if output, fetchErr := s.runGit(ctx, repositoryPath, authentication, "fetch", "--quiet", "origin", entry.Commit); fetchErr != nil {
			return "", fmt.Errorf("fetch selected package commit: %w: %s", fetchErr, cleanGitOutput(output))
		}
		if commit, err = s.gitValue(ctx, repositoryPath, "rev-parse", "--verify", "FETCH_HEAD^{commit}"); err == nil {
			return commit, nil
		}
	}
	return "", fmt.Errorf("resolve requested package version %s: %w", entry.selector(), err)
}

func (s *Store) packageDestination(packageID string) (string, bool, error) {
	identity, err := ParsePackageID(packageID)
	if err != nil {
		return "", false, err
	}
	destination := filepath.Join(s.packagesRoot, identity.Namespace, identity.Repository)
	info, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return destination, false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", false, errors.New("installed package path is not a real directory")
	}
	return destination, true, nil
}

func (s *Store) installedPackagePath(packageID string) (string, error) {
	path, exists, err := s.packageDestination(packageID)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("package is not installed: %s", packageID)
	}
	if info, err := os.Stat(filepath.Join(path, ".git")); err != nil || !info.IsDir() {
		return "", errors.New("installed package is not a Git repository")
	}
	return path, nil
}

func (s *Store) cleanRepositoryHead(ctx context.Context, path string) (string, error) {
	if info, err := os.Stat(filepath.Join(path, ".git")); err != nil || !info.IsDir() {
		return "", errors.New("installed package is not a Git repository")
	}
	status, err := s.gitValue(ctx, path, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return "", fmt.Errorf("inspect installed package: %w", err)
	}
	if status != "" {
		return "", errors.New("installed package has uncommitted changes")
	}
	head, err := s.gitValue(ctx, path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("inspect installed package HEAD: %w", err)
	}
	return head, nil
}

func validateStagedPackage(root string) error {
	var manifest PackageManifest
	if err := decodeTOMLFile(filepath.Join(root, "package.toml"), &manifest); err != nil {
		return fmt.Errorf("validate synchronized package manifest: %w", err)
	}
	if manifest.Schema != packageManifestSchema {
		return fmt.Errorf("synchronized package manifest schema must equal %d", packageManifestSchema)
	}
	if strings.TrimSpace(manifest.Description) == "" {
		return errors.New("synchronized package manifest description is required")
	}
	return nil
}

func serviceIDsAt(root, packageID string) ([]string, error) {
	directory := filepath.Join(root, "services")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := []string{}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || ValidateName(entry.Name()) != nil {
			continue
		}
		if info, statErr := os.Lstat(filepath.Join(directory, entry.Name(), "service.toml")); statErr == nil && info.Mode().IsRegular() {
			result = append(result, packageID+"/"+entry.Name())
		}
	}
	sort.Strings(result)
	return result, nil
}

func (s *Store) lockPackage(ctx context.Context, packageID string) (func(), error) {
	identity, err := ParsePackageID(packageID)
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(s.indexRoot, ".locks", identity.Namespace)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, identity.Repository+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (s *Store) gitValue(ctx context.Context, path string, arguments ...string) (string, error) {
	output, err := s.runGit(ctx, path, nil, arguments...)
	return strings.TrimSpace(output), err
}

func (s *Store) runGit(ctx context.Context, path string, extraEnvironment []string, arguments ...string) (string, error) {
	commandArguments := append([]string(nil), arguments...)
	if path != "" {
		commandArguments = append([]string{"-C", path}, commandArguments...)
	}
	command := exec.CommandContext(ctx, s.gitPath, commandArguments...)
	environment := make([]string, 0, len(os.Environ())+len(extraEnvironment)+2)
	for _, value := range os.Environ() {
		name := value
		if separator := strings.IndexByte(value, '='); separator >= 0 {
			name = value[:separator]
		}
		if name == "GIT_CONFIG_COUNT" || strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") || name == "GIT_ASKPASS" || name == "SSH_ASKPASS" {
			continue
		}
		environment = append(environment, value)
	}
	command.Env = append(environment, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	command.Env = append(command.Env, extraEnvironment...)
	output := &limitedGitOutput{limit: gitOutputLimit}
	command.Stdout, command.Stderr = output, output
	err := command.Run()
	if errors.Is(output.err, errGitOutputLimit) {
		return output.String(), errGitOutputLimit
	}
	return output.String(), err
}

var errGitOutputLimit = errors.New("Git output exceeded 2 MiB")

type limitedGitOutput struct {
	buffer bytes.Buffer
	limit  int
	err    error
}

func (b *limitedGitOutput) Write(value []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	remaining := b.limit - b.buffer.Len()
	if len(value) > remaining {
		if remaining > 0 {
			_, _ = b.buffer.Write(value[:remaining])
		}
		b.err = errGitOutputLimit
		return len(value), b.err
	}
	return b.buffer.Write(value)
}

func (b *limitedGitOutput) String() string { return b.buffer.String() }

func cleanGitOutput(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 1000 {
		return value[:1000] + "…"
	}
	return value
}

func isCommitID(value string) bool {
	if len(value) < 7 || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func safeGitTag(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") || strings.Contains(value, "..") || strings.Contains(value, "@{") || strings.HasSuffix(value, "/") || strings.ContainsAny(value, " ~^:?*[\\\x00") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func syncPackageDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil && !errors.Is(err, unix.EINVAL) {
		return err
	}
	return nil
}

var _ io.Writer = (*limitedGitOutput)(nil)
