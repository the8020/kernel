package development

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func (m *Manager) ListRepositories() ([]Repository, error) {
	m.repositoryMu.RLock()
	defer m.repositoryMu.RUnlock()
	entries, err := os.ReadDir(m.config.PackagesRoot)
	if err != nil {
		return nil, err
	}
	result := []Repository{}
	for _, namespace := range entries {
		if !namespace.IsDir() || !safePackageSegment(namespace.Name()) {
			continue
		}
		repositories, err := os.ReadDir(filepath.Join(m.config.PackagesRoot, namespace.Name()))
		if err != nil {
			return nil, err
		}
		for _, repository := range repositories {
			if !repository.IsDir() || !safePackageSegment(repository.Name()) {
				continue
			}
			item, err := m.inspectRepository(namespace.Name() + "/" + repository.Name())
			if err != nil {
				return nil, err
			}
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].PackageID < result[j].PackageID })
	return result, nil
}

func (m *Manager) InspectRepository(id string) (Repository, error) {
	m.repositoryMu.RLock()
	defer m.repositoryMu.RUnlock()
	return m.inspectRepository(id)
}

func (m *Manager) inspectRepository(id string) (Repository, error) {
	path, err := m.packageRoot(id)
	if err != nil {
		return Repository{}, err
	}
	result := Repository{PackageID: id, Path: path, Status: "not-initialized"}
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
	head, headErr := gitOutput(path, "rev-parse", "HEAD")
	if headErr != nil {
		result.Status = "missing-initial-commit"
		return result, nil
	}
	branch, _ := gitOutput(path, "branch", "--show-current")
	status, statusErr := gitOutput(path, "status", "--porcelain=v1")
	if statusErr != nil {
		return Repository{}, statusErr
	}
	remotes, _ := gitOutput(path, "remote")
	names := strings.Fields(remotes)
	if len(names) > 0 {
		result.RemoteName = names[0]
		result.RemoteURL, _ = gitOutput(path, "remote", "get-url", names[0])
	}
	result.ActivationReady, result.Branch, result.Head, result.Clean, result.Status = true, strings.TrimSpace(branch), strings.TrimSpace(head), strings.TrimSpace(status) == "", "ready"
	if !result.Clean {
		result.Status = "shared-worktree-modified"
	}
	return result, nil
}

func (m *Manager) InitializeRepository(ctx context.Context, id, authorName, authorEmail, message string) (Repository, error) {
	m.repositoryMu.Lock()
	defer m.repositoryMu.Unlock()
	path, err := m.packageRoot(id)
	if err != nil {
		return Repository{}, err
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		return Repository{}, errors.New("package repository is already initialized")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Repository{}, err
	}
	if strings.TrimSpace(authorName) == "" {
		authorName = "80|20 Development"
	}
	if strings.TrimSpace(authorEmail) == "" {
		authorEmail = "development@the8020.local"
	}
	if strings.TrimSpace(message) == "" {
		message = "Initialize " + id
	}
	if output, err := gitCommand(ctx, path, nil, "init", "-b", "main"); err != nil {
		return Repository{}, fmt.Errorf("initialize package Git repository: %w: %s", err, output)
	}
	if output, err := gitCommand(ctx, path, nil, "add", "-A"); err != nil {
		return Repository{}, fmt.Errorf("stage initial package commit: %w: %s", err, output)
	}
	environment := gitIdentity(authorName, authorEmail)
	if output, err := gitCommand(ctx, path, environment, "commit", "-m", message); err != nil {
		return Repository{}, fmt.Errorf("create initial package commit: %w: %s", err, output)
	}
	if m.config.Logger != nil {
		head, _ := m.repositoryHead(id)
		m.config.Logger.Info("package repository initialized", "package_id", id, "new_commit", head, "result_state", "ready")
	}
	return m.inspectRepository(id)
}

func (m *Manager) ConfigureRemote(ctx context.Context, id, name, url string) (Repository, error) {
	m.repositoryMu.Lock()
	defer m.repositoryMu.Unlock()
	repository, err := m.inspectRepository(id)
	if err != nil {
		return Repository{}, err
	}
	if !repository.ActivationReady {
		return Repository{}, errors.New("package repository is not activation-ready")
	}
	if name == "" {
		name = "origin"
	}
	if !safeRemoteName(name) {
		return Repository{}, errors.New("invalid Git remote name")
	}
	if strings.TrimSpace(url) != "" {
		if current, _ := gitOutput(repository.Path, "remote"); containsLine(current, name) {
			if output, err := gitCommand(ctx, repository.Path, nil, "remote", "set-url", name, url); err != nil {
				return Repository{}, fmt.Errorf("change package remote: %w: %s", err, output)
			}
		} else if output, err := gitCommand(ctx, repository.Path, nil, "remote", "add", name, url); err != nil {
			return Repository{}, fmt.Errorf("configure package remote: %w: %s", err, output)
		}
	}
	return m.inspectRepository(id)
}

func (m *Manager) RepositoryStatus(id string) (Repository, error) { return m.InspectRepository(id) }
func (m *Manager) repositoryHead(id string) (string, error) {
	path, err := m.packageRoot(id)
	if err != nil {
		return "", err
	}
	value, err := gitOutput(path, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}
func (m *Manager) packageRoot(id string) (string, error) {
	parts := strings.Split(id, "/")
	if len(parts) != 2 || !safePackageSegment(parts[0]) || !safePackageSegment(parts[1]) {
		return "", errors.New("package ID must be <namespace>/<repository>")
	}
	path := filepath.Join(m.config.PackagesRoot, parts[0], parts[1])
	canonical, err := canonicalDirectory(path)
	if err != nil {
		return "", err
	}
	if !beneath(canonical, m.config.PackagesRoot) {
		return "", errors.New("package repository escapes packages root")
	}
	return canonical, nil
}
func gitOutput(path string, arguments ...string) (string, error) {
	return gitOutputContext(context.Background(), path, nil, arguments...)
}
func gitOutputContext(ctx context.Context, path string, environment []string, arguments ...string) (string, error) {
	output, err := gitCommand(ctx, path, environment, arguments...)
	return strings.TrimSpace(output), err
}
func gitCommand(ctx context.Context, path string, environment []string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", path}, arguments...)...)
	command.Env = append(os.Environ(), environment...)
	output := &boundedBuffer{limit: commandOutputLimit}
	command.Stdout, command.Stderr = output, output
	err := command.Run()
	return output.RawString(), err
}
func gitIdentity(name, email string) []string {
	return []string{"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email, "GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email}
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
