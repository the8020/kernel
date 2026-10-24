package development

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func (m *Manager) activationStatus(sandbox Sandbox, d *workspace) (ActivationResult, error) {
	result := ActivationResult{Success: true, Status: "idle", Packages: []ActivationPackageResult{}}
	var attempt activationAttempt
	err := readJSON(filepath.Join(m.sandboxRoot(sandbox), "activation/active.json"), &attempt)
	if err == nil {
		result = attemptResult(attempt)
		if previous := sandbox.LastActivationResult; previous != nil && previous.AttemptID == attempt.ID && !previous.Success && previous.Error != "" {
			result.Error = previous.Error + "\n" + result.Error
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	root, err := os.OpenRoot(filepath.Join(d.storage, "upper"))
	if err != nil {
		return result, err
	}
	defer root.Close()
	names := map[string]bool{}
	readNames := func(directory *os.File, journals bool) error {
		defer directory.Close()
		entries, err := directory.ReadDir(129)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if len(entries) > 128 {
			result.InventoryTruncated, entries = true, entries[:128]
		}
		for _, entry := range entries {
			name := entry.Name()
			if journals {
				if !strings.HasSuffix(name, ".json") {
					continue
				}
				name = strings.TrimSuffix(name, ".json")
			}
			if name != attempt.ID {
				names[name] = true
			}
		}
		return nil
	}
	if directory, err := root.Open(".conflicts"); err == nil {
		if err := readNames(directory, false); err != nil {
			return result, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if directory, err := os.Open(filepath.Join(m.sandboxRoot(sandbox), "activation/retained")); err == nil {
		if err := readNames(directory, true); err != nil {
			return result, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	if len(ordered) > 128 {
		result.InventoryTruncated, ordered = true, ordered[:128]
	}
	for _, name := range ordered {
		state, recovery := "legacy-unresolved", ""
		var retained activationAttempt
		if err := readJSON(filepath.Join(m.sandboxRoot(sandbox), "activation/retained", name+".json"), &retained); err == nil && retained.ID == name && retained.Phase == "aborted" && retained.SandboxID == sandbox.SandboxID {
			state = "aborted-preserved"
			if retained.RetainedCleanup {
				recovery = "Partial cleanup may retain protected files under " + filepath.Join(d.storage, "snapshots/retire-"+name+"-*/staging") + "; ask the instance operator to export them before removing recovery records."
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
		result.Retained = append(result.Retained, ActivationRetained{AttemptID: name, Status: state, Worktree: "/workspace/packages/.conflicts/" + name, Recovery: recovery})
	}
	return result, nil
}

func attemptResult(attempt activationAttempt) ActivationResult {
	status := "recovery-required"
	switch attempt.Phase {
	case "staging", "captured":
		status = "staging"
	case "cleanup", "discarding":
		status = "cleanup-pending"
	}
	result := ActivationResult{AttemptID: attempt.ID, DeploymentID: attempt.TransactionID, Phase: attempt.Phase, Resumed: true, Status: status, Packages: []ActivationPackageResult{}}
	for _, item := range attempt.Packages {
		p := ActivationPackageResult{PackageID: item.PackageID, Status: status, Worktree: item.Worktree, PreviousHead: item.Previous, ResultingHead: item.Published}
		if item.Retired {
			p.Status, p.Worktree = "retired", ""
		}
		if item.Conflicted {
			p.Status, p.ConflictWorktree, result.Status = "conflicted", item.Worktree, "conflicted"
		}
		result.Packages = append(result.Packages, p)
	}
	result.Error = "Pending activation: use activate --continue --message 'Describe the changes', or activate --abort to preserve work and leave this attempt. Inspect with activate --status."
	return result
}

// Abort is a disposition, never a purge. Captures and resolution worktrees
// remain available; only a confirmed rollback or finished publication can exit.
func (m *Manager) retainAttempt(sandbox Sandbox, journal string, attempt *activationAttempt) error {
	if err := removeValidation(journal, attempt); err != nil {
		return err
	}
	attempt.RetainedCleanup = attempt.Phase == "cleanup" || attempt.Phase == "discarding"
	attempt.Phase, attempt.SandboxID = "aborted", sandbox.SandboxID
	target := filepath.Join(filepath.Dir(journal), "retained", attempt.ID+".json")
	if err := saveAttempt(target, attempt); err != nil {
		return err
	}
	if err := os.Remove(journal); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(journal))
}

func syncDirectory(name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func stagingID(attemptID, packageID string) string {
	return fmt.Sprintf("retire-%s-%x", attemptID, sha256.Sum256([]byte(packageID)))[:63]
}

func stagingRequest(ctx context.Context, d *workspace, action string, attempt *activationAttempt, item *activationPackage) (map[string]any, error) {
	return d.exchangeControl(ctx, map[string]any{"action": action, "path": strings.TrimPrefix(item.Worktree, "/workspace/packages/"), "id": stagingID(attempt.ID, item.PackageID), "version": item.StagingVersion})
}

// Only generated, fully prepared work can be discarded after confirmed rollback.
func disposableAttempt(attempt activationAttempt) bool {
	for _, item := range attempt.Packages {
		if item.Resolution || item.Conflicted || item.Resolved == "" || item.GitDir == "" || item.StagingVersion == "" {
			return false
		}
	}
	return len(attempt.Packages) > 0
}

func (m *Manager) rollbackAttempt(ctx context.Context, sandbox Sandbox, d *workspace, journal string, attempt *activationAttempt) error {
	attempt.TransactionID, attempt.Phase = "", "captured"
	if disposableAttempt(*attempt) {
		attempt.Phase = "discarding"
	}
	if err := saveAttempt(journal, attempt); err != nil {
		return err
	}
	if attempt.Phase == "discarding" {
		return m.retireStaging(ctx, sandbox, d, journal, attempt)
	}
	// Reuse resolutions on retry instead of archiving another copy each time.
	return removeValidation(journal, attempt)
}

func removeValidation(journal string, attempt *activationAttempt) error {
	roots := map[string]bool{}
	if attempt.Validation != "" {
		roots[attempt.Validation] = true
	}
	// Existing journals name their validation root through the saved candidates.
	for _, candidate := range attempt.Candidates {
		if strings.HasSuffix(candidate.Root, "/"+candidate.PackageID) {
			root := strings.TrimSuffix(candidate.Root, "/"+candidate.PackageID)
			if filepath.Dir(root) == filepath.Dir(journal) && strings.HasPrefix(filepath.Base(root), "validation-") {
				roots[root] = true
			}
		}
	}
	for root := range roots {
		if filepath.Dir(root) != filepath.Dir(journal) || !strings.HasPrefix(filepath.Base(root), "validation-") {
			return errors.New("invalid owned validation staging")
		}
		if err := os.RemoveAll(root); err != nil {
			return fmt.Errorf("remove validation staging (retry activation cleanup): %w", err)
		}
	}
	return nil
}

func (m *Manager) retireStaging(ctx context.Context, sandbox Sandbox, d *workspace, journal string, attempt *activationAttempt) error {
	if attempt.SandboxID != sandbox.SandboxID || (attempt.Phase != "cleanup" && attempt.Phase != "discarding") {
		return errors.New("staging ownership is unknown; abort preserving it and recover/export through ordinary Git")
	}
	for index := range attempt.Packages {
		item := &attempt.Packages[index]
		if item.Retired {
			continue
		}
		if item.Worktree != "/workspace/packages/.conflicts/"+attempt.ID+"/"+item.PackageID || item.Resolved == "" || item.GitDir == "" || item.StagingVersion == "" {
			return errors.New("staging has no verified completion identity; abort preserving it for manual recovery")
		}
		if err := removeStagingGit(ctx, d, sandbox, attempt, item); err != nil {
			return fmt.Errorf("cleanup %s: %w; use activate --continue to retry, or activate --abort to preserve remaining work", item.PackageID, err)
		}
		item.Retired = true
		if err := saveAttempt(journal, attempt); err != nil {
			return err
		}
	}
	if err := removeValidation(journal, attempt); err != nil {
		return err
	}
	if attempt.Phase == "discarding" {
		for _, item := range attempt.Packages {
			for _, captures := range [][]capturedPath{item.Captures, item.Directories} {
				for _, capture := range captures {
					if _, err := d.exchange(ctx, "discard", filepath.Join(item.PackageID, capture.Path), capture.ID); err != nil {
						return fmt.Errorf("discard rolled-back capture: %w", err)
					}
				}
			}
		}
		for _, capture := range attempt.Namespaces {
			if _, err := d.exchange(ctx, "discard", capture.Path, capture.ID); err != nil {
				return err
			}
		}
		staged := filepath.Join(filepath.Dir(journal), "staged", attempt.ID)
		if err := os.RemoveAll(staged); err != nil {
			return err
		}
	}
	if err := os.Remove(journal); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(journal)); err != nil {
		return err
	}
	if attempt.Phase == "discarding" {
		attempt.Phase = "aborted"
	} else {
		attempt.Phase = "completed"
	}
	return nil
}

func removeStagingGit(ctx context.Context, d *workspace, sandbox Sandbox, attempt *activationAttempt, item *activationPackage) error {
	// Git's own lockfiles exclude ordinary index/HEAD changes while the Gofer
	// checks open references and atomically retires the exact filesystem version.
	common := "/workspace/git/private/" + item.PackageID + "/.git"
	if !strings.HasPrefix(item.GitDir, common+"/worktrees/") || strings.Contains(strings.TrimPrefix(item.GitDir, common+"/worktrees/"), "/") {
		return errors.New("unexpected worktree Git registration")
	}
	script := `set -eu
gd=$1; wt=$2; common=$3; expected=$4
if test -e "$gd"; then
  test "$(cat "$gd/gitdir")" = "$wt/.git"
  (set -C; : >"$gd/HEAD.lock")
  trap 'rm -f "$gd/HEAD.lock"' EXIT
  (set -C; : >"$gd/index.lock")
  trap 'rm -f "$gd/HEAD.lock" "$gd/index.lock"' EXIT
  test "$(git --git-dir="$gd" rev-parse HEAD)" = "$expected" || { echo 'Staging HEAD changed after publication; preserve/export the newer commit.' >&2; exit 1; }
  if test -e "$wt"; then
    test "$(git -C "$wt" rev-parse --absolute-git-dir)" = "$gd"
    test -z "$(GIT_OPTIONAL_LOCKS=0 git -C "$wt" status --porcelain --untracked-files=all --ignored=matching)" || { echo 'Staging has edited, untracked or ignored files; preserve/export them.' >&2; exit 1; }
  fi
else
  test ! -e "$wt"
fi
printf 'ready\n'
read -r decision
test "$decision" = retire
if test -e "$gd"; then
  git --git-dir="$common" worktree remove "$wt"
fi
`
	input, send, err := os.Pipe()
	if err != nil {
		return err
	}
	defer input.Close()
	defer send.Close()
	output, receive, err := os.Pipe()
	if err != nil {
		return err
	}
	defer output.Close()
	defer receive.Close()
	finished := make(chan error, 1)
	go func() {
		err := d.ExecCommand(ctx, sandbox.SandboxID, []string{"/bin/bash", "-c", script, "retire-staging", item.GitDir, item.Worktree, common, item.Resolved}, input, receive)
		input.Close()
		receive.Close()
		finished <- err
	}()
	reader := bufio.NewReader(io.LimitReader(output, commandOutputLimit+1))
	line, err := reader.ReadString('\n')
	if err == nil && line != "ready\n" {
		err = fmt.Errorf("Git retirement refused: %s", strings.TrimSpace(line))
	}
	if err == nil {
		_, err = stagingRequest(ctx, d, "retire-staging", attempt, item)
	}
	if err == nil {
		_, err = io.WriteString(send, "retire\n")
	}
	send.Close()
	rest, readErr := io.ReadAll(reader)
	output.Close()
	processErr := <-finished
	if err := errors.Join(err, readErr, processErr); err != nil {
		return fmt.Errorf("%w %s", err, strings.TrimSpace(string(rest)))
	}
	_, err = stagingRequest(ctx, d, "release-staging", attempt, item)
	return err
}
