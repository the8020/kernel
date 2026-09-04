// Package packages owns filesystem-derived package and service definitions and
// shared desired state.
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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"

	"the8020/kernel/database"
	"the8020/kernel/deployment"
)

const (
	packageManifestSchema       = 1
	serviceManifestSchema       = 2
	manifestLimit               = 1 << 20
	packageInspectionEntryLimit = 5000
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ErrPackageNotReady gates consumers while activation has switched source but
// has not completed its post hook and atomic database publication.
var ErrPackageNotReady = errors.New("package is not active")

// ErrInvalidServicePolicy classifies invalid manifest/default/override
// combinations without coupling the package domain to a transport error code.
var ErrInvalidServicePolicy = errors.New("invalid service policy")

type invalidServicePolicyError struct{ message string }

func (e *invalidServicePolicyError) Error() string { return e.message }
func (e *invalidServicePolicyError) Unwrap() error { return ErrInvalidServicePolicy }

func invalidServicePolicy(message string) error {
	return &invalidServicePolicyError{message: message}
}

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

type LifecycleManifest struct {
	DefaultEnabled   bool   `toml:"default_enabled" json:"default_enabled"`
	ServiceType      string `toml:"service_type,omitempty" json:"service_type"`
	SessionKeepAlive string `toml:"session_keep_alive,omitempty" json:"session_keep_alive,omitempty"`
}

type OpenAPIManifest struct {
	Title       string `toml:"title,omitempty" json:"title,omitempty"`
	Version     string `toml:"version,omitempty" json:"version,omitempty"`
	Description string `toml:"description,omitempty" json:"description,omitempty"`
}

const (
	ServiceTypeStateless    = "stateless"
	ServiceTypeSession      = "session"
	AccessModePublic        = "public"
	AccessModeAuthenticated = "authenticated"
	UnauthenticatedReject   = "reject"
	UnauthenticatedRedirect = "redirect"
)

// ScalingManifest is the complete service-owned scaling contract. Runtime
// desired counts, cooldowns, and placement decisions are kernel policy rather
// than portable service configuration.
type ScalingManifest struct {
	MinimumWorkers       *int     `toml:"minimum_workers,omitempty" json:"minimum_workers,omitempty"`
	MaximumWorkers       *int     `toml:"maximum_workers,omitempty" json:"maximum_workers,omitempty"`
	ConcurrencyPerWorker *int     `toml:"concurrency_per_worker,omitempty" json:"concurrency_per_worker,omitempty"`
	TargetUtilization    *float64 `toml:"target_utilization,omitempty" json:"target_utilization,omitempty"`
	WorkerKeepAlive      string   `toml:"worker_keep_alive,omitempty" json:"worker_keep_alive,omitempty"`
}

type PlacementManifest struct {
	SandboxGroup      string `toml:"sandbox_group,omitempty" json:"sandbox_group"`
	MinimumSandboxes  *int   `toml:"minimum_sandboxes,omitempty" json:"minimum_sandboxes,omitempty"`
	WorkersPerSandbox *int   `toml:"workers_per_sandbox,omitempty" json:"workers_per_sandbox,omitempty"`
}

type UnauthenticatedManifest struct {
	Action      string `toml:"action,omitempty" json:"action"`
	Status      int    `toml:"status,omitempty" json:"status"`
	Message     string `toml:"message,omitempty" json:"message,omitempty"`
	RedirectURL string `toml:"redirect_url,omitempty" json:"redirect_url,omitempty"`
}

type AccessManifest struct {
	Mode            string                  `toml:"mode,omitempty" json:"mode"`
	Unauthenticated UnauthenticatedManifest `toml:"unauthenticated,omitempty" json:"unauthenticated"`
}

// ServiceManifest contains portable service metadata and defaults.
type ServiceManifest struct {
	Schema      int               `toml:"schema" json:"schema"`
	Description string            `toml:"description" json:"description"`
	Entrypoint  string            `toml:"entrypoint,omitempty" json:"entrypoint"`
	Lifecycle   LifecycleManifest `toml:"lifecycle,omitempty" json:"lifecycle"`
	OpenAPI     OpenAPIManifest   `toml:"openapi,omitempty" json:"openapi"`
	Scaling     ScalingManifest   `toml:"scaling,omitempty" json:"scaling"`
	Placement   PlacementManifest `toml:"placement,omitempty" json:"placement"`
	Access      AccessManifest    `toml:"access,omitempty" json:"access"`
}

type LifecycleOverrides struct {
	ServiceType      *string `json:"service_type,omitempty"`
	SessionKeepAlive *string `json:"session_keep_alive,omitempty"`
}

type ScalingOverrides struct {
	MinimumWorkers       *int     `json:"minimum_workers,omitempty"`
	MaximumWorkers       *int     `json:"maximum_workers,omitempty"`
	ConcurrencyPerWorker *int     `json:"concurrency_per_worker,omitempty"`
	TargetUtilization    *float64 `json:"target_utilization,omitempty"`
	WorkerKeepAlive      *string  `json:"worker_keep_alive,omitempty"`
}

type PlacementOverrides struct {
	SandboxGroup      *string `json:"sandbox_group,omitempty"`
	MinimumSandboxes  *int    `json:"minimum_sandboxes,omitempty"`
	WorkersPerSandbox *int    `json:"workers_per_sandbox,omitempty"`
}

// DesiredServiceState is shared desired state. Node-local process identity never
// belongs in this document.
type DesiredServiceState struct {
	Enabled    bool               `json:"enabled"`
	Generation uint64             `json:"generation"`
	Lifecycle  LifecycleOverrides `json:"lifecycle"`
	Scaling    ScalingOverrides   `json:"scaling"`
	Placement  PlacementOverrides `json:"placement"`
}

type TimeoutConfiguration struct {
	Request time.Duration `json:"request"`
	Drain   time.Duration `json:"drain"`
	Idle    time.Duration `json:"idle"`
}

// FrameworkDefaults is the single source of initial service defaults.
type FrameworkDefaults struct {
	SessionKeepAlive time.Duration          `json:"session_keep_alive"`
	Scaling          ScalingConfiguration   `json:"scaling"`
	Placement        PlacementConfiguration `json:"placement"`
	Timeouts         TimeoutConfiguration   `json:"timeouts"`
	DependencyMode   string                 `json:"dependency_mode"`
}

func DefaultFrameworkDefaults() FrameworkDefaults {
	return FrameworkDefaults{
		SessionKeepAlive: 10 * time.Minute,
		Scaling: ScalingConfiguration{
			MinimumWorkers: 0, MaximumWorkers: 0,
			ConcurrencyPerWorker: 32, TargetUtilization: 0.7,
			WorkerKeepAlive: 2 * time.Minute,
		},
		Placement:      PlacementConfiguration{MinimumSandboxes: 0, WorkersPerSandbox: 4},
		Timeouts:       TimeoutConfiguration{Request: 30 * time.Second, Drain: 30 * time.Second},
		DependencyMode: "cached-only",
	}
}

type LifecycleConfiguration struct {
	ServiceType      string        `json:"service_type"`
	SessionKeepAlive time.Duration `json:"session_keep_alive"`
}

type ScalingConfiguration struct {
	MinimumWorkers       int           `json:"minimum_workers"`
	MaximumWorkers       int           `json:"maximum_workers"`
	ConcurrencyPerWorker int           `json:"concurrency_per_worker"`
	TargetUtilization    float64       `json:"target_utilization"`
	WorkerKeepAlive      time.Duration `json:"worker_keep_alive"`
}

type PlacementConfiguration struct {
	SandboxGroup      string `json:"sandbox_group"`
	MinimumSandboxes  int    `json:"minimum_sandboxes"`
	WorkersPerSandbox int    `json:"workers_per_sandbox"`
}

type EffectiveConfiguration struct {
	Lifecycle      LifecycleConfiguration `json:"lifecycle"`
	Scaling        ScalingConfiguration   `json:"scaling"`
	Placement      PlacementConfiguration `json:"placement"`
	Timeouts       TimeoutConfiguration   `json:"-"`
	DependencyMode string                 `json:"-"`
}

type Package struct {
	ID                string           `json:"package_id"`
	Path              string           `json:"path"`
	Description       string           `json:"description,omitempty"`
	DocumentationURL  string           `json:"documentation_url,omitempty"`
	License           string           `json:"license,omitempty"`
	Valid             bool             `json:"valid"`
	ServiceCount      int              `json:"service_count"`
	Services          []PackageService `json:"services,omitempty"`
	Programs          []PackageProgram `json:"programs,omitempty"`
	Files             []PackageFile    `json:"files,omitempty"`
	ContentsTruncated bool             `json:"contents_truncated,omitempty"`
	ValidationErrors  []string         `json:"validation_errors,omitempty"`
	InspectionErrors  []string         `json:"inspection_errors,omitempty"`
}

// PackageService is portable service metadata read without creating or reading
// mutable desired state.
type PackageService struct {
	ID               string   `json:"service_id"`
	Path             string   `json:"path"`
	Description      string   `json:"description,omitempty"`
	ServiceType      string   `json:"service_type,omitempty"`
	AccessMode       string   `json:"access_mode,omitempty"`
	Entrypoint       string   `json:"entrypoint,omitempty"`
	Valid            bool     `json:"valid"`
	ValidationErrors []string `json:"validation_errors,omitempty"`
}

// PackageProgram is one fixed-depth program manifest under a package.
type PackageProgram struct {
	ID               string   `json:"program_id"`
	Path             string   `json:"path"`
	Description      string   `json:"description,omitempty"`
	Entrypoint       string   `json:"entrypoint,omitempty"`
	DefaultLayout    string   `json:"default_layout,omitempty"`
	Discoverable     bool     `json:"discoverable"`
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
}

type Service struct {
	ID                string   `json:"service_id"`
	PackageID         string   `json:"package_id"`
	Path              string   `json:"path"`
	CanonicalBasePath string   `json:"canonical_base_path"`
	Description       string   `json:"description,omitempty"`
	ServiceType       string   `json:"service_type,omitempty"`
	AccessMode        string   `json:"access_mode,omitempty"`
	Entrypoint        string   `json:"entrypoint,omitempty"`
	Enabled           bool     `json:"enabled"`
	DesiredGeneration uint64   `json:"desired_generation"`
	Valid             bool     `json:"valid"`
	ValidationErrors  []string `json:"validation_errors,omitempty"`
}

// Definition is the complete exact-on-disk service definition used for one
// validation, reconciliation, or request decision.
type Definition struct {
	Identity       Identity               `json:"identity"`
	PackagePath    string                 `json:"package_path"`
	ServicePath    string                 `json:"service_path"`
	EntrypointPath string                 `json:"entrypoint_path"`
	EntrypointURL  string                 `json:"entrypoint_url"`
	Package        PackageManifest        `json:"package"`
	Service        ServiceManifest        `json:"service"`
	State          DesiredServiceState    `json:"state"`
	StateExists    bool                   `json:"state_exists"`
	Effective      EffectiveConfiguration `json:"effective"`
}

type Config struct {
	WorkspaceRoot string
	PackagesRoot  string
	GitPath       string
	RepositoryMu  *sync.RWMutex
	Secrets       SecretResolver
	Database      database.Store
	StateStore    ServiceStateStore
	IndexStore    PackageIndexStore
	Defaults      FrameworkDefaults
	Logger        *slog.Logger
}

type Store struct {
	catalog       *Catalog
	workspaceRoot string
	packagesRoot  string
	state         ServiceStateStore
	index         PackageIndexStore
	gitPath       string
	repositoryMu  *sync.RWMutex
	packageLocks  sync.Map
	secrets       SecretResolver
	defaults      FrameworkDefaults
	logger        *slog.Logger
	deploymentMu  sync.RWMutex
	deployment    deployment.SchemaHook
	databaseIndex bool
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
	defaults := config.Defaults
	if defaults == (FrameworkDefaults{}) {
		defaults = DefaultFrameworkDefaults()
	}
	if err := validateEffective(EffectiveConfiguration{
		Lifecycle: LifecycleConfiguration{ServiceType: ServiceTypeStateless, SessionKeepAlive: defaults.SessionKeepAlive},
		Scaling:   defaults.Scaling, Placement: defaults.Placement, Timeouts: defaults.Timeouts, DependencyMode: defaults.DependencyMode,
	}); err != nil {
		return nil, fmt.Errorf("framework defaults: %w", err)
	}
	stateStore := config.StateStore
	if stateStore == nil {
		stateStore, err = NewDatabaseServiceStateStore(config.Database)
		if err != nil {
			return nil, fmt.Errorf("service state database: %w", err)
		}
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
		workspaceRoot: workspace, packagesRoot: packagesRoot, state: stateStore,
		index:   indexStore,
		gitPath: gitPath, repositoryMu: repositoryMu, secrets: config.Secrets, defaults: defaults,
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
	result.Services, err = s.inspectPackageServices(identity, result.Path)
	if err != nil {
		result.InspectionErrors = append(result.InspectionErrors, err.Error())
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

func (s *Store) inspectPackageServices(identity Identity, root string) ([]PackageService, error) {
	entries, err := os.ReadDir(filepath.Join(root, "services"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read package services: %w", err)
	}
	result := make([]PackageService, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || (!entry.IsDir() && entry.Type()&os.ModeSymlink == 0) {
			continue
		}
		manifestPath := filepath.Join(root, "services", entry.Name(), "service.toml")
		if _, statErr := os.Lstat(manifestPath); errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		serviceID := identity.PackageID() + "/" + entry.Name()
		item := PackageService{ID: serviceID, Path: filepath.ToSlash(filepath.Join("services", entry.Name()))}
		serviceIdentity, parseErr := ParseServiceID(serviceID)
		if parseErr != nil {
			item.ValidationErrors = []string{parseErr.Error()}
			result = append(result, item)
			continue
		}
		definition, definitionErr := s.readPortableService(serviceIdentity)
		if definitionErr != nil {
			item.ValidationErrors = []string{definitionErr.Error()}
		} else {
			item.Description = definition.Service.Description
			item.ServiceType = definition.Service.Lifecycle.ServiceType
			item.AccessMode = definition.Service.Access.Mode
			item.Entrypoint = filepath.ToSlash(filepath.Join(item.Path, definition.Service.Entrypoint))
			item.Valid = true
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
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

func (s *Store) ListServices() ([]Service, error) {
	packages, err := s.ListPackages()
	if err != nil {
		return nil, err
	}
	var result []Service
	for _, item := range packages {
		canonicalPackage, pathErr := canonicalWithin(item.Path, s.packagesRoot)
		if pathErr != nil || canonicalPackage != item.Path {
			continue
		}
		entries, readErr := os.ReadDir(filepath.Join(item.Path, "services"))
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, readErr
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") || (!entry.IsDir() && entry.Type()&os.ModeSymlink == 0) {
				continue
			}
			manifest := filepath.Join(item.Path, "services", entry.Name(), "service.toml")
			if _, statErr := os.Lstat(manifest); errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			serviceID := item.ID + "/" + entry.Name()
			service := Service{ID: serviceID, PackageID: item.ID, Path: filepath.Dir(manifest)}
			identity, parseErr := ParseServiceID(serviceID)
			if parseErr != nil {
				service.ValidationErrors = []string{parseErr.Error()}
				result = append(result, service)
				continue
			}
			service.CanonicalBasePath = identity.CanonicalBasePath()
			definition, definitionErr := s.ReadService(serviceID)
			if definitionErr != nil {
				service.ValidationErrors = []string{definitionErr.Error()}
			} else {
				service.Description, service.Entrypoint = definition.Service.Description, definition.EntrypointPath
				service.ServiceType, service.AccessMode = definition.Service.Lifecycle.ServiceType, definition.Service.Access.Mode
				service.Enabled, service.DesiredGeneration, service.Valid = definition.State.Enabled, definition.State.Generation, true
			}
			result = append(result, service)
			if s.logger != nil {
				level := slog.LevelInfo
				if !service.Valid {
					level = slog.LevelWarn
				}
				s.logger.Log(context.Background(), level, "service discovered", "package_id", service.PackageID, "service_id", service.ID, "canonical_base_path", service.CanonicalBasePath, "valid", service.Valid, "validation_errors", service.ValidationErrors)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

// ListStateServiceIDs reads the installed service declarations published by
// package activation. It never rescans package source during reconciliation.
func (s *Store) ListStateServiceIDs() ([]string, error) {
	records, err := s.state.List()
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(records))
	for _, record := range records {
		result = append(result, record.ServiceID)
	}
	sort.Strings(result)
	return result, nil
}

func (s *Store) ReadService(serviceID string) (Definition, error) {
	identity, err := ParseServiceID(serviceID)
	if err != nil {
		return Definition{}, err
	}
	if err := s.requireReadyPackage(context.Background(), identity.PackageID()); err != nil {
		return Definition{}, err
	}
	definition, err := s.readPortableService(identity)
	if err != nil {
		return Definition{}, err
	}
	state, exists, err := s.ReadState(serviceID)
	if err != nil {
		return Definition{}, err
	}
	if !exists {
		state, err = initialDesiredState(s.defaults, definition.Service, serviceID)
		if err != nil {
			return Definition{}, fmt.Errorf("service %s defaults: %w", serviceID, err)
		}
	}
	effective, err := calculateEffective(s.defaults, definition.Service, state)
	if err != nil {
		return Definition{}, fmt.Errorf("service %s effective policy: %w", serviceID, err)
	}
	definition.State, definition.StateExists, definition.Effective = state, exists, effective
	return definition, nil
}

func (s *Store) readPortableService(identity Identity) (Definition, error) {
	serviceID := identity.ServiceID()
	packagePath := filepath.Join(s.packagesRoot, identity.Namespace, identity.Repository)
	servicePath := filepath.Join(packagePath, "services", identity.Service)
	if canonical, err := canonicalWithin(packagePath, s.packagesRoot); err != nil || canonical != packagePath {
		if err == nil {
			err = errors.New("package root resolves through a symlink")
		}
		return Definition{}, fmt.Errorf("package %s: %w", identity.PackageID(), err)
	}
	if canonical, err := canonicalWithin(servicePath, packagePath); err != nil || canonical != servicePath {
		if err == nil {
			err = errors.New("service root resolves through a symlink")
		}
		return Definition{}, fmt.Errorf("service %s: %w", serviceID, err)
	}
	var packageManifest PackageManifest
	packageManifestPath := filepath.Join(packagePath, "package.toml")
	if err := decodeTOMLWithin(packageManifestPath, packagePath, &packageManifest); err != nil {
		return Definition{}, fmt.Errorf("%s: %w", packageManifestPath, err)
	}
	if packageManifest.Schema != packageManifestSchema {
		return Definition{}, fmt.Errorf("%s: schema must equal %d", packageManifestPath, packageManifestSchema)
	}
	serviceManifestPath := filepath.Join(servicePath, "service.toml")
	var serviceManifest ServiceManifest
	if err := decodeTOMLWithin(serviceManifestPath, servicePath, &serviceManifest); err != nil {
		return Definition{}, fmt.Errorf("%s: %w", serviceManifestPath, err)
	}
	if serviceManifest.Schema != serviceManifestSchema {
		return Definition{}, fmt.Errorf("%s: schema must equal %d", serviceManifestPath, serviceManifestSchema)
	}
	if err := normalizeAndValidateServicePolicy(&serviceManifest); err != nil {
		return Definition{}, fmt.Errorf("%s: %w", serviceManifestPath, err)
	}
	if serviceManifest.Entrypoint == "" {
		serviceManifest.Entrypoint = "service.ts"
	}
	if filepath.IsAbs(serviceManifest.Entrypoint) || filepath.Clean(serviceManifest.Entrypoint) != serviceManifest.Entrypoint || serviceManifest.Entrypoint == "." || strings.HasPrefix(serviceManifest.Entrypoint, ".."+string(filepath.Separator)) || strings.ContainsRune(serviceManifest.Entrypoint, '\x00') {
		return Definition{}, fmt.Errorf("%s: entrypoint must be a canonical relative path inside the service", serviceManifestPath)
	}
	entrypointPath := filepath.Join(servicePath, serviceManifest.Entrypoint)
	canonicalEntrypoint, err := canonicalWithin(entrypointPath, packagePath)
	if err != nil {
		return Definition{}, fmt.Errorf("entrypoint %s: %w", entrypointPath, err)
	}
	info, err := os.Stat(canonicalEntrypoint)
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("entrypoint is not a regular file")
		}
		return Definition{}, fmt.Errorf("entrypoint %s: %w", entrypointPath, err)
	}
	sandboxPath, err := filepath.Rel(s.packagesRoot, canonicalEntrypoint)
	if err != nil {
		return Definition{}, err
	}
	return Definition{Identity: identity, PackagePath: packagePath, ServicePath: servicePath, EntrypointPath: canonicalEntrypoint, EntrypointURL: "file:///workspace/packages/" + filepath.ToSlash(sandboxPath), Package: packageManifest, Service: serviceManifest}, nil
}

func normalizeAndValidateServicePolicy(manifest *ServiceManifest) error {
	if manifest.Lifecycle.ServiceType == "" {
		manifest.Lifecycle.ServiceType = ServiceTypeStateless
	}
	if manifest.Lifecycle.ServiceType != ServiceTypeStateless && manifest.Lifecycle.ServiceType != ServiceTypeSession {
		return errors.New("lifecycle.service_type must be stateless or session")
	}
	if value := manifest.Lifecycle.SessionKeepAlive; value != "" {
		if parsed, err := time.ParseDuration(value); err != nil || parsed <= 0 {
			return errors.New("lifecycle.session_keep_alive must be a positive duration")
		}
	}
	if err := validateWorkerBounds(manifest.Scaling.MinimumWorkers, manifest.Scaling.MaximumWorkers); err != nil {
		return err
	}
	if value := manifest.Scaling.ConcurrencyPerWorker; value != nil && *value < 1 {
		return errors.New("scaling.concurrency_per_worker must be at least 1")
	}
	if value := manifest.Scaling.WorkerKeepAlive; value != "" {
		if parsed, err := time.ParseDuration(value); err != nil || parsed <= 0 {
			return errors.New("scaling.worker_keep_alive must be a positive duration")
		}
	}
	if value := manifest.Scaling.TargetUtilization; value != nil && (*value <= 0 || *value > 1) {
		return errors.New("scaling.target_utilization must be greater than 0 and at most 1")
	}
	if manifest.Placement.SandboxGroup != strings.TrimSpace(manifest.Placement.SandboxGroup) || strings.ContainsRune(manifest.Placement.SandboxGroup, '\x00') {
		return errors.New("placement.sandbox_group must be trimmed and cannot contain a null byte")
	}
	if value := manifest.Placement.MinimumSandboxes; value != nil && *value < 0 {
		return errors.New("placement.minimum_sandboxes cannot be negative")
	}
	if value := manifest.Placement.WorkersPerSandbox; value != nil && *value < 1 {
		return errors.New("placement.workers_per_sandbox must be at least 1")
	}
	if manifest.Access.Mode == "" {
		manifest.Access.Mode = AccessModePublic
	}
	if manifest.Access.Mode != AccessModePublic && manifest.Access.Mode != AccessModeAuthenticated {
		return errors.New("access.mode must be public or authenticated")
	}
	if manifest.Access.Mode == AccessModePublic {
		// Keep one normalized declaration shape in the database even though the
		// unauthenticated policy is ignored for public services.
		manifest.Access.Unauthenticated = UnauthenticatedManifest{
			Action: UnauthenticatedReject, Status: 401, Message: "Authentication is required.",
		}
		return nil
	}
	policy := &manifest.Access.Unauthenticated
	if policy.Action == "" {
		policy.Action = UnauthenticatedReject
	}
	switch policy.Action {
	case UnauthenticatedReject:
		if policy.Status == 0 {
			policy.Status = 401
		}
		if policy.Status < 400 || policy.Status > 599 {
			return errors.New("authenticated reject status must be between 400 and 599")
		}
		if policy.Message == "" {
			policy.Message = "Authentication is required."
		}
	case UnauthenticatedRedirect:
		if policy.Status == 0 {
			policy.Status = 302
		}
		if policy.Status < 300 || policy.Status > 399 {
			return errors.New("authenticated redirect status must be between 300 and 399")
		}
		if policy.RedirectURL == "" || hasControlCharacter(policy.RedirectURL) {
			return errors.New("authenticated redirect_url must be a non-empty configured URL")
		}
		if _, err := url.Parse(policy.RedirectURL); err != nil {
			return fmt.Errorf("authenticated redirect_url: %w", err)
		}
	default:
		return errors.New("access.unauthenticated.action must be reject or redirect")
	}
	return nil
}

func validateWorkerBounds(minimum, maximum *int) error {
	if minimum != nil && *minimum < 0 {
		return errors.New("scaling.minimum_workers cannot be negative")
	}
	if maximum != nil && *maximum < 0 {
		return errors.New("scaling.maximum_workers cannot be negative")
	}
	if minimum != nil && maximum != nil && *maximum != 0 && *minimum > *maximum {
		return errors.New("scaling must satisfy minimum_workers <= maximum_workers when maximum_workers is nonzero")
	}
	return nil
}

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

func hasControlCharacter(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func (s *Store) ReadState(serviceID string) (DesiredServiceState, bool, error) {
	return s.state.Get(serviceID)
}

// MutateState serializes one desired-policy change and records a new immutable
// effective version before publishing it as desired.
func (s *Store) MutateState(ctx context.Context, serviceID string, mutate func(*DesiredServiceState) error) (DesiredServiceState, error) {
	identity, err := ParseServiceID(serviceID)
	if err != nil {
		return DesiredServiceState{}, err
	}
	if err := s.requireReadyPackage(ctx, identity.PackageID()); err != nil {
		return DesiredServiceState{}, err
	}
	if _, err := s.readPortableService(identity); err != nil {
		return DesiredServiceState{}, err
	}
	unlock, err := s.state.Lock(ctx, serviceID)
	if err != nil {
		return DesiredServiceState{}, err
	}
	defer func() { _ = unlock() }()
	state, exists, err := s.ReadState(serviceID)
	if err != nil {
		return DesiredServiceState{}, err
	}
	if !exists {
		return DesiredServiceState{}, fmt.Errorf("service %s is not installed", serviceID)
	}
	if mutate != nil {
		if err := mutate(&state); err != nil {
			return DesiredServiceState{}, err
		}
	}
	state.Generation++
	definition, err := s.readServiceWithState(identity, state)
	if err != nil {
		return DesiredServiceState{}, err
	}
	if err := validateEffective(definition.Effective); err != nil {
		return DesiredServiceState{}, err
	}
	if desired, ok := s.state.(ServiceDesiredStateStore); ok {
		commit, commitErr := s.packageCommit(ctx, definition.PackagePath)
		if commitErr != nil {
			return DesiredServiceState{}, commitErr
		}
		if err := desired.UpdateDesiredDefinition(ctx, definition, state, definition.Effective, commit); err != nil {
			return DesiredServiceState{}, err
		}
	} else if definitions, ok := s.state.(ServiceDefinitionStore); ok {
		commit, commitErr := s.packageCommit(ctx, definition.PackagePath)
		if commitErr != nil {
			return DesiredServiceState{}, commitErr
		}
		if err := definitions.InstallDefinition(ctx, definition, state, definition.Effective, commit); err != nil {
			return DesiredServiceState{}, err
		}
	} else if err := s.state.Put(serviceID, state); err != nil {
		return DesiredServiceState{}, err
	}
	return state, nil
}

func (s *Store) packageCommit(ctx context.Context, path string) (string, error) {
	commit, err := s.gitValue(ctx, path, "rev-parse", "--verify", "HEAD^{commit}")
	if err == nil && commit != "" {
		return commit, nil
	}
	return FingerprintPackage(path)
}

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

func initialDesiredState(defaults FrameworkDefaults, manifest ServiceManifest, serviceID string) (DesiredServiceState, error) {
	if _, err := calculateEffective(defaults, manifest, DesiredServiceState{}); err != nil {
		return DesiredServiceState{}, err
	}
	return DesiredServiceState{
		Enabled: manifest.Lifecycle.DefaultEnabled,
	}, nil
}

func (s *Store) readServiceWithState(identity Identity, state DesiredServiceState) (Definition, error) {
	definition, err := s.readPortableService(identity)
	if err != nil {
		return Definition{}, err
	}
	effective, err := calculateEffective(s.defaults, definition.Service, state)
	if err != nil {
		return Definition{}, err
	}
	definition.State, definition.StateExists, definition.Effective = state, true, effective
	return definition, nil
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

func calculateEffective(defaults FrameworkDefaults, manifest ServiceManifest, state DesiredServiceState) (EffectiveConfiguration, error) {
	result := EffectiveConfiguration{
		Lifecycle:      LifecycleConfiguration{ServiceType: manifest.Lifecycle.ServiceType, SessionKeepAlive: defaults.SessionKeepAlive},
		Scaling:        defaults.Scaling,
		Placement:      defaults.Placement,
		Timeouts:       defaults.Timeouts,
		DependencyMode: defaults.DependencyMode,
	}
	result.Placement.SandboxGroup = manifest.Placement.SandboxGroup
	if manifest.Lifecycle.SessionKeepAlive != "" {
		result.Lifecycle.SessionKeepAlive, _ = time.ParseDuration(manifest.Lifecycle.SessionKeepAlive)
	}
	if manifest.Placement.MinimumSandboxes != nil {
		result.Placement.MinimumSandboxes = *manifest.Placement.MinimumSandboxes
	}
	if manifest.Placement.WorkersPerSandbox != nil {
		result.Placement.WorkersPerSandbox = *manifest.Placement.WorkersPerSandbox
	}
	applyScalingManifest(&result.Scaling, manifest.Scaling)
	if state.Lifecycle.ServiceType != nil {
		result.Lifecycle.ServiceType = *state.Lifecycle.ServiceType
	}
	if state.Lifecycle.SessionKeepAlive != nil {
		parsed, err := time.ParseDuration(*state.Lifecycle.SessionKeepAlive)
		if err != nil {
			return result, invalidServicePolicy(fmt.Sprintf("lifecycle.session_keep_alive: %v", err))
		}
		result.Lifecycle.SessionKeepAlive = parsed
	}
	if err := applyScalingOverrides(&result.Scaling, state.Scaling); err != nil {
		return result, invalidServicePolicy(err.Error())
	}
	if state.Placement.SandboxGroup != nil {
		result.Placement.SandboxGroup = *state.Placement.SandboxGroup
	}
	if state.Placement.MinimumSandboxes != nil {
		result.Placement.MinimumSandboxes = *state.Placement.MinimumSandboxes
	}
	if state.Placement.WorkersPerSandbox != nil {
		result.Placement.WorkersPerSandbox = *state.Placement.WorkersPerSandbox
	}
	return result, validateEffective(result)
}

func applyScalingManifest(target *ScalingConfiguration, source ScalingManifest) {
	if source.MinimumWorkers != nil {
		target.MinimumWorkers = *source.MinimumWorkers
	}
	if source.MaximumWorkers != nil {
		target.MaximumWorkers = *source.MaximumWorkers
	}
	if source.ConcurrencyPerWorker != nil {
		target.ConcurrencyPerWorker = *source.ConcurrencyPerWorker
	}
	if source.TargetUtilization != nil {
		target.TargetUtilization = *source.TargetUtilization
	}
	if source.WorkerKeepAlive != "" {
		target.WorkerKeepAlive, _ = time.ParseDuration(source.WorkerKeepAlive)
	}
}

func applyScalingOverrides(target *ScalingConfiguration, source ScalingOverrides) error {
	if source.MinimumWorkers != nil {
		target.MinimumWorkers = *source.MinimumWorkers
	}
	if source.MaximumWorkers != nil {
		target.MaximumWorkers = *source.MaximumWorkers
	}
	if source.ConcurrencyPerWorker != nil {
		target.ConcurrencyPerWorker = *source.ConcurrencyPerWorker
	}
	if source.TargetUtilization != nil {
		target.TargetUtilization = *source.TargetUtilization
	}
	if source.WorkerKeepAlive != nil {
		parsed, err := time.ParseDuration(*source.WorkerKeepAlive)
		if err != nil {
			return fmt.Errorf("scaling.worker_keep_alive: %w", err)
		}
		target.WorkerKeepAlive = parsed
	}
	return nil
}

func validateEffective(value EffectiveConfiguration) error {
	if value.Lifecycle.ServiceType != ServiceTypeStateless && value.Lifecycle.ServiceType != ServiceTypeSession {
		return invalidServicePolicy("lifecycle.service_type must be stateless or session")
	}
	if value.Lifecycle.SessionKeepAlive <= 0 {
		return invalidServicePolicy("lifecycle.session_keep_alive must be positive")
	}
	if value.Scaling.MinimumWorkers < 0 || value.Scaling.MaximumWorkers < 0 || (value.Scaling.MaximumWorkers != 0 && value.Scaling.MinimumWorkers > value.Scaling.MaximumWorkers) {
		return invalidServicePolicy("scaling workers must satisfy minimum_workers >= 0 and maximum_workers = 0 or maximum_workers >= minimum_workers")
	}
	if value.Scaling.ConcurrencyPerWorker < 1 {
		return invalidServicePolicy("scaling.concurrency_per_worker must be at least 1")
	}
	if value.Scaling.TargetUtilization <= 0 || value.Scaling.TargetUtilization > 1 {
		return invalidServicePolicy("scaling.target_utilization must be greater than 0 and at most 1")
	}
	if value.Scaling.WorkerKeepAlive <= 0 {
		return invalidServicePolicy("scaling.worker_keep_alive must be positive")
	}
	if value.Timeouts.Request <= 0 || value.Timeouts.Drain <= 0 || value.Timeouts.Idle < 0 {
		return invalidServicePolicy("request and drain timeouts must be positive and idle timeout cannot be negative")
	}
	if value.DependencyMode != "cached-only" && value.DependencyMode != "online" {
		return invalidServicePolicy("dependency_mode must be cached-only or online")
	}
	if value.Placement.SandboxGroup != strings.TrimSpace(value.Placement.SandboxGroup) || strings.ContainsRune(value.Placement.SandboxGroup, '\x00') {
		return invalidServicePolicy("placement.sandbox_group must be trimmed and cannot contain a null byte")
	}
	if value.Placement.MinimumSandboxes < 0 {
		return invalidServicePolicy("placement.minimum_sandboxes cannot be negative")
	}
	if value.Placement.WorkersPerSandbox < 1 {
		return invalidServicePolicy("placement.workers_per_sandbox must be at least 1")
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
