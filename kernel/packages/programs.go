package packages

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"the8020/kernel/deployment"
)

const packageSandboxRoot = "/workspace/packages"

// ListPrograms reads only ready package program manifests for an explicit
// selector. It does not inspect services, Git status, or package file trees.
func (s *Store) ListPrograms(ctx context.Context) ([]ProgramDefinition, error) {
	entries, err := s.index.List(ctx)
	if err != nil {
		return nil, err
	}
	result := []ProgramDefinition{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.State != "ready" || entry.ActiveCommit == "" {
			continue
		}
		root, exists, err := s.packageDestination(entry.PackageID)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		programs, err := os.ReadDir(filepath.Join(root, "programs"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, item := range programs {
			if !item.IsDir() || strings.HasPrefix(item.Name(), ".") {
				continue
			}
			program, err := ValidateProgram(root, entry.PackageID, item.Name(), entry.ActiveCommit)
			if err != nil {
				continue
			}
			result = append(result, program)
			if len(result) > 2000 {
				return nil, errors.New("program selector exceeds 2000 entries")
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

// ProgramDefinition is one validated program from a ready package.
// EntrypointURL uses the shared read-only packages mount in ordinary sandboxes.
type ProgramDefinition struct {
	ID            string `json:"program_id"`
	PackageID     string `json:"package_id"`
	Name          string `json:"name"`
	Commit        string `json:"commit"`
	Entrypoint    string `json:"entrypoint"`
	EntrypointURL string `json:"entrypoint_url"`
	Description   string `json:"description,omitempty"`
	Discoverable  bool   `json:"discoverable"`
	UUI           bool   `json:"uui"`
}

// ParseProgramID accepts only <namespace>/<repository>/<program> identities.
func ParseProgramID(value string) (Identity, string, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 3 {
		return Identity{}, "", errors.New("program ID must be <namespace>/<repository>/<program>")
	}
	identity, err := ParsePackageID(strings.Join(parts[:2], "/"))
	if err != nil {
		return Identity{}, "", err
	}
	if err := ValidateName(parts[2]); err != nil {
		return Identity{}, "", err
	}
	return identity, parts[2], nil
}

// ResolveProgram reads the active package record and validates the selected
// program. Activation owns checkout publication; invocation does not inspect
// Git, scan the package tree, or prepare a private copy of package sources.
func (s *Store) ResolveProgram(ctx context.Context, programID string) (ProgramDefinition, error) {
	return s.resolveProgram(ctx, programID, nil)
}

func (s *Store) resolveProgram(ctx context.Context, programID string, candidates map[string]deployment.Candidate) (ProgramDefinition, error) {
	identity, name, err := ParseProgramID(programID)
	if err != nil {
		return ProgramDefinition{}, err
	}
	if candidate, exists := candidates[identity.PackageID()]; exists {
		return ValidateProgram(candidate.Root, identity.PackageID(), name, candidate.Commit)
	}
	entry, found, err := s.index.Get(ctx, identity.PackageID())
	if err != nil {
		return ProgramDefinition{}, err
	}
	if !found || entry.State != "ready" || entry.ActiveCommit == "" {
		return ProgramDefinition{}, fmt.Errorf("package %s has no ready active commit", identity.PackageID())
	}
	root, exists, err := s.packageDestination(identity.PackageID())
	if err != nil {
		return ProgramDefinition{}, err
	}
	if !exists {
		return ProgramDefinition{}, fmt.Errorf("package is not installed: %s", identity.PackageID())
	}
	return ValidateProgram(root, identity.PackageID(), name, entry.ActiveCommit)
}

// ValidateProgram validates one program in a package root. Activation and
// command discovery use it for candidate trees before they become active.
func ValidateProgram(packageRoot, packageID, name, commit string) (ProgramDefinition, error) {
	identity, err := ParsePackageID(packageID)
	if err != nil {
		return ProgramDefinition{}, err
	}
	if err := ValidateName(name); err != nil {
		return ProgramDefinition{}, err
	}
	absRoot, err := filepath.Abs(packageRoot)
	if err != nil {
		return ProgramDefinition{}, err
	}
	absRoot = filepath.Clean(absRoot)
	if err := requireRealPath(absRoot, absRoot, true); err != nil {
		return ProgramDefinition{}, fmt.Errorf("package root: %w", err)
	}
	programRoot := filepath.Join(absRoot, "programs", name)
	if err := requireRealPath(absRoot, programRoot, true); err != nil {
		return ProgramDefinition{}, fmt.Errorf("program %s: %w", name, err)
	}
	manifestPath := filepath.Join(programRoot, "program.toml")
	if err := requireRealPath(absRoot, manifestPath, false); err != nil {
		return ProgramDefinition{}, fmt.Errorf("program manifest: %w", err)
	}
	var manifest programManifest
	if err := decodeTOMLFile(manifestPath, &manifest); err != nil {
		return ProgramDefinition{}, fmt.Errorf("program manifest: %w", err)
	}
	if manifest.Schema != packageManifestSchema {
		return ProgramDefinition{}, fmt.Errorf("program manifest schema must equal %d", packageManifestSchema)
	}
	if strings.TrimSpace(manifest.Description) == "" {
		return ProgramDefinition{}, errors.New("program manifest description is required")
	}
	entrypoint := manifest.Entrypoint
	if entrypoint == "" {
		entrypoint = "program.ts"
	}
	if err := validateProgramRelativePath(entrypoint); err != nil {
		return ProgramDefinition{}, fmt.Errorf("program entrypoint %v", err)
	}
	entrypointPath := filepath.Join(programRoot, filepath.FromSlash(entrypoint))
	if err := requireRealPath(programRoot, entrypointPath, false); err != nil {
		return ProgramDefinition{}, fmt.Errorf("program entrypoint: %w", err)
	}
	discoverable := true
	if manifest.Discoverable != nil {
		discoverable = *manifest.Discoverable
	}
	sandboxPath := filepath.ToSlash(filepath.Join(packageSandboxRoot, identity.Namespace, identity.Repository, "programs", name, filepath.FromSlash(entrypoint)))
	return ProgramDefinition{
		ID: packageID + "/" + name, PackageID: packageID, Name: name,
		Commit: commit, Entrypoint: entrypoint,
		EntrypointURL: (&url.URL{Scheme: "file", Path: sandboxPath}).String(),
		Description:   manifest.Description, Discoverable: discoverable, UUI: manifest.UUI,
	}, nil
}

// requireRealPath rejects every symlink component and verifies the target
// type. This is stricter than merely proving where a symlink resolves.
func requireRealPath(root, target string, directory bool) error {
	root, target = filepath.Clean(root), filepath.Clean(target)
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", root)
	}
	if !beneath(target, root) {
		return errors.New("path is outside its owning package")
	}
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	current := root
	parts := []string{}
	if relative != "." {
		parts = strings.Split(relative, string(filepath.Separator))
	}
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", current)
		}
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if directory && !info.IsDir() {
		return errors.New("path is not a directory")
	}
	if !directory && !info.Mode().IsRegular() {
		return errors.New("path is not a regular file")
	}
	return nil
}
