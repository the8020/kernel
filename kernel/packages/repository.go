package packages

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"the8020/kernel/deployment"
)

const maximumRepositoryCommits = 100

type RepositoryBranch struct {
	Name    string `json:"name"`
	Commit  string `json:"commit"`
	Current bool   `json:"current"`
	Remote  bool   `json:"remote"`
}

type RepositoryCommit struct {
	Commit      string `json:"commit"`
	ShortCommit string `json:"short_commit"`
	AuthoredAt  string `json:"authored_at"`
	Author      string `json:"author"`
	Subject     string `json:"subject"`
	Current     bool   `json:"current"`
}

type Repository struct {
	PackageID       string             `json:"package_id"`
	Path            string             `json:"path"`
	ActivationReady bool               `json:"activation_ready"`
	Branch          string             `json:"branch,omitempty"`
	Head            string             `json:"head,omitempty"`
	RemoteName      string             `json:"remote_name,omitempty"`
	RemoteURL       string             `json:"remote_url,omitempty"`
	Clean           bool               `json:"clean"`
	Status          string             `json:"status"`
	Branches        []RepositoryBranch `json:"branches"`
	Commits         []RepositoryCommit `json:"commits"`
	remoteURL       string
}

// RepositoryMutation includes private service sets so command composition can
// refresh only workloads affected by a changed checkout.
type RepositoryMutation struct {
	Repository       Repository `json:"repository"`
	Changed          bool       `json:"changed"`
	PreviousServices []string   `json:"-"`
	Services         []string   `json:"-"`
}

func (s *Store) ListPackageRepositories(ctx context.Context) ([]Repository, error) {
	s.repositoryMu.RLock()
	defer s.repositoryMu.RUnlock()
	namespaces, err := os.ReadDir(s.packagesRoot)
	if err != nil {
		return nil, fmt.Errorf("read packages root: %w", err)
	}
	result := []Repository{}
	for _, namespace := range namespaces {
		if !namespace.IsDir() || ValidateName(namespace.Name()) != nil {
			continue
		}
		repositories, err := os.ReadDir(filepath.Join(s.packagesRoot, namespace.Name()))
		if err != nil {
			return nil, fmt.Errorf("read package namespace %s: %w", namespace.Name(), err)
		}
		for _, repository := range repositories {
			if !repository.IsDir() || ValidateName(repository.Name()) != nil {
				continue
			}
			item, err := s.inspectPackageRepositoryUnlocked(ctx, namespace.Name()+"/"+repository.Name())
			if err != nil {
				return nil, err
			}
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].PackageID < result[j].PackageID })
	return result, nil
}

func (s *Store) InspectPackageRepository(ctx context.Context, packageID string) (Repository, error) {
	s.repositoryMu.RLock()
	defer s.repositoryMu.RUnlock()
	return s.inspectPackageRepositoryUnlocked(ctx, packageID)
}

func (s *Store) inspectPackageRepositoryUnlocked(ctx context.Context, packageID string) (Repository, error) {
	path, exists, err := s.packageDestination(packageID)
	if err != nil {
		return Repository{}, err
	}
	if !exists {
		return Repository{}, fmt.Errorf("package is not installed: %s", packageID)
	}
	return s.inspectPackageRepositoryAt(ctx, packageID, path)
}

func (s *Store) inspectPackageRepositoryAt(ctx context.Context, packageID, path string) (Repository, error) {
	result := Repository{
		PackageID: packageID, Path: path, Status: "not-initialized",
		Branches: []RepositoryBranch{}, Commits: []RepositoryCommit{},
	}
	info, err := os.Stat(filepath.Join(path, ".git"))
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return Repository{}, err
	}
	if !info.IsDir() {
		result.Status = "invalid-git-metadata"
		return result, nil
	}
	head, err := s.gitValue(ctx, path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		result.Status = "missing-initial-commit"
		return result, nil
	}
	branch, _ := s.gitValue(ctx, path, "branch", "--show-current")
	status, err := s.gitValue(ctx, path, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return Repository{}, fmt.Errorf("inspect package repository: %w", err)
	}
	remoteName, remoteURL, err := s.repositoryRemote(ctx, path)
	if err != nil {
		return Repository{}, err
	}
	branches, err := s.repositoryBranches(ctx, path, remoteName, branch)
	if err != nil {
		return Repository{}, err
	}
	commits, err := s.repositoryCommits(ctx, path, head)
	if err != nil {
		return Repository{}, err
	}
	result.ActivationReady = true
	result.Branch = branch
	result.Head = head
	result.RemoteName = remoteName
	result.RemoteURL = displayRemoteURL(remoteURL)
	result.remoteURL = remoteURL
	result.Clean = status == ""
	result.Status = "ready"
	result.Branches = branches
	result.Commits = commits
	if !result.Clean {
		result.Status = "shared-worktree-modified"
	}
	return result, nil
}

func (s *Store) InitializePackageRepository(ctx context.Context, packageID, authorName, authorEmail, message string) (Repository, error) {
	s.repositoryMu.Lock()
	defer s.repositoryMu.Unlock()
	unlock, err := s.lockPackage(ctx, packageID)
	if err != nil {
		return Repository{}, err
	}
	defer unlock()
	path, exists, err := s.packageDestination(packageID)
	if err != nil {
		return Repository{}, err
	}
	if !exists {
		return Repository{}, fmt.Errorf("package is not installed: %s", packageID)
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		return Repository{}, errors.New("package repository is already initialized")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Repository{}, err
	}
	if strings.TrimSpace(authorName) == "" {
		authorName = "80|20 Package Manager"
	}
	if strings.TrimSpace(authorEmail) == "" {
		authorEmail = "packages@the8020.local"
	}
	if strings.TrimSpace(message) == "" {
		message = "Initialize " + packageID
	}
	if output, err := s.runGit(ctx, path, nil, "init", "-q", "-b", "main"); err != nil {
		return Repository{}, fmt.Errorf("initialize package repository: %w: %s", err, cleanGitOutput(output))
	}
	if output, err := s.runGit(ctx, path, nil, "add", "-A"); err != nil {
		return Repository{}, fmt.Errorf("stage package repository: %w: %s", err, cleanGitOutput(output))
	}
	environment := []string{
		"GIT_AUTHOR_NAME=" + authorName, "GIT_AUTHOR_EMAIL=" + authorEmail,
		"GIT_COMMITTER_NAME=" + authorName, "GIT_COMMITTER_EMAIL=" + authorEmail,
	}
	if output, err := s.runGit(ctx, path, environment, "commit", "-q", "-m", message); err != nil {
		return Repository{}, fmt.Errorf("commit package repository: %w: %s", err, cleanGitOutput(output))
	}
	return s.inspectPackageRepositoryUnlocked(ctx, packageID)
}

func (s *Store) ConfigurePackageRemote(ctx context.Context, packageID, name, remoteURL string) (Repository, error) {
	s.repositoryMu.Lock()
	defer s.repositoryMu.Unlock()
	unlock, err := s.lockPackage(ctx, packageID)
	if err != nil {
		return Repository{}, err
	}
	defer unlock()
	repository, err := s.inspectPackageRepositoryUnlocked(ctx, packageID)
	if err != nil {
		return Repository{}, err
	}
	if !repository.ActivationReady {
		return Repository{}, errors.New("package repository is not initialized")
	}
	if name == "" {
		name = "origin"
	}
	if !safeRemoteName(name) {
		return Repository{}, errors.New("invalid Git remote name")
	}
	remoteURL = strings.TrimSpace(remoteURL)
	if parsed, parseErr := url.Parse(remoteURL); parseErr != nil || parsed.User != nil {
		return Repository{}, errors.New("Git remote URL must not contain credentials")
	}
	if remoteURL != "" {
		remotes, _ := s.gitValue(ctx, repository.Path, "remote")
		operation := []string{"remote", "add", name, remoteURL}
		if containsLine(remotes, name) {
			operation = []string{"remote", "set-url", name, remoteURL}
		}
		if output, err := s.runGit(ctx, repository.Path, nil, operation...); err != nil {
			return Repository{}, fmt.Errorf("configure package remote: %w: %s", err, cleanGitOutput(output))
		}
	}
	return s.inspectPackageRepositoryUnlocked(ctx, packageID)
}

func (s *Store) PullPackageRepository(ctx context.Context, packageID string) (RepositoryMutation, error) {
	return s.mutatePackageRepository(ctx, packageID, func(path string, repository Repository) error {
		if !repository.Clean {
			return errors.New("package repository has uncommitted changes")
		}
		if repository.Branch == "" {
			return errors.New("cannot pull while HEAD is detached")
		}
		if repository.RemoteName == "" {
			return errors.New("package repository has no Git remote")
		}
		environment, err := s.repositoryAuthentication(packageID, repository.remoteURL)
		if err != nil {
			return err
		}
		if output, err := s.runGit(ctx, path, environment, "fetch", "--quiet", "--prune", repository.RemoteName); err != nil {
			return fmt.Errorf("fetch package repository: %w: %s", err, cleanGitOutput(output))
		}
		upstream := "refs/remotes/" + repository.RemoteName + "/" + repository.Branch
		if _, err := s.gitValue(ctx, path, "rev-parse", "--verify", upstream+"^{commit}"); err != nil {
			return fmt.Errorf("remote branch %s/%s does not exist", repository.RemoteName, repository.Branch)
		}
		if output, err := s.runGit(ctx, path, nil, "merge", "--ff-only", upstream); err != nil {
			return fmt.Errorf("fast-forward package repository: %w: %s", err, cleanGitOutput(output))
		}
		return nil
	})
}

func (s *Store) PushPackageRepository(ctx context.Context, packageID string) (Repository, error) {
	s.repositoryMu.Lock()
	defer s.repositoryMu.Unlock()
	unlock, err := s.lockPackage(ctx, packageID)
	if err != nil {
		return Repository{}, err
	}
	defer unlock()
	repository, err := s.inspectPackageRepositoryUnlocked(ctx, packageID)
	if err != nil {
		return Repository{}, err
	}
	if !repository.ActivationReady {
		return Repository{}, errors.New("package repository is not initialized")
	}
	if repository.Branch == "" {
		return Repository{}, errors.New("cannot push while HEAD is detached")
	}
	if repository.RemoteName == "" {
		return Repository{}, errors.New("package repository has no Git remote")
	}
	environment, err := s.repositoryAuthentication(packageID, repository.remoteURL)
	if err != nil {
		return Repository{}, err
	}
	refspec := "HEAD:refs/heads/" + repository.Branch
	if output, err := s.runGit(ctx, repository.Path, environment, "push", "--porcelain", repository.RemoteName, refspec); err != nil {
		return Repository{}, fmt.Errorf("push package repository: %w: %s", err, cleanGitOutput(output))
	}
	return s.inspectPackageRepositoryUnlocked(ctx, packageID)
}

func (s *Store) CheckoutPackageRepository(ctx context.Context, packageID, branch, commit string) (RepositoryMutation, error) {
	branch = strings.TrimSpace(branch)
	commit = strings.ToLower(strings.TrimSpace(commit))
	if (branch == "") == (commit == "") {
		return RepositoryMutation{}, errors.New("select exactly one branch or commit")
	}
	if commit != "" && !isCommitID(commit) {
		return RepositoryMutation{}, errors.New("repository commit must be a 7- to 64-character hexadecimal object ID")
	}
	return s.mutatePackageRepository(ctx, packageID, func(path string, repository Repository) error {
		if !repository.Clean {
			return errors.New("package repository has uncommitted changes")
		}
		if branch != "" {
			if output, err := s.runGit(ctx, path, nil, "check-ref-format", "--branch", branch); err != nil {
				return fmt.Errorf("invalid repository branch: %s", cleanGitOutput(output))
			}
			if _, err := s.gitValue(ctx, path, "show-ref", "--verify", "refs/heads/"+branch); err == nil {
				if output, err := s.runGit(ctx, path, nil, "checkout", "--quiet", branch); err != nil {
					return fmt.Errorf("check out package branch: %w: %s", err, cleanGitOutput(output))
				}
				return nil
			}
			if repository.RemoteName == "" {
				return fmt.Errorf("local branch %q does not exist", branch)
			}
			remoteRef := "refs/remotes/" + repository.RemoteName + "/" + branch
			if _, err := s.gitValue(ctx, path, "show-ref", "--verify", remoteRef); err != nil {
				environment, authErr := s.repositoryAuthentication(packageID, repository.remoteURL)
				if authErr != nil {
					return authErr
				}
				if output, fetchErr := s.runGit(ctx, path, environment, "fetch", "--quiet", "--prune", repository.RemoteName); fetchErr != nil {
					return fmt.Errorf("fetch package branches: %w: %s", fetchErr, cleanGitOutput(output))
				}
			}
			if _, err := s.gitValue(ctx, path, "show-ref", "--verify", remoteRef); err != nil {
				return fmt.Errorf("branch %q does not exist", branch)
			}
			if output, err := s.runGit(ctx, path, nil, "checkout", "--quiet", "-b", branch, "--track", repository.RemoteName+"/"+branch); err != nil {
				return fmt.Errorf("check out remote package branch: %w: %s", err, cleanGitOutput(output))
			}
			return nil
		}
		if _, err := s.gitValue(ctx, path, "rev-parse", "--verify", commit+"^{commit}"); err != nil {
			if repository.RemoteName == "" {
				return fmt.Errorf("repository commit %s does not exist", commit)
			}
			environment, authErr := s.repositoryAuthentication(packageID, repository.remoteURL)
			if authErr != nil {
				return authErr
			}
			if output, fetchErr := s.runGit(ctx, path, environment, "fetch", "--quiet", repository.RemoteName, commit); fetchErr != nil {
				return fmt.Errorf("fetch package commit: %w: %s", fetchErr, cleanGitOutput(output))
			}
		}
		if output, err := s.runGit(ctx, path, nil, "checkout", "--quiet", "--detach", commit); err != nil {
			return fmt.Errorf("check out package commit: %w: %s", err, cleanGitOutput(output))
		}
		return nil
	})
}

func (s *Store) mutatePackageRepository(ctx context.Context, packageID string, mutate func(string, Repository) error) (RepositoryMutation, error) {
	s.repositoryMu.Lock()
	defer s.repositoryMu.Unlock()
	unlock, err := s.lockPackage(ctx, packageID)
	if err != nil {
		return RepositoryMutation{}, err
	}
	defer unlock()
	repository, err := s.inspectPackageRepositoryUnlocked(ctx, packageID)
	if err != nil {
		return RepositoryMutation{}, err
	}
	if !repository.ActivationReady {
		return RepositoryMutation{}, errors.New("package repository is not initialized")
	}
	previousServices, err := serviceIDsAt(repository.Path, packageID)
	if err != nil {
		return RepositoryMutation{}, err
	}
	previousHead := repository.Head
	stageRoot, err := os.MkdirTemp(filepath.Dir(repository.Path), "."+filepath.Base(repository.Path)+"-mutation-")
	if err != nil {
		return RepositoryMutation{}, fmt.Errorf("create package mutation stage: %w", err)
	}
	defer os.RemoveAll(stageRoot)
	stage := filepath.Join(stageRoot, "repository")
	if err := copyRepository(repository.Path, stage); err != nil {
		return RepositoryMutation{}, fmt.Errorf("stage package repository: %w", err)
	}
	if err := mutate(stage, repository); err != nil {
		return RepositoryMutation{}, err
	}
	updated, err := s.inspectPackageRepositoryAt(ctx, packageID, stage)
	if err != nil {
		return RepositoryMutation{}, err
	}
	services, err := serviceIDsAt(updated.Path, packageID)
	if err != nil {
		return RepositoryMutation{}, err
	}
	changed := previousHead != updated.Head
	prepared := false
	sourceSwitched := false
	hook := s.schemaDeployment()
	if changed && hook != nil {
		if err := hook.Prepare(ctx, []deployment.Candidate{{PackageID: packageID, Root: updated.Path, Commit: updated.Head}}); err != nil {
			return RepositoryMutation{}, fmt.Errorf("prepare package database schema: %w", err)
		}
		prepared = true
		defer func() {
			if prepared && !sourceSwitched {
				_ = hook.Complete(context.Background(), false)
			}
		}()
	}
	sourceSwitched, err = replacePackageDirectory(repository.Path, stage)
	if err != nil {
		return RepositoryMutation{}, fmt.Errorf("activate package repository mutation: %w", err)
	}
	updated.Path = repository.Path
	if prepared && hook != nil {
		if err := hook.Complete(ctx, true); err != nil {
			return RepositoryMutation{}, fmt.Errorf("complete package database schema deployment: %w", err)
		}
		prepared = false
	}
	return RepositoryMutation{
		Repository: updated, Changed: changed,
		PreviousServices: previousServices, Services: services,
	}, nil
}

func copyRepository(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case entry.Type()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case entry.Type().IsRegular():
			input, err := os.Open(path)
			if err != nil {
				return err
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
			if err != nil {
				_ = input.Close()
				return err
			}
			_, copyErr := io.Copy(output, input)
			return errors.Join(copyErr, output.Sync(), output.Close(), input.Close())
		default:
			return fmt.Errorf("unsupported repository entry %s", relative)
		}
	})
}

func (s *Store) repositoryAuthentication(packageID, remoteURL string) ([]string, error) {
	parsed, err := url.Parse(remoteURL)
	if err != nil || parsed.User != nil {
		return nil, errors.New("Git remote URL must not contain credentials")
	}
	s.indexMu.RLock()
	entry, err := s.readPackageIndexUnlocked(packageID)
	s.indexMu.RUnlock()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if entry.Secret == "" {
		return nil, nil
	}
	if s.secrets == nil {
		return nil, errors.New("package repository secret storage is unavailable")
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return nil, errors.New("a selected Git secret requires an HTTPS remote without embedded credentials")
	}
	value, err := s.secrets.SecretValue(entry.Secret)
	if err != nil {
		return nil, fmt.Errorf("resolve package Git secret %q: %w", entry.Secret, err)
	}
	header := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + value))
	scope := parsed.Scheme + "://" + parsed.Host + "/"
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http." + scope + ".extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + header,
	}, nil
}

func (s *Store) repositoryRemote(ctx context.Context, path string) (string, string, error) {
	output, err := s.gitValue(ctx, path, "remote")
	if err != nil {
		return "", "", fmt.Errorf("list package remotes: %w", err)
	}
	names := strings.Fields(output)
	if len(names) == 0 {
		return "", "", nil
	}
	name := names[0]
	if containsLine(output, "origin") {
		name = "origin"
	}
	remoteURL, err := s.gitValue(ctx, path, "remote", "get-url", name)
	if err != nil {
		return "", "", fmt.Errorf("read package remote: %w", err)
	}
	return name, remoteURL, nil
}

func (s *Store) repositoryBranches(ctx context.Context, path, remoteName, current string) ([]RepositoryBranch, error) {
	format := "%(refname)%00%(objectname)"
	output, err := s.gitValue(ctx, path, "for-each-ref", "--format="+format, "refs/heads", "refs/remotes")
	if err != nil {
		return nil, fmt.Errorf("list package branches: %w", err)
	}
	byName := map[string]RepositoryBranch{}
	for _, line := range strings.Split(output, "\n") {
		parts := strings.Split(line, "\x00")
		if len(parts) != 2 {
			continue
		}
		ref, commit := parts[0], parts[1]
		name, remote := "", false
		switch {
		case strings.HasPrefix(ref, "refs/heads/"):
			name = strings.TrimPrefix(ref, "refs/heads/")
		case remoteName != "" && strings.HasPrefix(ref, "refs/remotes/"+remoteName+"/"):
			name = strings.TrimPrefix(ref, "refs/remotes/"+remoteName+"/")
			remote = true
		}
		if name == "" || name == "HEAD" {
			continue
		}
		item := RepositoryBranch{Name: name, Commit: commit, Current: name == current, Remote: remote}
		if previous, exists := byName[name]; !exists || previous.Remote && !remote {
			byName[name] = item
		}
	}
	result := make([]RepositoryBranch, 0, len(byName))
	for _, branch := range byName {
		result = append(result, branch)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (s *Store) repositoryCommits(ctx context.Context, path, current string) ([]RepositoryCommit, error) {
	format := "%H%x00%h%x00%aI%x00%an%x00%s"
	output, err := s.runGit(ctx, path, nil, "log", "--all", "--max-count=100", "--date=iso-strict", "--pretty=format:"+format)
	if err != nil {
		return nil, fmt.Errorf("list package commits: %w: %s", err, cleanGitOutput(output))
	}
	result := []RepositoryCommit{}
	foundCurrent := false
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		commit, ok := parseRepositoryCommit(line, current)
		if !ok {
			continue
		}
		foundCurrent = foundCurrent || commit.Current
		result = append(result, commit)
		if len(result) == maximumRepositoryCommits {
			break
		}
	}
	if !foundCurrent {
		output, err := s.runGit(ctx, path, nil, "show", "-s", "--date=iso-strict", "--pretty=format:"+format, current)
		if err != nil {
			return nil, fmt.Errorf("inspect current package commit: %w: %s", err, cleanGitOutput(output))
		}
		if commit, ok := parseRepositoryCommit(strings.TrimSpace(output), current); ok {
			result = append([]RepositoryCommit{commit}, result...)
			if len(result) > maximumRepositoryCommits {
				result = result[:maximumRepositoryCommits]
			}
		}
	}
	return result, nil
}

func parseRepositoryCommit(line, current string) (RepositoryCommit, bool) {
	parts := strings.SplitN(line, "\x00", 5)
	if len(parts) != 5 {
		return RepositoryCommit{}, false
	}
	if _, err := time.Parse(time.RFC3339, parts[2]); err != nil {
		return RepositoryCommit{}, false
	}
	return RepositoryCommit{
		Commit: parts[0], ShortCommit: parts[1], AuthoredAt: parts[2],
		Author: parts[3], Subject: parts[4], Current: parts[0] == current,
	}, true
}

func displayRemoteURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.User == nil {
		return value
	}
	parsed.User = nil
	return parsed.String()
}

func safeRemoteName(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && !strings.ContainsRune("._/-", character) {
			return false
		}
	}
	return !strings.Contains(value, "..") && !strings.HasPrefix(value, "-")
}

func containsLine(value, wanted string) bool {
	for _, line := range strings.Split(value, "\n") {
		if strings.TrimSpace(line) == wanted {
			return true
		}
	}
	return false
}
