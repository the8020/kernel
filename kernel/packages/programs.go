package packages

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const packageSandboxRoot = "/workspace/packages"

// ProgramDefinition is one validated ordinary program from an exact active
// package checkout. HostPath is used only for validation; EntrypointURL is the
// read-only path visible inside runtime sandboxes.
type ProgramDefinition struct {
	ID            string `json:"program_id"`
	PackageID     string `json:"package_id"`
	Name          string `json:"name"`
	Commit        string `json:"commit"`
	PackageRoot   string `json:"-"`
	HostPath      string `json:"-"`
	Entrypoint    string `json:"entrypoint"`
	EntrypointURL string `json:"entrypoint_url"`
	Description   string `json:"description,omitempty"`
	Discoverable  bool   `json:"discoverable"`
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

// ResolveProgram proves that a program belongs to the exact ready package
// commit before returning its sandbox-visible entrypoint.
func (s *Store) ResolveProgram(ctx context.Context, programID string) (ProgramDefinition, error) {
	identity, name, err := ParseProgramID(programID)
	if err != nil {
		return ProgramDefinition{}, err
	}
	commit, err := s.ActivatedPackageCommit(ctx, identity.PackageID())
	if err != nil {
		return ProgramDefinition{}, err
	}
	root, exists, err := s.packageDestination(identity.PackageID())
	if err != nil {
		return ProgramDefinition{}, err
	}
	if !exists {
		return ProgramDefinition{}, fmt.Errorf("package is not installed: %s", identity.PackageID())
	}
	return ValidateProgram(root, identity.PackageID(), name, commit)
}

// SnapshotProgram copies one exact active package tree into an invocation-owned
// directory. The unique source path keeps runtime groups for different
// invocations and package revisions separate. The caller must run cleanup after
// the job has stopped.
func (s *Store) SnapshotProgram(ctx context.Context, programID string) (ProgramDefinition, func() error, error) {
	s.repositoryMu.RLock()
	defer s.repositoryMu.RUnlock()
	program, err := s.ResolveProgram(ctx, programID)
	if err != nil {
		return ProgramDefinition{}, nil, err
	}
	snapshotParent := filepath.Join(s.packagesRoot, ".cbus-programs")
	if err := os.MkdirAll(snapshotParent, 0o700); err != nil {
		return ProgramDefinition{}, nil, fmt.Errorf("create command program snapshot root: %w", err)
	}
	snapshotRoot, err := os.MkdirTemp(snapshotParent, "invocation-")
	if err != nil {
		return ProgramDefinition{}, nil, fmt.Errorf("create command program snapshot: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(snapshotRoot) }
	packageRoot := filepath.Join(snapshotRoot, "package")
	if err := copyTree(program.PackageRoot, packageRoot); err != nil {
		_ = cleanup()
		return ProgramDefinition{}, nil, fmt.Errorf("snapshot command program: %w", err)
	}
	program, err = ValidateProgram(packageRoot, program.PackageID, program.Name, program.Commit)
	if err != nil {
		_ = cleanup()
		return ProgramDefinition{}, nil, fmt.Errorf("validate command program snapshot: %w", err)
	}
	return program, cleanup, nil
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case entry.Type()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case entry.Type().IsRegular():
			input, err := os.Open(path)
			if err != nil {
				return err
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
			if err != nil {
				_ = input.Close()
				return err
			}
			_, copyErr := io.Copy(output, input)
			return errors.Join(copyErr, output.Close(), input.Close())
		default:
			return fmt.Errorf("unsupported package entry %s", relative)
		}
	})
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
		Commit: commit, PackageRoot: absRoot, HostPath: entrypointPath, Entrypoint: entrypoint,
		EntrypointURL: (&url.URL{Scheme: "file", Path: sandboxPath}).String(),
		Description:   manifest.Description, Discoverable: discoverable,
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
