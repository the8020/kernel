// Package packages owns package identity, active source, native declarations,
// and package activation. Application configuration belongs to Deno packages.
package packages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/pelletier/go-toml/v2"

	"the8020/kernel/database"
	"the8020/kernel/deployment"
)

const (
	packageManifestSchema       = 1
	manifestLimit               = 1 << 20
	packageInspectionEntryLimit = 5000
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ErrPackageNotReady gates consumers while activation has switched source but
// has not completed its post hook and atomic database publication.
var ErrPackageNotReady = errors.New("package is not active")

// Identity is the filesystem-derived identity of one package or service.
type Identity struct {
	Namespace  string `json:"namespace"`
	Repository string `json:"repository"`
	Service    string `json:"service,omitempty"`
}

func (i Identity) PackageID() string { return i.Namespace + "/" + i.Repository }
func (i Identity) ServiceID() string { return i.PackageID() + "/" + i.Service }
func (i Identity) CanonicalBasePath() string {
	return "/" + i.Namespace + "/" + i.Repository + "/" + i.Service
}

// PackageManifest contains portable package metadata. Identity remains absent
// by design because it comes only from the filesystem.
type PackageManifest struct {
	Schema           int    `toml:"schema" json:"schema"`
	Description      string `toml:"description" json:"description"`
	DocumentationURL string `toml:"documentation_url,omitempty" json:"documentation_url,omitempty"`
	License          string `toml:"license,omitempty" json:"license,omitempty"`
}

type Package struct {
	ID                string           `json:"package_id"`
	Path              string           `json:"path"`
	Description       string           `json:"description,omitempty"`
	DocumentationURL  string           `json:"documentation_url,omitempty"`
	License           string           `json:"license,omitempty"`
	Valid             bool             `json:"valid"`
	Programs          []PackageProgram `json:"programs,omitempty"`
	Files             []PackageFile    `json:"files,omitempty"`
	ContentsTruncated bool             `json:"contents_truncated,omitempty"`
	ValidationErrors  []string         `json:"validation_errors,omitempty"`
	InspectionErrors  []string         `json:"inspection_errors,omitempty"`
}

// PackageProgram is one fixed-depth program manifest under a package.
type PackageProgram struct {
	ID               string   `json:"program_id"`
	Path             string   `json:"path"`
	Description      string   `json:"description,omitempty"`
	Entrypoint       string   `json:"entrypoint,omitempty"`
	DefaultLayout    string   `json:"default_layout,omitempty"`
	Discoverable     bool     `json:"discoverable"`
	UUI              bool     `json:"uui"`
	Valid            bool     `json:"valid"`
	ValidationErrors []string `json:"validation_errors,omitempty"`
}

// PackageFile is one non-directory package entry. Paths are package-relative,
// and Git's internal metadata is deliberately excluded.
type PackageFile struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type programManifest struct {
	Schema        int    `toml:"schema"`
	Description   string `toml:"description"`
	Entrypoint    string `toml:"entrypoint,omitempty"`
	DefaultLayout string `toml:"default_layout,omitempty"`
	Discoverable  *bool  `toml:"discoverable,omitempty"`
	UUI           bool   `toml:"uui,omitempty"`
}

type Config struct {
	WorkspaceRoot string
	PackagesRoot  string
	GitPath       string
	RepositoryMu  *sync.RWMutex
	Secrets       SecretResolver
	Database      database.Store
	IndexStore    PackageIndexStore
	Logger        *slog.Logger
}

type Store struct {
	catalog       *Catalog
	workspaceRoot string
	packagesRoot  string
	index         PackageIndexStore
	gitPath       string
	repositoryMu  *sync.RWMutex
	packageLocks  sync.Map
	secrets       SecretResolver
	logger        *slog.Logger
	deploymentMu  sync.RWMutex
	deployment    deployment.SchemaHook
	databaseIndex bool
	handlers      handlerIndex
}

func New(config Config) (*Store, error) {
	workspace, err := canonicalDirectory(config.WorkspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	packagesRoot, err := resolveDirectory(workspace, config.PackagesRoot, "packages")
	if err != nil {
		return nil, fmt.Errorf("packages root: %w", err)
	}
	indexStore := config.IndexStore
	databaseIndex := indexStore == nil
	if indexStore == nil {
		indexStore, err = NewDatabasePackageIndexStore(config.Database)
		if err != nil {
			return nil, fmt.Errorf("package index database: %w", err)
		}
	}
	repositoryMu := config.RepositoryMu
	if repositoryMu == nil {
		repositoryMu = &sync.RWMutex{}
	}
	gitPath := strings.TrimSpace(config.GitPath)
	if gitPath == "" {
		gitPath = "git"
	}
	catalog, err := NewCatalog(packagesRoot, config.Logger)
	if err != nil {
		return nil, err
	}
	return &Store{
		catalog:       catalog,
		workspaceRoot: workspace, packagesRoot: packagesRoot,
		index:   indexStore,
		gitPath: gitPath, repositoryMu: repositoryMu, secrets: config.Secrets,
		logger: config.Logger, databaseIndex: databaseIndex,
	}, nil
}

// SecretResolver exposes only the value lookup required by authenticated Git.
type SecretResolver interface {
	SecretValue(string) (string, error)
}

func (s *Store) PackagesRoot() string { return s.packagesRoot }

func (s *Store) SetSchemaDeployment(hook deployment.SchemaHook) {
	s.deploymentMu.Lock()
	s.deployment = hook
	s.deploymentMu.Unlock()
}

func (s *Store) schemaDeployment() deployment.SchemaHook {
	s.deploymentMu.RLock()
	defer s.deploymentMu.RUnlock()
	return s.deployment
}

func (s *Store) ListPackages() ([]Package, error) {
	if !s.databaseIndex {
		return s.catalog.ListPackages()
	}
	entries, err := s.index.List(context.Background())
	if err != nil {
		return nil, err
	}
	result := make([]Package, 0, len(entries))
	for _, entry := range entries {
		item := s.catalog.inspectPackage(Identity{Namespace: entry.Author, Repository: entry.Repository})
		if entry.State != "ready" {
			item.Valid = false
			item.ValidationErrors = append(item.ValidationErrors, "package database state is "+entry.State)
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *Store) InspectPackage(packageID string) (Package, error) {
	identity, err := ParsePackageID(packageID)
	if err != nil {
		return Package{}, err
	}
	result := s.inspectPackage(identity)
	canonical, err := canonicalWithin(result.Path, s.packagesRoot)
	if err != nil || canonical != result.Path {
		return result, nil
	}
	result.Programs, err = s.inspectPackagePrograms(identity, result.Path)
	if err != nil {
		result.InspectionErrors = append(result.InspectionErrors, err.Error())
	}
	result.Files, result.ContentsTruncated, err = inspectPackageFiles(result.Path)
	if err != nil {
		result.InspectionErrors = append(result.InspectionErrors, err.Error())
	}
	return result, nil
}

// ResolvePackage reads only one fixed package manifest without recursive inspection.
func (s *Store) ResolvePackage(packageID string) (Package, error) {
	if err := s.requireReadyPackage(context.Background(), packageID); err != nil {
		return Package{}, err
	}
	return s.catalog.ResolvePackage(packageID)
}

// ActivatedPackageCommit verifies that the installed source is exactly the
// clean commit currently published as ready. Database table evaluation uses
// this proof before allowing activated source to change shared schema.
func (s *Store) ActivatedPackageCommit(ctx context.Context, packageID string) (string, error) {
	entry, exists, err := s.index.Get(ctx, packageID)
	if err != nil {
		return "", err
	}
	if !exists || entry.State != "ready" || entry.ActiveCommit == "" {
		return "", fmt.Errorf("package %s has no ready active commit", packageID)
	}
	path, exists, err := s.packageDestination(packageID)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("package is not installed: %s", packageID)
	}
	commit, err := s.installedCommit(ctx, path)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(commit, entry.ActiveCommit) {
		return "", fmt.Errorf("package %s checkout %q does not match ready active commit %q", packageID, commit, entry.ActiveCommit)
	}
	return commit, nil
}

func (s *Store) requireReadyPackage(ctx context.Context, packageID string) error {
	if !s.databaseIndex {
		return nil
	}
	entry, exists, err := s.index.Get(ctx, packageID)
	if err != nil {
		return err
	}
	if !exists {
		return os.ErrNotExist
	}
	if entry.State != "ready" {
		return fmt.Errorf("%w: %s", ErrPackageNotReady, packageID)
	}
	return nil
}

func (s *Store) inspectPackage(identity Identity) Package {
	return s.catalog.inspectPackage(identity)
}

func (s *Store) inspectPackagePrograms(identity Identity, root string) ([]PackageProgram, error) {
	entries, err := os.ReadDir(filepath.Join(root, "programs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read package programs: %w", err)
	}
	result := make([]PackageProgram, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || (!entry.IsDir() && entry.Type()&os.ModeSymlink == 0) {
			continue
		}
		programRoot := filepath.Join(root, "programs", entry.Name())
		manifestPath := filepath.Join(programRoot, "program.toml")
		if _, statErr := os.Lstat(manifestPath); errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		item := PackageProgram{
			ID:           identity.PackageID() + "/" + entry.Name(),
			Path:         filepath.ToSlash(filepath.Join("programs", entry.Name())),
			Discoverable: true,
		}
		if err := ValidateName(entry.Name()); err != nil {
			item.ValidationErrors = append(item.ValidationErrors, err.Error())
		}
		canonical, canonicalErr := canonicalWithin(programRoot, root)
		if canonicalErr != nil {
			item.ValidationErrors = append(item.ValidationErrors, fmt.Sprintf("%s: %v", programRoot, canonicalErr))
		} else if canonical != programRoot {
			item.ValidationErrors = append(item.ValidationErrors, fmt.Sprintf("program root %s resolves through a symlink", programRoot))
		}
		var manifest programManifest
		if canonicalErr == nil && canonical == programRoot {
			if err := decodeTOMLWithin(manifestPath, programRoot, &manifest); err != nil {
				item.ValidationErrors = append(item.ValidationErrors, fmt.Sprintf("%s: %v", manifestPath, err))
			} else {
				item.Description = manifest.Description
				item.Entrypoint = manifest.Entrypoint
				item.DefaultLayout = manifest.DefaultLayout
				item.UUI = manifest.UUI
				if manifest.Discoverable != nil {
					item.Discoverable = *manifest.Discoverable
				}
				if manifest.Schema != packageManifestSchema {
					item.ValidationErrors = append(item.ValidationErrors, fmt.Sprintf("%s: schema must equal %d", manifestPath, packageManifestSchema))
				}
				if manifest.Description == "" {
					item.ValidationErrors = append(item.ValidationErrors, fmt.Sprintf("%s: description is required", manifestPath))
				}
				if item.Entrypoint == "" {
					item.Entrypoint = "program.ts"
				}
				if err := validateProgramRelativePath(item.Entrypoint); err != nil {
					item.ValidationErrors = append(item.ValidationErrors, fmt.Sprintf("%s: entrypoint %v", manifestPath, err))
				} else if canonicalEntrypoint, err := canonicalWithin(filepath.Join(programRoot, filepath.FromSlash(item.Entrypoint)), programRoot); err != nil {
					item.ValidationErrors = append(item.ValidationErrors, fmt.Sprintf("entrypoint %s: %v", item.Entrypoint, err))
				} else if info, err := os.Stat(canonicalEntrypoint); err != nil || !info.Mode().IsRegular() {
					if err == nil {
						err = errors.New("entrypoint is not a regular file")
					}
					item.ValidationErrors = append(item.ValidationErrors, fmt.Sprintf("entrypoint %s: %v", item.Entrypoint, err))
				}
				if item.DefaultLayout != "" {
					if err := validateProgramRelativePath(item.DefaultLayout); err != nil {
						item.ValidationErrors = append(item.ValidationErrors, fmt.Sprintf("%s: default_layout %v", manifestPath, err))
					}
				}
			}
		}
		item.Valid = len(item.ValidationErrors) == 0
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func validateProgramRelativePath(value string) error {
	if value == "" || filepath.IsAbs(value) || strings.ContainsAny(value, "\\\x00") || filepath.ToSlash(filepath.Clean(value)) != value {
		return errors.New("must be a canonical relative path inside the program")
	}
	for _, segment := range strings.Split(value, "/") {
		if err := ValidateName(segment); err != nil {
			return errors.New("must be a canonical relative path inside the program")
		}
	}
	return nil
}

func inspectPackageFiles(root string) ([]PackageFile, bool, error) {
	result := make([]PackageFile, 0)
	visited := 0
	truncated := false
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if parts[0] == ".git" {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		visited++
		if visited > packageInspectionEntryLimit {
			truncated = true
			return fs.SkipAll
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		kind := "other"
		switch {
		case entry.Type()&os.ModeSymlink != 0:
			kind = "symlink"
		case info.Mode().IsRegular():
			kind = "file"
		}
		result = append(result, PackageFile{Path: filepath.ToSlash(relative), Type: kind, Size: info.Size()})
		return nil
	})
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, truncated, err
}

// ListStateServiceIDs reads the installed service declarations published by
// package activation. It never rescans package source during reconciliation.

// ParseByteSize parses the manifest's deliberately small decimal byte-size
// vocabulary. It is shared with the runtime configuration bridge so manifest
// limits are validated and applied identically.
func ParseByteSize(value string) (int64, error) {
	upper := strings.ToUpper(strings.TrimSpace(value))
	for _, unit := range []struct {
		suffix string
		factor int64
	}{{"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1_000}, {"B", 1}} {
		if strings.HasSuffix(upper, unit.suffix) {
			number := strings.TrimSpace(strings.TrimSuffix(upper, unit.suffix))
			parsed, err := strconv.ParseInt(number, 10, 64)
			if err != nil || parsed <= 0 || parsed > (1<<63-1)/unit.factor {
				return 0, errors.New("must be a positive byte size such as 1KB, 10MB, or 1GB")
			}
			return parsed * unit.factor, nil
		}
	}
	return 0, errors.New("must include B, KB, MB, or GB")
}

// MutateState serializes one desired-policy change and records a new immutable
// effective version before publishing it as desired.

// FingerprintPackage gives local, non-Git packages the same deterministic
// source identity used by schema evaluation.
func FingerprintPackage(root string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(hash, filepath.ToSlash(relative)+"\x00"); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(hash, file)
		return errors.Join(copyErr, file.Close())
	})
	if err != nil {
		return "", err
	}
	return "filesystem-" + hex.EncodeToString(hash.Sum(nil)), nil
}

func ParsePackageID(value string) (Identity, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return Identity{}, errors.New("package ID must be <namespace>/<repository>")
	}
	for _, part := range parts {
		if err := ValidateName(part); err != nil {
			return Identity{}, err
		}
	}
	return Identity{Namespace: parts[0], Repository: parts[1]}, nil
}

func ParseServiceID(value string) (Identity, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 3 {
		return Identity{}, errors.New("service ID must be <namespace>/<repository>/<service>")
	}
	for _, part := range parts {
		if err := ValidateName(part); err != nil {
			return Identity{}, err
		}
	}
	return Identity{Namespace: parts[0], Repository: parts[1], Service: parts[2]}, nil
}

func ValidateName(value string) error {
	if !namePattern.MatchString(value) || value == "." || value == ".." || strings.ContainsAny(value, "/\\\x00") {
		return fmt.Errorf("name %q must match %s", value, namePattern.String())
	}
	return nil
}

func decodeTOMLFile(path string, output any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := toml.NewDecoder(io.LimitReader(file, manifestLimit+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	return nil
}

func decodeTOMLWithin(path, root string, output any) error {
	canonical, err := canonicalWithin(path, root)
	if err != nil {
		return err
	}
	return decodeTOMLFile(canonical, output)
}

func resolveDirectory(workspace, configured, fallback string) (string, error) {
	if configured == "" {
		configured = fallback
	}
	explicitAbsolute := filepath.IsAbs(configured)
	if !explicitAbsolute {
		configured = filepath.Join(workspace, configured)
	}
	configured = filepath.Clean(configured)
	if !explicitAbsolute && !beneath(configured, workspace) {
		return "", errors.New("configured root must remain inside the workspace")
	}
	if err := os.MkdirAll(configured, 0o755); err != nil {
		return "", err
	}
	canonical, err := canonicalDirectory(configured)
	if err != nil {
		return "", err
	}
	if !explicitAbsolute && !beneath(canonical, workspace) {
		return "", errors.New("configured root resolves outside the workspace")
	}
	return canonical, nil
}
func canonicalDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	return filepath.Clean(canonical), nil
}
func canonicalWithin(path, root string) (string, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", err
	}
	canonical = filepath.Clean(canonical)
	if !beneath(canonical, root) {
		return "", errors.New("path resolves outside its allowed root")
	}
	return canonical, nil
}
func beneath(path, root string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
