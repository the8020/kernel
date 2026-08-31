package development

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type packageChanges struct {
	PackageID string
	Base      string
	Shared    string
	Tree      string
	Files     []ActivationFile
}

type preparedActivation struct {
	changes  packageChanges
	result   ActivationPackageResult
	current  string
	commit   string
	worktree string
}

func (m *Manager) Preview(ctx context.Context, id string, options ActivationOptions) (ActivationPreview, error) {
	unlock := m.lockWorkspace(id)
	defer unlock()
	workspace, err := m.loadWorkspace(id)
	if err != nil {
		return ActivationPreview{}, err
	}
	changes, err := m.scanChanges(ctx, workspace)
	if err != nil {
		return ActivationPreview{}, err
	}
	selected, err := selectedChanges(options.SelectedPackages, changes)
	if err != nil {
		return ActivationPreview{}, err
	}
	preview := ActivationPreview{WorkspaceID: id}
	for _, change := range changes {
		m.repositoryMu.RLock()
		repository, inspectErr := m.inspectRepository(change.PackageID)
		m.repositoryMu.RUnlock()
		if inspectErr != nil {
			return ActivationPreview{}, inspectErr
		}
		preview.Packages = append(preview.Packages, ActivationPackagePreview{
			PackageID:       change.PackageID,
			Selected:        selected[change.PackageID],
			BaseCommit:      change.Base,
			SharedCommit:    repository.Head,
			RequiresMerge:   change.Base != repository.Head,
			ActivationReady: repository.ActivationReady && repository.Clean,
			RemoteName:      repository.RemoteName,
			RemoteURL:       repository.RemoteURL,
			Files:           change.Files,
		})
	}
	m.log("development activation preview", workspace, "package_count", len(preview.Packages), "result_state", "previewed")
	return preview, nil
}

func (m *Manager) Activate(ctx context.Context, id string, options ActivationOptions) (result ActivationResult, returnErr error) {
	unlock := m.lockWorkspace(id)
	defer unlock()
	workspace, err := m.loadWorkspace(id)
	if err != nil {
		return ActivationResult{}, err
	}
	result = ActivationResult{WorkspaceID: id, Status: "failed"}
	defer func() {
		workspace.LastActivationResult = &result
		workspace.LastActivationStatus = result.Status
		workspace.LastActivationAt = time.Now().UTC()
		workspace.UpdatedAt = workspace.LastActivationAt
		if saveErr := m.saveWorkspace(workspace); saveErr != nil {
			returnErr = errors.Join(returnErr, saveErr)
		}
	}()
	if strings.TrimSpace(options.Description) == "" {
		return result, errors.New("activation description is required")
	}
	if workspace.ActiveSandboxID != "" {
		if err := m.driver.Pause(ctx, workspace.ActiveSandboxID); err != nil {
			return result, err
		}
		workspace.WritesPaused = true
	}
	workspace.State, workspace.ActivationActive = StateActivating, true
	workspace.UpdatedAt = time.Now().UTC()
	_ = m.saveWorkspace(workspace)
	defer func() {
		workspace.ActivationActive, workspace.WritesPaused = false, false
		if workspace.ActiveSandboxID != "" {
			_ = m.driver.Resume(context.Background(), workspace.ActiveSandboxID)
		}
		if workspace.State == StateActivating {
			workspace.State = StateReady
		}
	}()

	changes, err := m.scanChanges(ctx, workspace)
	if err != nil {
		return result, err
	}
	selected, err := selectedChanges(options.SelectedPackages, changes)
	if err != nil {
		return result, err
	}
	if len(changes) == 0 {
		result.Success, result.Status = true, "not-committed"
		return result, nil
	}

	authorName, authorEmail := m.activationAuthor(workspace, options)
	activationRoot := filepath.Join(m.config.RuntimeRoot, "activation")
	if err := os.MkdirAll(activationRoot, 0o700); err != nil {
		return result, err
	}
	m.repositoryMu.Lock()
	defer m.repositoryMu.Unlock()
	prepared := []preparedActivation{}
	defer func() {
		for _, item := range prepared {
			if item.worktree == "" {
				continue
			}
			shared, _ := m.packageRoot(item.changes.PackageID)
			_, _ = gitCommand(context.Background(), shared, nil, "worktree", "remove", "--force", item.worktree)
			_, _ = gitCommand(context.Background(), shared, nil, "worktree", "prune")
			_ = os.RemoveAll(filepath.Dir(item.worktree))
		}
	}()

	conflicted, failed := false, false
	for _, change := range changes {
		message := options.Description
		if override := strings.TrimSpace(options.PackageMessages[change.PackageID]); override != "" {
			message = override
		}
		if !selected[change.PackageID] {
			result.Packages = append(result.Packages, ActivationPackageResult{PackageID: change.PackageID, Status: "not-committed", CommitMessage: message})
			continue
		}
		item, prepareErr := m.preparePackage(ctx, workspace, change, message, authorName, authorEmail, options.Metadata, activationRoot)
		prepared = append(prepared, item)
		result.Packages = append(result.Packages, item.result)
		if prepareErr != nil {
			conflicted = conflicted || item.result.Status == "conflicted"
			failed = failed || item.result.Status != "conflicted"
		}
	}
	if conflicted || failed {
		workspace.ConflictedPackages = nil
		for _, item := range result.Packages {
			if item.Status == "conflicted" {
				workspace.ConflictedPackages = append(workspace.ConflictedPackages, item.PackageID)
			}
		}
		sort.Strings(workspace.ConflictedPackages)
		if conflicted {
			workspace.State, result.Status = StateConflicted, "conflicted"
			return result, errors.New("activation has Git conflicts")
		}
		result.Status = "failed"
		return result, errors.New("one or more packages failed activation")
	}

	bases, err := readBases(filepath.Join(m.workspaceRoot(workspace), "bases.toml"))
	if err != nil {
		return result, err
	}
	byID := map[string]ActivationPackageResult{}
	for index := range prepared {
		item := &prepared[index]
		if item.result.Status == "not-committed-clean" {
			byID[item.changes.PackageID] = item.result
			continue
		}
		shared, _ := m.packageRoot(item.changes.PackageID)
		head, headErr := m.repositoryHead(item.changes.PackageID)
		repository, inspectErr := m.inspectRepository(item.changes.PackageID)
		if headErr != nil || inspectErr != nil || head != item.current || !repository.Clean {
			item.result.Status, item.result.Error = "failed", "shared package changed during activation"
			failed = true
			byID[item.changes.PackageID] = item.result
			continue
		}
		if output, resetErr := gitCommand(ctx, shared, nil, "reset", "--hard", item.commit); resetErr != nil {
			item.result.Status, item.result.Error = "failed", output
			failed = true
			byID[item.changes.PackageID] = item.result
			continue
		}
		item.result.Status, item.result.ResultingHead = "committed", item.commit
		if err := m.synchronizePrivatePackage(ctx, workspace, item.changes.PackageID, item.commit); err != nil {
			item.result.Status, item.result.Error = "failed", err.Error()
			failed = true
		} else {
			bases.Packages[item.changes.PackageID] = baseDocument{BaseCommit: item.commit}
		}
		byID[item.changes.PackageID] = item.result
	}
	for index := range result.Packages {
		if item, ok := byID[result.Packages[index].PackageID]; ok {
			result.Packages[index] = item
		}
	}
	if err := writeBases(filepath.Join(m.workspaceRoot(workspace), "bases.toml"), bases); err != nil {
		return result, err
	}
	workspace.ConflictedPackages = nil
	result.Success, result.Status = !failed, "committed"
	if failed {
		result.Status = "failed"
		return result, errors.New("one or more packages failed activation")
	}
	return result, nil
}

func (m *Manager) scanChanges(ctx context.Context, workspace Workspace) ([]packageChanges, error) {
	bases, err := readBases(filepath.Join(m.workspaceRoot(workspace), "bases.toml"))
	if err != nil {
		return nil, err
	}
	result := []packageChanges{}
	for _, id := range sortedBaseIDs(bases) {
		base := bases.Packages[id]
		private, err := m.privatePackageRoot(workspace, id)
		if err != nil {
			return nil, err
		}
		index, err := os.CreateTemp(filepath.Join(m.workspaceRoot(workspace), "runtime"), ".activation-index-*")
		if err != nil {
			return nil, err
		}
		indexPath := index.Name()
		_ = index.Close()
		_ = os.Remove(indexPath)
		environment := []string{"GIT_INDEX_FILE=" + indexPath}
		if output, commandErr := gitCommand(ctx, private, environment, "read-tree", base.BaseCommit); commandErr != nil {
			_ = os.Remove(indexPath)
			return nil, fmt.Errorf("read development package base %s: %w: %s", id, commandErr, output)
		}
		if output, commandErr := gitCommand(ctx, private, environment, "add", "-A", "-f", "--", "."); commandErr != nil {
			_ = os.Remove(indexPath)
			return nil, fmt.Errorf("scan development package %s: %w: %s", id, commandErr, output)
		}
		status, commandErr := gitCommand(ctx, private, environment, "diff", "--cached", "--name-status", "-z", "--find-renames", base.BaseCommit)
		if commandErr != nil {
			_ = os.Remove(indexPath)
			return nil, fmt.Errorf("inspect development package %s: %w: %s", id, commandErr, status)
		}
		files := parseNameStatus(status)
		if len(files) == 0 {
			_ = os.Remove(indexPath)
			continue
		}
		tree, commandErr := gitOutputContext(ctx, private, environment, "write-tree")
		_ = os.Remove(indexPath)
		if commandErr != nil {
			return nil, fmt.Errorf("write development package tree %s: %w", id, commandErr)
		}
		m.repositoryMu.RLock()
		shared, _ := m.repositoryHead(id)
		m.repositoryMu.RUnlock()
		result = append(result, packageChanges{PackageID: id, Base: base.BaseCommit, Shared: shared, Tree: tree, Files: files})
	}
	return result, nil
}

func parseNameStatus(value string) []ActivationFile {
	fields := strings.Split(value, "\x00")
	result := []ActivationFile{}
	for index := 0; index < len(fields) && fields[index] != ""; {
		status := fields[index]
		index++
		if index >= len(fields) {
			break
		}
		path := fields[index]
		index++
		change := "modified"
		switch status[0] {
		case 'A':
			change = "new"
		case 'D':
			change = "deleted"
		case 'R', 'C':
			if index >= len(fields) {
				break
			}
			newPath := fields[index]
			index++
			change, path = "renamed from "+path, newPath
		}
		result = append(result, ActivationFile{Path: path, Change: change})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}

func (m *Manager) preparePackage(ctx context.Context, workspace Workspace, changes packageChanges, message, authorName, authorEmail string, metadata map[string]string, activationRoot string) (preparedActivation, error) {
	result := ActivationPackageResult{PackageID: changes.PackageID, Status: "failed", CommitMessage: message}
	repository, err := m.inspectRepository(changes.PackageID)
	if err != nil || !repository.ActivationReady || !repository.Clean {
		if err == nil {
			err = errors.New("shared package is not activation-ready and clean")
		}
		result.Error = err.Error()
		return preparedActivation{changes: changes, result: result}, err
	}
	result.PreviousHead = repository.Head
	private, err := m.privatePackageRoot(workspace, changes.PackageID)
	if err != nil {
		result.Error = err.Error()
		return preparedActivation{changes: changes, result: result}, err
	}
	commitMessage := activationCommitMessage(message, workspace.WorkspaceID, metadata)
	messageFile, err := os.CreateTemp(filepath.Join(m.workspaceRoot(workspace), "runtime"), ".activation-message-*")
	if err != nil {
		return preparedActivation{changes: changes, result: result}, err
	}
	messagePath := messageFile.Name()
	defer os.Remove(messagePath)
	if _, err := messageFile.WriteString(commitMessage); err != nil {
		_ = messageFile.Close()
		return preparedActivation{changes: changes, result: result}, err
	}
	if err := messageFile.Close(); err != nil {
		return preparedActivation{changes: changes, result: result}, err
	}
	identity := gitIdentity(authorName, authorEmail)
	privateCommit, err := gitOutputContext(ctx, private, identity, "commit-tree", changes.Tree, "-p", changes.Base, "-F", messagePath)
	if err != nil {
		result.Error = err.Error()
		return preparedActivation{changes: changes, result: result}, err
	}
	shared, _ := m.packageRoot(changes.PackageID)
	if output, fetchErr := gitCommand(ctx, shared, nil, "fetch", "--no-tags", filepath.Join(private, ".git"), privateCommit); fetchErr != nil {
		result.Error = output
		return preparedActivation{changes: changes, result: result}, fetchErr
	}
	stage, err := os.MkdirTemp(activationRoot, "package-")
	if err != nil {
		return preparedActivation{changes: changes, result: result}, err
	}
	worktree := filepath.Join(stage, "merge")
	if output, worktreeErr := gitCommand(ctx, shared, nil, "worktree", "add", "--detach", worktree, repository.Head); worktreeErr != nil {
		_ = os.RemoveAll(stage)
		result.Error = output
		return preparedActivation{changes: changes, result: result}, worktreeErr
	}
	if output, cherryErr := gitCommand(ctx, worktree, identity, "cherry-pick", privateCommit); cherryErr != nil {
		conflictsText, _ := gitOutputContext(ctx, worktree, nil, "diff", "--name-only", "--diff-filter=U")
		conflicts := strings.Fields(conflictsText)
		if len(conflicts) == 0 {
			conflicts = []string{"Git merge conflict"}
		}
		result.Status, result.Conflicts, result.Error = "conflicted", conflicts, output
		if preserveErr := m.preserveConflict(ctx, workspace, changes.PackageID, repository.Head, worktree); preserveErr != nil {
			result.Error += "; preserve conflict: " + preserveErr.Error()
		}
		return preparedActivation{changes: changes, result: result, current: repository.Head, worktree: worktree}, cherryErr
	}
	commit, err := gitOutputContext(ctx, worktree, nil, "rev-parse", "HEAD")
	if err != nil {
		result.Error = err.Error()
		return preparedActivation{changes: changes, result: result, current: repository.Head, worktree: worktree}, err
	}
	result.Status = "prepared"
	return preparedActivation{changes: changes, result: result, current: repository.Head, commit: commit, worktree: worktree}, nil
}

func (m *Manager) preserveConflict(ctx context.Context, workspace Workspace, id, sharedHead, worktree string) error {
	private, err := m.privatePackageRoot(workspace, id)
	if err != nil {
		return err
	}
	shared, _ := m.packageRoot(id)
	if output, err := gitCommand(ctx, private, nil, "fetch", "--no-tags", shared, sharedHead); err != nil {
		return fmt.Errorf("fetch conflict base: %w: %s", err, output)
	}
	if output, err := gitCommand(ctx, private, nil, "reset", "--mixed", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("reset conflict base: %w: %s", err, output)
	}
	if err := replaceWorktree(ctx, worktree, private); err != nil {
		return err
	}
	bases, err := readBases(filepath.Join(m.workspaceRoot(workspace), "bases.toml"))
	if err != nil {
		return err
	}
	bases.Packages[id] = baseDocument{BaseCommit: sharedHead, Conflicted: true}
	return writeBases(filepath.Join(m.workspaceRoot(workspace), "bases.toml"), bases)
}

func (m *Manager) synchronizePrivatePackage(ctx context.Context, workspace Workspace, id, commit string) error {
	private, err := m.privatePackageRoot(workspace, id)
	if err != nil {
		return err
	}
	shared, _ := m.packageRoot(id)
	if output, err := gitCommand(ctx, private, nil, "fetch", "--no-tags", shared, commit); err != nil {
		return fmt.Errorf("fetch activated package: %w: %s", err, output)
	}
	if output, err := gitCommand(ctx, private, nil, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("reset activated package: %w: %s", err, output)
	}
	if output, err := gitCommand(ctx, private, nil, "clean", "-fdx"); err != nil {
		return fmt.Errorf("clean activated package: %w: %s", err, output)
	}
	return nil
}

func (m *Manager) privatePackageRoot(workspace Workspace, id string) (string, error) {
	parts := strings.Split(id, "/")
	if len(parts) != 2 || !safePackageSegment(parts[0]) || !safePackageSegment(parts[1]) {
		return "", errors.New("package ID must be <namespace>/<repository>")
	}
	root, err := canonicalDirectory(filepath.Join(workspace.SourcePath, filepath.FromSlash(id)))
	if err != nil {
		return "", err
	}
	if !beneath(root, workspace.SourcePath) {
		return "", errors.New("private package escaped workspace source")
	}
	return root, nil
}

func selectedChanges(requested []string, changes []packageChanges) (map[string]bool, error) {
	available := map[string]bool{}
	for _, change := range changes {
		available[change.PackageID] = true
	}
	selected := map[string]bool{}
	if len(requested) == 0 {
		for id := range available {
			selected[id] = true
		}
		return selected, nil
	}
	for _, id := range requested {
		if !available[id] {
			return nil, fmt.Errorf("selected package %s has no changes", id)
		}
		selected[id] = true
	}
	return selected, nil
}

func (m *Manager) activationAuthor(workspace Workspace, options ActivationOptions) (string, string) {
	name, email := strings.TrimSpace(options.AuthorName), strings.TrimSpace(options.AuthorEmail)
	if name == "" {
		name, _ = gitOutputContext(context.Background(), workspace.PersistentHomePath, nil, "config", "--file", filepath.Join(workspace.PersistentHomePath, ".gitconfig"), "user.name")
	}
	if email == "" {
		email, _ = gitOutputContext(context.Background(), workspace.PersistentHomePath, nil, "config", "--file", filepath.Join(workspace.PersistentHomePath, ".gitconfig"), "user.email")
	}
	if name == "" {
		name = workspace.OwnerUserID
	}
	if email == "" {
		email = workspace.OwnerUserID + "@development.local"
	}
	return name, email
}

func activationCommitMessage(message, workspaceID string, metadata map[string]string) string {
	value := message + "\n\nThe8020-Workspace: " + workspaceID
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value += "\nThe8020-" + sanitizeFooterKey(key) + ": " + strings.ReplaceAll(metadata[key], "\n", " ")
	}
	return value + "\n"
}

func sanitizeFooterKey(value string) string {
	parts := strings.FieldsFunc(value, func(character rune) bool {
		return (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9')
	})
	if len(parts) == 0 {
		return "Metadata"
	}
	return strings.Join(parts, "-")
}
