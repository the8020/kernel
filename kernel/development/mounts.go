package development

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultMountProfile returns the package, helper-script, and temporary mount
// profile. Tests and embedders may provide a profile directly.
func DefaultMountProfile() []MountDefinition {
	return []MountDefinition{
		{ID: "packages", Target: "/workspace/packages", Behavior: MountSandboxSource, Writable: true},
		{ID: "scripts", Source: "scripts", Target: "/workspace/scripts", Behavior: MountReadOnly, Executable: true},
		{ID: "temporary", Target: "/tmp", Behavior: MountEphemeral, Writable: true},
	}
}

func cloneMountProfile(profile []MountDefinition) []MountDefinition {
	return append([]MountDefinition(nil), profile...)
}

func loadMountProfile(config Config) ([]MountDefinition, error) {
	profile := cloneMountProfile(config.MountProfile)
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
	sourceCount := 0
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
		switch mount.Behavior {
		case MountSandboxSource:
			sourceCount++
			if mount.ID != "packages" || mount.Source != "" || mount.Target != "/workspace/packages" || !mount.Writable || mount.Executable {
				return errors.New("packages must be the writable sandbox activation source at /workspace/packages")
			}
		case MountReadOnly:
			if mount.Writable {
				return fmt.Errorf("read-only shared mount %s has incompatible behavior flags", mount.ID)
			}
			if _, err := applicationMountSource(config, mount.Source); err != nil {
				return fmt.Errorf("development mount %s: %w", mount.ID, err)
			}
		case MountPersistent:
			if !mount.Writable || mount.Executable || !validPersistentSource(mount.Source) {
				return fmt.Errorf("persistent user mount %s has incompatible source or behavior flags", mount.ID)
			}
		case MountEphemeral:
			if mount.Source != "" || !mount.Writable || mount.Executable {
				return fmt.Errorf("ephemeral mount %s has incompatible source or behavior flags", mount.ID)
			}
		default:
			return fmt.Errorf("development mount %s has unknown behavior %q", mount.ID, mount.Behavior)
		}
	}
	if sourceCount != 1 {
		return errors.New("development mount profile requires exactly one sandbox source")
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
	return value == "/tmp" || strings.HasPrefix(value, "/tmp/") || value == "/workspace" || strings.HasPrefix(value, "/workspace/")
}

func validPersistentSource(value string) bool {
	if !strings.HasPrefix(filepath.ToSlash(value), "users/<user-id>/dev-sandbox/") || strings.Count(value, "<user-id>") != 1 {
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
		return "", errors.New("application source escaped the configured root")
	}
	for _, restricted := range []string{"node", "database", "users"} {
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
	if !beneath(canonical, m.sandboxRootForUser(userID)) {
		return "", errors.New("persistent development mount escaped the user's dev-sandbox root")
	}
	return canonical, nil
}

func (m *Manager) preparePersistentMounts(userID string) error {
	for _, mount := range m.config.MountProfile {
		if mount.Behavior != MountPersistent {
			continue
		}
		_, err := m.persistentMountSource(userID, mount.Source, true)
		if err != nil {
			return fmt.Errorf("prepare persistent mount %s: %w", mount.ID, err)
		}
	}
	return nil
}

func (m *Manager) resolveMounts(sandbox Sandbox) ([]SandboxMount, error) {
	result := make([]SandboxMount, 0, len(m.config.MountProfile))
	for _, mount := range m.config.MountProfile {
		resolved := SandboxMount{MountDefinition: mount}
		var err error
		switch mount.Behavior {
		case MountSandboxSource:
			resolved.HostSource = sandbox.SourcePath
			if resolved.HostSource == "" {
				err = errors.New("sandbox source is not initialized")
			}
		case MountReadOnly:
			resolved.HostSource, err = applicationMountSource(m.config, mount.Source)
		case MountPersistent:
			resolved.HostSource, err = m.persistentMountSource(sandbox.UserID, mount.Source, true)
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
