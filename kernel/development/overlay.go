package development

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const overlayStateSchema = 1

type overlayStateDocument struct {
	Schema   int                               `toml:"schema"`
	Packages map[string]overlayPackageDocument `toml:"packages"`
}

type overlayPackageDocument struct {
	BaseCommit string `toml:"base_commit"`
	Patch      string `toml:"patch"`
}

func (m *Manager) overlayRoot(sandbox Sandbox) string {
	return filepath.Join(m.sandboxRoot(sandbox), "runtime", "overlay")
}

func (m *Manager) persistCapturedOverlay(sandbox Sandbox, changes []packageChanges) error {
	runtimeRoot := filepath.Join(m.sandboxRoot(sandbox), "runtime")
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(runtimeRoot, ".overlay-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	document := overlayStateDocument{Schema: overlayStateSchema, Packages: map[string]overlayPackageDocument{}}
	for _, change := range changes {
		if change.PatchPath == "" {
			return fmt.Errorf("development package %s has no captured patch", change.PackageID)
		}
		relative := filepath.ToSlash(filepath.Join("patches", filepath.FromSlash(change.PackageID)+".patch"))
		if err := copyPatch(filepath.Join(stage, filepath.FromSlash(relative)), change.PatchPath); err != nil {
			return err
		}
		document.Packages[change.PackageID] = overlayPackageDocument{BaseCommit: change.Base, Patch: relative}
	}
	if err := writeTOML(filepath.Join(stage, "state.toml"), document, 0o600); err != nil {
		return err
	}
	return replaceOverlayDirectory(stage, m.overlayRoot(sandbox))
}

func replaceOverlayDirectory(stage, destination string) error {
	backup := stage + ".previous"
	if _, err := os.Lstat(destination); err == nil {
		if err := os.Rename(destination, backup); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(stage, destination); err != nil {
		if _, backupErr := os.Lstat(backup); backupErr == nil {
			_ = os.Rename(backup, destination)
		}
		return err
	}
	return os.RemoveAll(backup)
}

func (m *Manager) checkpointOverlayLocked(ctx context.Context, sandbox Sandbox) error {
	if _, active := m.owned.Load(sandbox.SandboxID); !active {
		return nil
	}
	stage, err := os.MkdirTemp(filepath.Join(m.sandboxRoot(sandbox), "runtime"), ".overlay-capture-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	changes, err := m.captureChanges(ctx, sandbox, stage)
	if err != nil {
		return err
	}
	return m.persistCapturedOverlay(sandbox, changes)
}

func (m *Manager) restoreOverlayLocked(ctx context.Context, sandbox *Sandbox) error {
	statePath := filepath.Join(m.overlayRoot(*sandbox), "state.toml")
	var document overlayStateDocument
	if err := readTOML(statePath, &document); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if document.Schema != overlayStateSchema {
		return errors.New("invalid development overlay checkpoint")
	}
	if document.Packages == nil {
		document.Packages = map[string]overlayPackageDocument{}
	}
	ids := make([]string, 0, len(document.Packages))
	for id := range document.Packages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		item := document.Packages[id]
		if _, err := m.packageRoot(id); err != nil || !validGitObjectID(item.BaseCommit) || !validRelative(item.Patch) {
			return fmt.Errorf("invalid development overlay checkpoint package %s", id)
		}
		patchPath := filepath.Join(m.overlayRoot(*sandbox), filepath.FromSlash(item.Patch))
		canonicalPatch, err := filepath.EvalSymlinks(patchPath)
		if err != nil || !beneath(canonicalPatch, m.overlayRoot(*sandbox)) {
			return fmt.Errorf("development overlay checkpoint patch escaped package %s", id)
		}
		patch, err := os.Open(canonicalPatch)
		if err != nil {
			return err
		}
		current, _ := m.repositoryHead(id)
		command := "git -C " + shellQuote("/workspace/packages/"+id) + " apply --binary --whitespace=nowarn -"
		if current != item.BaseCommit {
			command = "set +e\n" +
				"git -C " + shellQuote("/workspace/packages/"+id) + " apply --3way --binary --whitespace=nowarn -\n" +
				"status=$?\n" +
				"git -C " + shellQuote("/workspace/packages/"+id) + " reset --mixed --quiet HEAD\n" +
				"exit \"$status\""
		}
		applyErr := m.driver.ExecStream(ctx, sandbox.SandboxID, command, patch, io.Discard)
		closeErr := patch.Close()
		if applyErr != nil || closeErr != nil {
			return fmt.Errorf("restore development overlay package %s: %w", id, errors.Join(applyErr, closeErr))
		}
	}
	return nil
}

func validGitObjectID(value string) bool {
	if len(value) < 40 || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func (m *Manager) resetOverlayLocked(ctx context.Context, sandbox *Sandbox) error {
	if _, active := m.owned.Load(sandbox.SandboxID); active {
		_ = m.driver.Kill(ctx, sandbox.SandboxID)
		if err := m.driver.Delete(ctx, sandbox.SandboxID); err != nil {
			return err
		}
		m.owned.Delete(sandbox.SandboxID)
		_ = removeDevelopmentFilestore(m.config.PackagesRoot, sandbox.SandboxID)
	}
	return m.startLocked(ctx, sandbox)
}

func removeDevelopmentFilestore(packagesRoot, sandboxID string) error {
	if !validDevelopmentSandboxID(sandboxID) {
		return errors.New("safe development sandbox ID is required")
	}
	return os.Remove(filepath.Join(packagesRoot, ".gvisor.filestore."+sandboxID))
}

func (m *Manager) resetOverlay(ctx context.Context, userID string) error {
	unlock := m.lockUser(userID)
	defer unlock()
	sandbox, err := m.loadSandbox(userID)
	if err != nil {
		return err
	}
	if err := m.resetOverlayLocked(ctx, &sandbox); err != nil {
		return err
	}
	if sandbox.LastActivationResult != nil {
		sandbox.LastActivationResult.OverlayReset = true
		sandbox.LastActivationResult.OverlayResetPending = false
	}
	return m.saveSandbox(sandbox)
}

func (m *Manager) resetOverlayAfterHelper(userID string) {
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.resetOverlay(ctx, userID); err != nil && m.config.Logger != nil {
		m.config.Logger.Error("development overlay reset after helper activation failed", "user_id", userID, "error", err)
	}
}
