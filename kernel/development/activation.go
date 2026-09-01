package development

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type packageChanges struct {
	PackageID   string
	Base        string
	Shared      string
	Files       []ActivationFile
	AddedRows   int
	RemovedRows int
	PatchPath   string
}

type preparedActivation struct {
	changes   packageChanges
	result    ActivationPackageResult
	current   string
	commit    string
	worktrees []string
	stage     string
}

type deferredOverlayResetKey struct{}

func deferOverlayReset(ctx context.Context) bool {
	value, _ := ctx.Value(deferredOverlayResetKey{}).(bool)
	return value
}

func (m *Manager) Preview(ctx context.Context, userID string, options ActivationOptions) (ActivationPreview, error) {
	unlock := m.lockUser(userID)
	defer unlock()
	sandbox, err := m.loadSandbox(userID)
	if err != nil {
		return ActivationPreview{}, err
	}
	changes, err := m.scanChanges(ctx, sandbox)
	if err != nil {
		return ActivationPreview{}, err
	}
	selected, err := selectedChanges(options.SelectedPackages, changes)
	if err != nil {
		return ActivationPreview{}, err
	}
	preview := ActivationPreview{Packages: []ActivationPackagePreview{}}
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
			ChangedFiles:    len(change.Files),
			AddedRows:       change.AddedRows,
			RemovedRows:     change.RemovedRows,
			Files:           change.Files,
		})
	}
	m.log("development activation preview", sandbox, "package_count", len(preview.Packages), "result_state", "previewed")
	return preview, nil
}

func (m *Manager) Activate(ctx context.Context, userID string, options ActivationOptions) (result ActivationResult, returnErr error) {
	unlock := m.lockUser(userID)
	defer unlock()
	sandbox, err := m.loadSandbox(userID)
	if err != nil {
		return ActivationResult{}, err
	}
	result = ActivationResult{Status: "failed", Packages: []ActivationPackageResult{}}
	defer func() {
		sandbox.LastActivationResult = &result
		sandbox.LastActivationStatus = result.Status
		sandbox.LastActivationAt = time.Now().UTC()
		sandbox.UpdatedAt = sandbox.LastActivationAt
		if saveErr := m.saveSandbox(sandbox); saveErr != nil {
			returnErr = errors.Join(returnErr, saveErr)
		}
	}()
	if strings.TrimSpace(options.Description) == "" {
		return result, errors.New("activation description is required")
	}
	if _, active := m.owned.Load(sandbox.SandboxID); !active {
		return result, errors.New("development sandbox is not running")
	}
	sandbox.State, sandbox.ActivationActive = StateActivating, true
	sandbox.UpdatedAt = time.Now().UTC()
	_ = m.saveSandbox(sandbox)
	defer func() {
		sandbox.ActivationActive, sandbox.WritesPaused = false, false
		if sandbox.State == StateActivating {
			sandbox.State = StateReady
		}
	}()

	activationRoot, err := os.MkdirTemp(m.config.RuntimeRoot, "activation-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(activationRoot)
	changes, err := m.captureChanges(ctx, sandbox, filepath.Join(activationRoot, "captured"))
	if err != nil {
		return result, err
	}
	selected, err := selectedChanges(options.SelectedPackages, changes)
	if err != nil {
		return result, err
	}
	if len(changes) == 0 {
		result.Success, result.Status, result.OverlayReset = true, "not-committed", true
		return result, nil
	}
	if err := m.driver.Pause(ctx, sandbox.SandboxID); err != nil {
		return result, fmt.Errorf("pause development sandbox for activation: %w", err)
	}
	paused := true
	sandbox.WritesPaused = true
	_ = m.saveSandbox(sandbox)
	defer func() {
		if paused {
			_ = m.driver.Resume(context.Background(), sandbox.SandboxID)
		}
	}()

	authorName, authorEmail := m.activationAuthor(sandbox, options)
	m.repositoryMu.Lock()
	defer m.repositoryMu.Unlock()
	prepared := []preparedActivation{}
	defer func() {
		for _, item := range prepared {
			m.cleanupPreparedActivation(item)
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
		item, prepareErr := m.preparePackage(ctx, sandbox, change, message, authorName, authorEmail, options.Metadata, activationRoot)
		prepared = append(prepared, item)
		result.Packages = append(result.Packages, item.result)
		if prepareErr != nil {
			conflicted = conflicted || item.result.Status == "conflicted"
			failed = failed || item.result.Status != "conflicted"
		}
	}
	if conflicted || failed {
		sandbox.ConflictedPackages = nil
		for _, item := range result.Packages {
			if item.Status == "conflicted" {
				sandbox.ConflictedPackages = append(sandbox.ConflictedPackages, item.PackageID)
			}
		}
		sort.Strings(sandbox.ConflictedPackages)
		if conflicted {
			sandbox.State, result.Status = StateConflicted, "conflicted"
			return result, errors.New("activation has Git conflicts")
		}
		result.Status = "failed"
		return result, errors.New("one or more packages failed activation")
	}

	byID := map[string]ActivationPackageResult{}
	for index := range prepared {
		item := &prepared[index]
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
		byID[item.changes.PackageID] = item.result
	}
	for index := range result.Packages {
		if item, ok := byID[result.Packages[index].PackageID]; ok {
			result.Packages[index] = item
		}
	}
	if failed {
		result.Status = "failed"
		return result, errors.New("one or more packages failed activation")
	}

	remaining := make([]packageChanges, 0, len(changes))
	for _, change := range changes {
		if !selected[change.PackageID] {
			remaining = append(remaining, change)
		}
	}
	if err := m.persistCapturedOverlay(sandbox, remaining); err != nil {
		result.Status = "failed"
		return result, fmt.Errorf("preserve unselected development changes: %w", err)
	}
	sandbox.ConflictedPackages = nil
	result.Success, result.Status = true, "committed"
	if deferOverlayReset(ctx) {
		result.OverlayResetPending = true
		return result, nil
	}
	if err := m.resetOverlayLocked(ctx, &sandbox); err != nil {
		result.Success, result.Status = false, "committed-reset-failed"
		return result, fmt.Errorf("activated packages but failed to reset development overlay: %w", err)
	}
	paused = false
	result.OverlayReset = true
	return result, nil
}

func (m *Manager) scanChanges(ctx context.Context, sandbox Sandbox) ([]packageChanges, error) {
	if _, active := m.owned.Load(sandbox.SandboxID); !active {
		return nil, errors.New("development sandbox is not running")
	}
	result := []packageChanges{}
	for _, id := range packageDirectories(m.config.PackagesRoot) {
		m.repositoryMu.RLock()
		repository, inspectErr := m.inspectRepository(id)
		m.repositoryMu.RUnlock()
		if inspectErr != nil || repository.Head == "" {
			continue
		}
		status, err := m.driver.Exec(ctx, sandbox.SandboxID, sandboxIndexCommand(id, repository.Head, "diff", "--cached", "--name-status", "-z", "--find-renames", repository.Head))
		if err != nil {
			return nil, fmt.Errorf("inspect development package %s: %w", id, err)
		}
		files := parseNameStatus(string(status))
		if len(files) == 0 {
			continue
		}
		numstat, err := m.driver.Exec(ctx, sandbox.SandboxID, sandboxIndexCommand(id, repository.Head, "diff", "--cached", "--numstat", "-z", repository.Head))
		if err != nil {
			return nil, fmt.Errorf("measure development package %s: %w", id, err)
		}
		added, removed := parseNumstat(string(numstat))
		result = append(result, packageChanges{PackageID: id, Base: repository.Head, Shared: repository.Head, Files: files, AddedRows: added, RemovedRows: removed})
	}
	return result, nil
}

func (m *Manager) captureChanges(ctx context.Context, sandbox Sandbox, root string) ([]packageChanges, error) {
	changes, err := m.scanChanges(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	for index := range changes {
		change := &changes[index]
		change.PatchPath = filepath.Join(root, "patches", filepath.FromSlash(change.PackageID)+".patch")
		if err := os.MkdirAll(filepath.Dir(change.PatchPath), 0o700); err != nil {
			return nil, err
		}
		patch, err := os.OpenFile(change.PatchPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		command := sandboxIndexCommand(change.PackageID, change.Base, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", change.Base)
		exportErr := m.driver.ExecStream(ctx, sandbox.SandboxID, command, nil, patch)
		closeErr := patch.Close()
		if exportErr != nil || closeErr != nil {
			return nil, errors.Join(exportErr, closeErr)
		}
	}
	return changes, nil
}

func sandboxIndexCommand(id, base string, arguments ...string) string {
	repository := "/workspace/packages/" + id
	parts := []string{"git", "-C", shellQuote(repository)}
	for _, argument := range arguments {
		parts = append(parts, shellQuote(argument))
	}
	return "set -eu\n" +
		"index=$(mktemp /tmp/.the8020-activation-index.XXXXXX)\n" +
		"trap 'rm -f \"$index\"' EXIT\n" +
		"export GIT_INDEX_FILE=\"$index\"\n" +
		"git -C " + shellQuote(repository) + " read-tree " + shellQuote(base) + "\n" +
		"git -C " + shellQuote(repository) + " add -A -f -- .\n" +
		strings.Join(parts, " ")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func parseNameStatus(value string) []ActivationFile {
	fields := strings.Split(value, "\x00")
	result := []ActivationFile{}
	for index := 0; index < len(fields) && fields[index] != ""; {
		status := fields[index]
		index++
		if status == "" || index >= len(fields) {
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

func parseNumstat(value string) (int, int) {
	added, removed := 0, 0
	for _, record := range strings.Split(value, "\x00") {
		fields := strings.SplitN(record, "\t", 3)
		if len(fields) < 2 {
			continue
		}
		if count, err := strconv.Atoi(fields[0]); err == nil {
			added += count
		}
		if count, err := strconv.Atoi(fields[1]); err == nil {
			removed += count
		}
	}
	return added, removed
}

func (m *Manager) preparePackage(ctx context.Context, sandbox Sandbox, changes packageChanges, message, authorName, authorEmail string, metadata map[string]string, activationRoot string) (preparedActivation, error) {
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
	shared, _ := m.packageRoot(changes.PackageID)
	stage, err := os.MkdirTemp(activationRoot, "package-")
	if err != nil {
		return preparedActivation{changes: changes, result: result}, err
	}
	item := preparedActivation{changes: changes, result: result, current: repository.Head, stage: stage}
	baseWorktree := filepath.Join(stage, "base")
	if output, worktreeErr := gitCommand(ctx, shared, nil, "worktree", "add", "--detach", baseWorktree, changes.Base); worktreeErr != nil {
		item.result.Error = output
		return item, worktreeErr
	}
	item.worktrees = append(item.worktrees, baseWorktree)
	if output, applyErr := gitCommand(ctx, baseWorktree, nil, "apply", "--index", "--binary", "--whitespace=nowarn", changes.PatchPath); applyErr != nil {
		item.result.Error = output
		return item, applyErr
	}
	messagePath := filepath.Join(stage, "message.txt")
	if err := os.WriteFile(messagePath, []byte(activationCommitMessage(message, sandbox.SandboxID, metadata)), 0o600); err != nil {
		item.result.Error = err.Error()
		return item, err
	}
	identity := gitIdentity(authorName, authorEmail)
	if output, commitErr := gitCommand(ctx, baseWorktree, identity, "commit", "--no-gpg-sign", "--file", messagePath); commitErr != nil {
		item.result.Error = output
		return item, commitErr
	}
	privateCommit, err := gitOutputContext(ctx, baseWorktree, nil, "rev-parse", "HEAD")
	if err != nil {
		item.result.Error = err.Error()
		return item, err
	}
	if repository.Head == changes.Base {
		item.commit, item.result.Status = privateCommit, "prepared"
		return item, nil
	}
	mergeWorktree := filepath.Join(stage, "merge")
	if output, worktreeErr := gitCommand(ctx, shared, nil, "worktree", "add", "--detach", mergeWorktree, repository.Head); worktreeErr != nil {
		item.result.Error = output
		return item, worktreeErr
	}
	item.worktrees = append(item.worktrees, mergeWorktree)
	if output, cherryErr := gitCommand(ctx, mergeWorktree, identity, "cherry-pick", privateCommit); cherryErr != nil {
		conflictsText, _ := gitOutputContext(ctx, mergeWorktree, nil, "diff", "--name-only", "--diff-filter=U")
		conflicts := strings.Fields(conflictsText)
		if len(conflicts) == 0 {
			conflicts = []string{"Git merge conflict"}
		}
		item.result.Status, item.result.Conflicts, item.result.Error = "conflicted", conflicts, output
		return item, cherryErr
	}
	item.commit, err = gitOutputContext(ctx, mergeWorktree, nil, "rev-parse", "HEAD")
	if err != nil {
		item.result.Error = err.Error()
		return item, err
	}
	item.result.Status = "prepared"
	return item, nil
}

func (m *Manager) cleanupPreparedActivation(item preparedActivation) {
	shared, _ := m.packageRoot(item.changes.PackageID)
	for index := len(item.worktrees) - 1; index >= 0; index-- {
		_, _ = gitCommand(context.Background(), shared, nil, "worktree", "remove", "--force", item.worktrees[index])
	}
	_, _ = gitCommand(context.Background(), shared, nil, "worktree", "prune")
	_ = os.RemoveAll(item.stage)
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

func (m *Manager) activationAuthor(sandbox Sandbox, options ActivationOptions) (string, string) {
	name, email := strings.TrimSpace(options.AuthorName), strings.TrimSpace(options.AuthorEmail)
	if name == "" {
		name = sandbox.UserID
	}
	if email == "" {
		email = sandbox.UserID + "@development.local"
	}
	return name, email
}

func activationCommitMessage(message, sandboxID string, metadata map[string]string) string {
	values := map[string]string{"sandbox": sandboxID}
	for key, value := range metadata {
		values["metadata_"+sanitizeFooterKey(key)] = strings.ReplaceAll(value, "\n", " ")
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var appendix strings.Builder
	appendix.WriteString("# 80|20 activation metadata\n[the8020.activation]\n")
	for _, key := range keys {
		appendix.WriteString(strconv.Quote(key))
		appendix.WriteString(" = ")
		appendix.WriteString(strconv.Quote(values[key]))
		appendix.WriteByte('\n')
	}
	return strings.TrimSpace(message) + "\n\n" + appendix.String()
}

func sanitizeFooterKey(value string) string {
	parts := strings.FieldsFunc(value, func(character rune) bool {
		return (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9')
	})
	if len(parts) == 0 {
		return "metadata"
	}
	return strings.ToLower(strings.Join(parts, "_"))
}

func copyPatch(destination string, source string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	return errors.Join(copyErr, output.Close())
}
