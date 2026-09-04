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

	"the8020/kernel/deployment"
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

type sandboxPackageScan struct {
	PackageID string
	Base      string
}

const (
	// Indexes are derivable sandbox-lifetime state. Keeping them in the
	// ephemeral mount makes later previews incremental without persisting a
	// second source of truth.
	sandboxActivationIndexRoot = "/tmp/.the8020-activation-index-v2"
	activationScanSectionLimit = 4 << 20
)

// activationScanWriter consumes one NUL-terminated payload per package. A
// preview carries raw/numstat records; capture only carries a fixed changed
// marker because activation and checkpointing do not return file statistics.
// The script appends one extra NUL, so the first empty path record ends a
// section; Git paths themselves cannot be empty.
type activationScanWriter struct {
	packages []sandboxPackageScan
	changes  []packageChanges
	section  []byte
	index    int
	details  bool
	err      error
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

	// Candidate sources must be mountable by the trusted schema evaluator.
	// Kernel runtime state is deliberately protected from every sandbox mount.
	activationRoot, err := os.MkdirTemp(m.config.Root, ".activation-")
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

	hook := m.schemaDeployment()
	preparedSchema := false
	sourceSwitched := false
	if hook != nil {
		candidates := make([]deployment.Candidate, 0, len(prepared))
		for _, item := range prepared {
			if item.commit == "" || len(item.worktrees) == 0 {
				continue
			}
			candidates = append(candidates, deployment.Candidate{
				PackageID: item.changes.PackageID,
				Root:      item.worktrees[len(item.worktrees)-1],
				Commit:    item.commit,
			})
		}
		if err := hook.Prepare(ctx, candidates); err != nil {
			return result, fmt.Errorf("prepare database schema: %w", err)
		}
		preparedSchema = len(candidates) > 0
		defer func() {
			if preparedSchema && !sourceSwitched {
				returnErr = errors.Join(returnErr, hook.Complete(context.Background(), false))
			}
		}()
	}

	byID := map[string]ActivationPackageResult{}
	switched := []preparedActivation{}
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
		sourceSwitched = true
		switched = append(switched, *item)
		item.result.Status, item.result.ResultingHead = "committed", item.commit
		byID[item.changes.PackageID] = item.result
	}
	for index := range result.Packages {
		if item, ok := byID[result.Packages[index].PackageID]; ok {
			result.Packages[index] = item
		}
	}
	if failed {
		for _, item := range switched {
			shared, _ := m.packageRoot(item.changes.PackageID)
			_, _ = gitCommand(context.WithoutCancel(ctx), shared, nil, "reset", "--hard", item.current)
		}
		if preparedSchema {
			_ = hook.Complete(context.WithoutCancel(ctx), false)
			preparedSchema = false
		}
		result.Status = "failed"
		return result, errors.New("one or more packages failed activation")
	}
	if preparedSchema {
		if err := hook.Complete(ctx, true); err != nil {
			return result, fmt.Errorf("complete package activation: %w", err)
		}
		preparedSchema = false
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
	return m.scanPackageChanges(ctx, sandbox, true)
}

func (m *Manager) scanPackageChanges(ctx context.Context, sandbox Sandbox, details bool) ([]packageChanges, error) {
	if _, active := m.owned.Load(sandbox.SandboxID); !active {
		return nil, errors.New("development sandbox is not running")
	}
	packages := []sandboxPackageScan{}
	for _, id := range packageDirectories(m.config.PackagesRoot) {
		m.repositoryMu.RLock()
		head, headErr := m.repositoryHead(id)
		m.repositoryMu.RUnlock()
		if headErr != nil || head == "" {
			continue
		}
		packages = append(packages, sandboxPackageScan{PackageID: id, Base: head})
	}
	if len(packages) == 0 {
		return []packageChanges{}, nil
	}
	output := &activationScanWriter{packages: packages, changes: []packageChanges{}, details: details}
	execErr := m.driver.ExecCommand(ctx, sandbox.SandboxID, []string{"/bin/sh", "-c", sandboxScanCommand(packages, details)}, nil, output)
	if execErr != nil {
		return nil, fmt.Errorf("inspect development packages: %w", execErr)
	}
	if err := output.finish(); err != nil {
		return nil, fmt.Errorf("decode development package scan: %w", err)
	}
	return output.changes, nil
}

func (m *Manager) captureChanges(ctx context.Context, sandbox Sandbox, root string) ([]packageChanges, error) {
	changes, err := m.scanPackageChanges(ctx, sandbox, false)
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
		command := sandboxCachedIndexCommand(change.PackageID, change.Base, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", change.Base)
		exportErr := m.driver.ExecCommand(ctx, sandbox.SandboxID, []string{"/bin/sh", "-c", command}, nil, patch)
		closeErr := patch.Close()
		if exportErr != nil || closeErr != nil {
			return nil, fmt.Errorf("capture development package %s: %w", change.PackageID, errors.Join(exportErr, closeErr))
		}
	}
	return changes, nil
}

func sandboxScanCommand(packages []sandboxPackageScan, details bool) string {
	var command strings.Builder
	command.WriteString(`set -eu
umask 077
activation_package=
trap 'activation_status=$?; trap - 0; if [ "$activation_status" -ne 0 ]; then printf "activation scan failed for package %s\n" "$activation_package" >&2; fi; exit "$activation_status"' 0
reset_activation_index() {
	rm -f -- "$activation_index" "$activation_index.lock" "$activation_base_file" "$activation_added_file" "$activation_ignored_file"
	GIT_INDEX_FILE="$activation_index" git -C "$activation_repository" read-tree "$activation_base" >/dev/null
	GIT_INDEX_FILE="$activation_index" git -C "$activation_repository" add -A -- . >/dev/null
	printf '%s\n' "$activation_base" > "$activation_base_file"
}
refresh_activation_index() {
	rm -f -- "$activation_added_file" "$activation_ignored_file"
	GIT_INDEX_FILE="$activation_index" git -C "$activation_repository" diff --cached --name-only --diff-filter=A -z "$activation_base" > "$activation_added_file"
	if [ -s "$activation_added_file" ]; then
		activation_ignore_status=0
		git -C "$activation_repository" check-ignore --no-index --stdin -z < "$activation_added_file" > "$activation_ignored_file" || activation_ignore_status=$?
		if [ "$activation_ignore_status" -gt 1 ]; then
			return "$activation_ignore_status"
		fi
		if [ -s "$activation_ignored_file" ]; then
			GIT_INDEX_FILE="$activation_index" git -C "$activation_repository" update-index --force-remove -z --stdin < "$activation_ignored_file"
		fi
	fi
	rm -f -- "$activation_added_file" "$activation_ignored_file"
	GIT_INDEX_FILE="$activation_index" git -C "$activation_repository" add -A -- . >/dev/null
}
prepare_activation_index() {
	activation_repository=$1
	activation_index=$2
	activation_base_file=$3
	activation_base=$4
	activation_added_file=$activation_index.added
	activation_ignored_file=$activation_index.ignored
	activation_cached_base=
	if [ -f "$activation_base_file" ]; then
		IFS= read -r activation_cached_base < "$activation_base_file" || activation_cached_base=
	fi
	if [ "$activation_cached_base" != "$activation_base" ] || [ ! -f "$activation_index" ]; then
		reset_activation_index
	elif ! refresh_activation_index; then
		reset_activation_index
	fi
}
`)
	cacheDirectories := map[string]bool{sandboxActivationIndexRoot: true}
	for _, item := range packages {
		cacheDirectories[sandboxActivationIndexRoot+"/"+item.PackageID] = true
	}
	directories := make([]string, 0, len(cacheDirectories))
	for directory := range cacheDirectories {
		directories = append(directories, directory)
	}
	sort.Strings(directories)
	command.WriteString("mkdir -p --")
	for _, directory := range directories {
		command.WriteByte(' ')
		command.WriteString(shellQuote(directory))
	}
	command.WriteByte('\n')
	for _, item := range packages {
		index, baseFile := sandboxIndexPaths(item.PackageID)
		repository := "/workspace/packages/" + item.PackageID
		command.WriteString("activation_package=" + shellQuote(item.PackageID) + "\n")
		command.WriteString("prepare_activation_index " + shellQuote(repository) + " " + shellQuote(index) + " " + shellQuote(baseFile) + " " + shellQuote(item.Base) + "\n")
		if details {
			command.WriteString("GIT_INDEX_FILE=" + shellQuote(index) + " GIT_OPTIONAL_LOCKS=0 git -C " + shellQuote(repository) + " diff --cached --raw --numstat -z --find-renames --no-ext-diff --no-textconv " + shellQuote(item.Base) + "\n")
		} else {
			command.WriteString("activation_diff_status=0\n")
			command.WriteString("GIT_INDEX_FILE=" + shellQuote(index) + " GIT_OPTIONAL_LOCKS=0 git -C " + shellQuote(repository) + " diff --cached --quiet --no-renames --no-ext-diff --no-textconv " + shellQuote(item.Base) + " || activation_diff_status=$?\n")
			command.WriteString("if [ \"$activation_diff_status\" -eq 1 ]; then printf 'changed\\0'; elif [ \"$activation_diff_status\" -ne 0 ]; then exit \"$activation_diff_status\"; fi\n")
		}
		command.WriteString("printf '\\0'\n")
	}
	return command.String()
}

func sandboxIndexPaths(id string) (string, string) {
	directory := sandboxActivationIndexRoot + "/" + id
	return directory + "/index", directory + "/base"
}

func sandboxCachedIndexCommand(id, base string, arguments ...string) string {
	repository := "/workspace/packages/" + id
	index, baseFile := sandboxIndexPaths(id)
	parts := []string{"git", "-C", shellQuote(repository)}
	for _, argument := range arguments {
		parts = append(parts, shellQuote(argument))
	}
	return "set -eu\n" +
		"cached_base=\n" +
		"if [ -f " + shellQuote(baseFile) + " ]; then IFS= read -r cached_base < " + shellQuote(baseFile) + " || cached_base=; fi\n" +
		"if [ \"$cached_base\" != " + shellQuote(base) + " ] || [ ! -f " + shellQuote(index) + " ]; then\n" +
		"\tprintf 'activation index is unavailable for package %s\\n' " + shellQuote(id) + " >&2\n" +
		"\texit 1\n" +
		"fi\n" +
		"export GIT_INDEX_FILE=" + shellQuote(index) + " GIT_OPTIONAL_LOCKS=0\n" +
		"exec " + strings.Join(parts, " ")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (w *activationScanWriter) Write(value []byte) (int, error) {
	for _, character := range value {
		if w.err != nil {
			continue
		}
		if w.index >= len(w.packages) {
			w.err = errors.New("activation scan returned unexpected trailing output")
			continue
		}
		if character == 0 && (len(w.section) == 0 || w.section[len(w.section)-1] == 0) {
			w.finishSection()
			continue
		}
		if len(w.section) >= activationScanSectionLimit {
			w.err = fmt.Errorf("activation scan output for package %s exceeds %d bytes", w.packages[w.index].PackageID, activationScanSectionLimit)
			continue
		}
		w.section = append(w.section, character)
	}
	return len(value), nil
}

func (w *activationScanWriter) finishSection() {
	item := w.packages[w.index]
	if !w.details {
		if len(w.section) > 0 && string(w.section) != "changed\x00" {
			w.err = fmt.Errorf("package %s: Git change marker is malformed", item.PackageID)
			return
		}
		if len(w.section) > 0 {
			w.changes = append(w.changes, packageChanges{PackageID: item.PackageID, Base: item.Base, Shared: item.Base})
		}
		w.index++
		w.section = w.section[:0]
		return
	}
	files, added, removed, err := parseRawNumstat(w.section)
	if err != nil {
		w.err = fmt.Errorf("package %s: %w", item.PackageID, err)
		return
	}
	if len(files) > 0 {
		w.changes = append(w.changes, packageChanges{PackageID: item.PackageID, Base: item.Base, Shared: item.Base, Files: files, AddedRows: added, RemovedRows: removed})
	}
	w.index++
	w.section = w.section[:0]
}

func (w *activationScanWriter) finish() error {
	if w.err != nil {
		return w.err
	}
	if w.index != len(w.packages) || len(w.section) != 0 {
		return fmt.Errorf("activation scan returned %d of %d package records", w.index, len(w.packages))
	}
	return nil
}

func parseRawNumstat(value []byte) ([]ActivationFile, int, int, error) {
	if len(value) == 0 {
		return []ActivationFile{}, 0, 0, nil
	}
	fields := strings.Split(string(value), "\x00")
	if fields[len(fields)-1] != "" {
		return nil, 0, 0, errors.New("Git scan output is not NUL-terminated")
	}
	fields = fields[:len(fields)-1]
	files := []ActivationFile{}
	index := 0
	for index < len(fields) && strings.HasPrefix(fields[index], ":") {
		header := strings.Fields(strings.TrimPrefix(fields[index], ":"))
		index++
		if len(header) != 5 || header[4] == "" || index >= len(fields) || fields[index] == "" {
			return nil, 0, 0, errors.New("Git raw diff record is malformed")
		}
		status := header[4][0]
		path := fields[index]
		index++
		change := "modified"
		switch status {
		case 'A':
			change = "new"
		case 'D':
			change = "deleted"
		case 'R', 'C':
			if index >= len(fields) || fields[index] == "" {
				return nil, 0, 0, errors.New("Git rename record is malformed")
			}
			change, path = "renamed from "+path, fields[index]
			index++
		}
		files = append(files, ActivationFile{Path: path, Change: change})
	}
	added, removed := 0, 0
	for index < len(fields) {
		record := strings.SplitN(fields[index], "\t", 3)
		index++
		if len(record) != 3 {
			return nil, 0, 0, errors.New("Git numstat record is malformed")
		}
		for column, total := range []struct {
			value string
			total *int
		}{{record[0], &added}, {record[1], &removed}} {
			if total.value == "-" {
				continue
			}
			count, err := strconv.Atoi(total.value)
			if err != nil || count < 0 {
				return nil, 0, 0, fmt.Errorf("Git numstat column %d is invalid", column+1)
			}
			*total.total += count
		}
		if record[2] == "" {
			if index+1 >= len(fields) || fields[index] == "" || fields[index+1] == "" {
				return nil, 0, 0, errors.New("Git numstat rename record is malformed")
			}
			index += 2
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, added, removed, nil
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
