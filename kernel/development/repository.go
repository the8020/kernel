package development

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (m *Manager) inspectRepository(id string) (repository, error) {
	path, err := m.packageRoot(id)
	if err != nil {
		return repository{}, err
	}
	result := repository{PackageID: id, Path: path, Status: "not-initialized"}
	info, err := os.Stat(filepath.Join(path, ".git"))
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return repository{}, err
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
		return repository{}, statusErr
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
