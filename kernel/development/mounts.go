package development

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const mountProfileSchema = 1

// DefaultMountProfile returns the Phase 1E package, home, and temporary mount
// profile. Callers may instead provide an operator-authored TOML profile.
func DefaultMountProfile() []MountDefinition {
	return []MountDefinition{
		{ID: "packages", Target: "/workspace/packages", Behavior: MountWorkspaceSource, Writable: true, Persistence: "workspace", ParticipatesActivate: true, WorkspaceOwned: true},
		{ID: "home", Source: "users/<user-id>/home", Target: "/home/developer", Behavior: MountPersistent, Writable: true, Persistence: "persistent-user"},
		{ID: "temporary", Target: "/tmp", Behavior: MountEphemeral, Writable: true, Persistence: "sandbox"},
	}
}

func cloneMountProfile(profile []MountDefinition) []MountDefinition {
	return append([]MountDefinition(nil), profile...)
}

func loadMountProfile(config Config) ([]MountDefinition, error) {
	profile := cloneMountProfile(config.MountProfile)
	if len(profile) == 0 && config.MountProfileFile != "" {
		if !filepath.IsAbs(config.MountProfileFile) {
			return nil, errors.New("development mount profile path must be absolute")
		}
		profilePath, err := filepath.EvalSymlinks(config.MountProfileFile)
		if errors.Is(err, os.ErrNotExist) {
			profile = DefaultMountProfile()
		} else if err != nil {
			return nil, fmt.Errorf("resolve development mount profile: %w", err)
		}
		if len(profile) > 0 {
			if err := validateMountProfile(config, profile); err != nil {
				return nil, err
			}
			return cloneMountProfile(profile), nil
		}
		profilePath = filepath.Clean(profilePath)
		if !beneath(profilePath, config.ConfigRoot) {
			return nil, errors.New("development mount profile must remain inside shared config")
		}
		var document MountProfileDocument
		if err := readTOML(profilePath, &document); err != nil {
			return nil, fmt.Errorf("read development mount profile: %w", err)
		}
		if document.Schema != mountProfileSchema {
			return nil, fmt.Errorf("development mount profile schema must be %d", mountProfileSchema)
		}
		profile = document.Mounts
	}
	if len(profile) == 0 {
		profile = DefaultMountProfile()
	}
	if err := validateMountProfile(config, profile); err != nil {
		return nil, err
	}
	return cloneMountProfile(profile), nil
}

func validateMountProfile(config Config, profile []MountDefinition) error {
	ids, targets := map[string]bool{}, map[string]bool{}
	sourceCount, homeCount := 0, 0
	for _, mount := range profile {
		if !safeMountID(mount.ID) || ids[mount.ID] {
			return fmt.Errorf("development mount ID %q is invalid or duplicated", mount.ID)
		}
		ids[mount.ID] = true
		if !safeSandboxMountTarget(mount.Target) || targets[mount.Target] {
			return fmt.Errorf("development mount target %q is invalid or duplicated", mount.Target)
		}
		for target := range targets {
			if beneath(mount.Target, target) || beneath(target, mount.Target) {
				return fmt.Errorf("development mount target %q overlaps %q", mount.Target, target)
			}
		}
		targets[mount.Target] = true
		if mount.Persistence == "" {
			return fmt.Errorf("development mount %s requires persistence behavior", mount.ID)
		}
		switch mount.Behavior {
		case MountWorkspaceSource:
			sourceCount++
			if mount.ID != "packages" || mount.Source != "" || mount.Target != "/workspace/packages" || !mount.Writable || !mount.ParticipatesActivate || !mount.WorkspaceOwned || mount.Persistence != "workspace" {
				return errors.New("packages must be the writable workspace-owned activation source at /workspace/packages")
			}
		case MountReadOnly:
			if mount.Writable || mount.ParticipatesActivate || mount.WorkspaceOwned || mount.Persistence != "shared" {
				return fmt.Errorf("read-only shared mount %s has incompatible behavior flags", mount.ID)
			}
			if _, err := applicationMountSource(config, mount.Source); err != nil {
				return fmt.Errorf("development mount %s: %w", mount.ID, err)
			}
		case MountPersistent:
			if !mount.Writable || mount.ParticipatesActivate || mount.WorkspaceOwned || mount.Persistence != "persistent-user" || !validPersistentSource(mount.Source) {
				return fmt.Errorf("persistent user mount %s has incompatible source or behavior flags", mount.ID)
			}
			if mount.Target == "/home/developer" {
				homeCount++
			}
		case MountEphemeral:
			if mount.Source != "" || !mount.Writable || mount.ParticipatesActivate || mount.WorkspaceOwned || mount.Persistence != "sandbox" {
				return fmt.Errorf("ephemeral mount %s has incompatible source or behavior flags", mount.ID)
			}
		default:
			return fmt.Errorf("development mount %s has unknown behavior %q", mount.ID, mount.Behavior)
		}
	}
	if sourceCount != 1 {
		return errors.New("development mount profile requires exactly one workspace source")
	}
	if homeCount != 1 {
		return errors.New("development mount profile requires exactly one persistent /home/developer mount")
	}
	return nil
}

func safeMountID(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func safeSandboxMountTarget(value string) bool {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || value == "/" || strings.ContainsRune(value, 0) {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && !strings.ContainsRune("/._-", character) {
			return false
		}
	}
	return value == "/tmp" || strings.HasPrefix(value, "/tmp/") || value == "/workspace" || strings.HasPrefix(value, "/workspace/") || value == "/home/developer" || strings.HasPrefix(value, "/home/developer/")
}

func validPersistentSource(value string) bool {
	if !strings.HasPrefix(filepath.ToSlash(value), "users/<user-id>/") || strings.Count(value, "<user-id>") != 1 {
		return false
	}
	probe := strings.Replace(value, "<user-id>", "profile-user", 1)
	return validRelative(probe) && filepath.Clean(filepath.FromSlash(probe)) == filepath.FromSlash(probe)
}

func applicationMountSource(config Config, relative string) (string, error) {
	if !validRelative(relative) || filepath.Clean(filepath.FromSlash(relative)) != filepath.FromSlash(relative) {
		return "", errors.New("application source must be a clean relative path")
	}
	source, err := canonicalDirectory(filepath.Join(config.Root, filepath.FromSlash(relative)))
	if err != nil {
		return "", err
	}
	if source == config.Root || !beneath(source, config.Root) {
		return "", errors.New("application source escaped the configured workspace root")
	}
	for _, restricted := range []string{"node", "config", "state", "users"} {
		if beneath(source, filepath.Join(config.Root, restricted)) {
			return "", fmt.Errorf("application source may not expose %s", restricted)
		}
	}
	return source, nil
}

func (m *Manager) userRoot(userID string) string {
	return filepath.Join(m.config.UsersRoot, userID)
}

func (m *Manager) persistentMountSource(userID, template string, create bool) (string, error) {
	if !safeUserID(userID) || !validPersistentSource(template) {
		return "", errors.New("invalid persistent development mount source")
	}
	relative := strings.Replace(template, "<user-id>", userID, 1)
	relative = strings.TrimPrefix(filepath.ToSlash(relative), "users/")
	path := filepath.Join(m.config.UsersRoot, filepath.FromSlash(relative))
	if create {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return "", err
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return "", err
		}
	}
	canonical, err := canonicalDirectory(path)
	if err != nil {
		return "", err
	}
	if !beneath(canonical, m.userRoot(userID)) {
		return "", errors.New("persistent development mount escaped its configured user root")
	}
	return canonical, nil
}

func (m *Manager) preparePersistentMounts(workspace *Workspace) error {
	workspace.PersistentHomePath = ""
	for _, mount := range workspace.MountProfile {
		if mount.Behavior != MountPersistent {
			continue
		}
		source, err := m.persistentMountSource(workspace.OwnerUserID, mount.Source, true)
		if err != nil {
			return fmt.Errorf("prepare persistent mount %s: %w", mount.ID, err)
		}
		if mount.Target == "/home/developer" {
			workspace.PersistentHomePath = source
		}
	}
	if workspace.PersistentHomePath == "" {
		return errors.New("development workspace has no persistent home mount")
	}
	return nil
}

func (m *Manager) resolveMounts(workspace Workspace) ([]SandboxMount, error) {
	result := make([]SandboxMount, 0, len(workspace.MountProfile))
	for _, mount := range workspace.MountProfile {
		resolved := SandboxMount{MountDefinition: mount}
		var err error
		switch mount.Behavior {
		case MountWorkspaceSource:
			resolved.HostSource = workspace.SourcePath
			if resolved.HostSource == "" {
				err = errors.New("workspace source is not initialized")
			}
		case MountReadOnly:
			resolved.HostSource, err = applicationMountSource(m.config, mount.Source)
		case MountPersistent:
			resolved.HostSource, err = m.persistentMountSource(workspace.OwnerUserID, mount.Source, true)
		case MountEphemeral:
		default:
			err = fmt.Errorf("unknown mount behavior %q", mount.Behavior)
		}
		if err != nil {
			return nil, fmt.Errorf("resolve development mount %s: %w", mount.ID, err)
		}
		result = append(result, resolved)
	}
	return result, nil
}
