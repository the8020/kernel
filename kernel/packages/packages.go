// Package packages owns filesystem-derived package and service definitions and
// shared desired state.
package packages

import (
	"context"
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
)

const (
	manifestSchema              = 1
	manifestLimit               = 1 << 20
	packageInspectionEntryLimit = 5000
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

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
	DefaultEnabled bool `toml:"default_enabled" json:"default_enabled"`
}

type OpenAPIManifest struct {
	Title       string `toml:"title,omitempty" json:"title,omitempty"`
	Version     string `toml:"version,omitempty" json:"version,omitempty"`
	Description string `toml:"description,omitempty" json:"description,omitempty"`
}

const (
	ExecutionModeStateless  = "stateless"
	ExecutionModePersistent = "persistent"
	AccessModePublic        = "public"
	AccessModeAuthenticated = "authenticated"
	UnauthenticatedReject   = "reject"
	UnauthenticatedRedirect = "redirect"
)

type ExecutionManifest struct {
	Mode                 string `toml:"mode,omitempty" json:"mode"`
	ConcurrencyPerWorker *int   `toml:"concurrency_per_worker,omitempty" json:"concurrency_per_worker,omitempty"`
	KeepAlive            string `toml:"keep_alive,omitempty" json:"keep_alive,omitempty"`
}

// ScalingManifest is the complete service-owned scaling contract. Runtime
// desired counts, cooldowns, and placement decisions are kernel policy rather
// than portable service configuration.
type ScalingManifest struct {
	ReplicasMinimum          *int     `toml:"replicas_min,omitempty" json:"replicas_min,omitempty"`
	ReplicasMaximum          *int     `toml:"replicas_max,omitempty" json:"replicas_max,omitempty"`
	WorkersPerReplicaMinimum *int     `toml:"workers_per_replica_min,omitempty" json:"workers_per_replica_min,omitempty"`
	WorkersPerReplicaMaximum *int     `toml:"workers_per_replica_max,omitempty" json:"workers_per_replica_max,omitempty"`
	TargetUtilization        *float64 `toml:"target_utilization,omitempty" json:"target_utilization,omitempty"`
}

type PlacementManifest struct {
	SandboxGroup string `toml:"sandbox_group,omitempty" json:"sandbox_group"`
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
	Execution   ExecutionManifest `toml:"execution,omitempty" json:"execution"`
	Scaling     ScalingManifest   `toml:"scaling,omitempty" json:"scaling"`
	Placement   PlacementManifest `toml:"placement,omitempty" json:"placement"`
	Access      AccessManifest    `toml:"access,omitempty" json:"access"`
}

type ExecutionOverrides struct {
	ConcurrencyPerWorker *int    `toml:"concurrency_per_worker,omitempty" json:"concurrency_per_worker,omitempty"`
	KeepAlive            *string `toml:"keep_alive,omitempty" json:"keep_alive,omitempty"`
}

type ScalingOverrides struct {
	ReplicasMinimum          *int     `toml:"replicas_min,omitempty" json:"replicas_min,omitempty"`
	ReplicasMaximum          *int     `toml:"replicas_max,omitempty" json:"replicas_max,omitempty"`
	WorkersPerReplicaMinimum *int     `toml:"workers_per_replica_min,omitempty" json:"workers_per_replica_min,omitempty"`
	WorkersPerReplicaMaximum *int     `toml:"workers_per_replica_max,omitempty" json:"workers_per_replica_max,omitempty"`
	TargetUtilization        *float64 `toml:"target_utilization,omitempty" json:"target_utilization,omitempty"`
}

type PlacementOverrides struct {
	SandboxGroup *string `toml:"sandbox_group,omitempty" json:"sandbox_group,omitempty"`
}

// DesiredServiceState is shared desired state. Node-local process identity never
// belongs in this document.
type DesiredServiceState struct {
	Schema     int                `toml:"schema" json:"schema"`
	Enabled    bool               `toml:"enabled" json:"enabled"`
	Generation uint64             `toml:"generation" json:"generation"`
	Execution  ExecutionOverrides `toml:"execution,omitempty" json:"execution"`
	Scaling    ScalingOverrides   `toml:"scaling,omitempty" json:"scaling"`
	Placement  PlacementOverrides `toml:"placement,omitempty" json:"placement"`
}

type TimeoutConfiguration struct {
	Request time.Duration `json:"request"`
	Drain   time.Duration `json:"drain"`
	Idle    time.Duration `json:"idle"`
}

// FrameworkDefaults is the single source of initial service defaults.
type FrameworkDefaults struct {
	ConcurrencyPerWorker int                  `json:"concurrency_per_worker"`
	PersistentKeepAlive  time.Duration        `json:"persistent_keep_alive"`
	Scaling              ScalingConfiguration `json:"scaling"`
	Timeouts             TimeoutConfiguration `json:"timeouts"`
	DependencyMode       string               `json:"dependency_mode"`
}

func DefaultFrameworkDefaults() FrameworkDefaults {
	return FrameworkDefaults{
		ConcurrencyPerWorker: 32,
		PersistentKeepAlive:  2 * time.Minute,
		Scaling: ScalingConfiguration{
			ReplicasMinimum: 1, ReplicasMaximum: 1,
			WorkersPerReplicaMinimum: 1, WorkersPerReplicaMaximum: 4,
			TargetUtilization: 0.7,
		},
		Timeouts:       TimeoutConfiguration{Request: 30 * time.Second, Drain: 30 * time.Second},
		DependencyMode: "cached-only",
	}
}

type ExecutionConfiguration struct {
	Mode                 string        `json:"mode"`
	ConcurrencyPerWorker int           `json:"concurrency_per_worker"`
	KeepAlive            time.Duration `json:"keep_alive"`
}

type ScalingConfiguration struct {
	ReplicasMinimum          int     `json:"replicas_min"`
	ReplicasMaximum          int     `json:"replicas_max"`
	WorkersPerReplicaMinimum int     `json:"workers_per_replica_min"`
	WorkersPerReplicaMaximum int     `json:"workers_per_replica_max"`
	TargetUtilization        float64 `json:"target_utilization"`
}

type PlacementConfiguration struct {
	SandboxGroup string `json:"sandbox_group"`
}

type EffectiveConfiguration struct {
	Execution      ExecutionConfiguration `json:"execution"`
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
	ExecutionMode    string   `json:"execution_mode,omitempty"`
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
	ExecutionMode     string   `json:"execution_mode,omitempty"`
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
	WorkspaceRoot    string
	PackagesRoot     string
	StateRoot        string
	IndexRoot        string
	GitPath          string
	RepositoryMu     *sync.RWMutex
	StateStore       ServiceStateStore
	StateLockTimeout time.Duration
	Defaults         FrameworkDefaults
	Logger           *slog.Logger
}

type Store struct {
	workspaceRoot string
	packagesRoot  string
	state         ServiceStateStore
	stateRoot     string
	indexRoot     string
	gitPath       string
	repositoryMu  *sync.RWMutex
	indexMu       sync.RWMutex
	defaults      FrameworkDefaults
	logger        *slog.Logger
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
		Execution: ExecutionConfiguration{Mode: ExecutionModeStateless, ConcurrencyPerWorker: defaults.ConcurrencyPerWorker, KeepAlive: defaults.PersistentKeepAlive},
		Scaling:   defaults.Scaling, Timeouts: defaults.Timeouts, DependencyMode: defaults.DependencyMode,
	}); err != nil {
		return nil, fmt.Errorf("framework defaults: %w", err)
	}
	if config.StateLockTimeout <= 0 {
		config.StateLockTimeout = 5 * time.Second
	}
	stateStore := config.StateStore
	stateRoot := ""
	if stateStore == nil {
		stateRoot, err = resolveDirectory(workspace, config.StateRoot, filepath.Join("state", "services"))
		if err != nil {
			return nil, fmt.Errorf("service state root: %w", err)
		}
		fileStore, fileErr := NewFileServiceStateStore(stateRoot, config.StateLockTimeout)
		if fileErr != nil {
			return nil, fileErr
		}
		stateStore = fileStore
	}
	indexRoot, err := resolveDirectory(workspace, config.IndexRoot, filepath.Join("state", "package-index"))
	if err != nil {
		return nil, fmt.Errorf("package index root: %w", err)
	}
	repositoryMu := config.RepositoryMu
	if repositoryMu == nil {
		repositoryMu = &sync.RWMutex{}
	}
	gitPath := strings.TrimSpace(config.GitPath)
	if gitPath == "" {
		gitPath = "git"
	}
	return &Store{
		workspaceRoot: workspace, packagesRoot: packagesRoot, state: stateStore,
		stateRoot: stateRoot, indexRoot: indexRoot,
		gitPath: gitPath, repositoryMu: repositoryMu, defaults: defaults,
		logger: config.Logger,
	}, nil
}

func (s *Store) PackagesRoot() string { return s.packagesRoot }
func (s *Store) StateRoot() string    { return s.stateRoot }
func (s *Store) IndexRoot() string    { return s.indexRoot }

func (s *Store) ListPackages() ([]Package, error) {
	namespaces, err := os.ReadDir(s.packagesRoot)
	if err != nil {
		return nil, fmt.Errorf("read packages root: %w", err)
	}
	var result []Package
	for _, namespace := range namespaces {
		if strings.HasPrefix(namespace.Name(), ".") || (!namespace.IsDir() && namespace.Type()&os.ModeSymlink == 0) {
			continue
		}
		repositories, readErr := os.ReadDir(filepath.Join(s.packagesRoot, namespace.Name()))
		if readErr != nil {
			result = append(result, Package{ID: namespace.Name() + "/?", Path: filepath.Join(s.packagesRoot, namespace.Name()), ValidationErrors: []string{readErr.Error()}})
			continue
		}
		for _, repository := range repositories {
			if strings.HasPrefix(repository.Name(), ".") || (!repository.IsDir() && repository.Type()&os.ModeSymlink == 0) {
				continue
			}
			manifestPath := filepath.Join(s.packagesRoot, namespace.Name(), repository.Name(), "package.toml")
			if _, statErr := os.Lstat(manifestPath); errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			identity := Identity{Namespace: namespace.Name(), Repository: repository.Name()}
			item := s.inspectPackage(identity)
			result = append(result, item)
			if s.logger != nil {
				level := slog.LevelInfo
				if !item.Valid {
					level = slog.LevelWarn
				}
				s.logger.Log(context.Background(), level, "package discovered", "package_id", item.ID, "path", item.Path, "valid", item.Valid, "service_count", item.ServiceCount, "validation_errors", item.ValidationErrors)
			}
		}
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

func (s *Store) inspectPackage(identity Identity) Package {
	root := filepath.Join(s.packagesRoot, identity.Namespace, identity.Repository)
	result := Package{ID: identity.PackageID(), Path: root}
	if err := ValidateName(identity.Namespace); err != nil {
		result.ValidationErrors = append(result.ValidationErrors, "namespace: "+err.Error())
	}
	if err := ValidateName(identity.Repository); err != nil {
		result.ValidationErrors = append(result.ValidationErrors, "repository: "+err.Error())
	}
	canonical, err := canonicalWithin(root, s.packagesRoot)
	rootValid := err == nil && canonical == root
	if err != nil {
		result.ValidationErrors = append(result.ValidationErrors, fmt.Sprintf("%s: %v", root, err))
	} else if canonical != root {
		result.ValidationErrors = append(result.ValidationErrors, fmt.Sprintf("package root %s resolves through a symlink", root))
	}
	if rootValid {
		var manifest PackageManifest
		manifestPath := filepath.Join(root, "package.toml")
		if err := decodeTOMLWithin(manifestPath, root, &manifest); err != nil {
			result.ValidationErrors = append(result.ValidationErrors, fmt.Sprintf("%s: %v", manifestPath, err))
		} else {
			if manifest.Schema != manifestSchema {
				result.ValidationErrors = append(result.ValidationErrors, fmt.Sprintf("%s: schema must equal %d", manifestPath, manifestSchema))
			}
			result.Description, result.DocumentationURL, result.License = manifest.Description, manifest.DocumentationURL, manifest.License
		}
		services, err := os.ReadDir(filepath.Join(root, "services"))
		if err == nil {
			for _, entry := range services {
				if strings.HasPrefix(entry.Name(), ".") || (!entry.IsDir() && entry.Type()&os.ModeSymlink == 0) {
					continue
				}
				if _, statErr := os.Lstat(filepath.Join(root, "services", entry.Name(), "service.toml")); statErr == nil {
					result.ServiceCount++
				}
			}
		}
	}
	result.Valid = len(result.ValidationErrors) == 0
	return result
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
			item.ExecutionMode = definition.Service.Execution.Mode
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
				if manifest.Schema != manifestSchema {
					item.ValidationErrors = append(item.ValidationErrors, fmt.Sprintf("%s: schema must equal %d", manifestPath, manifestSchema))
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
				service.ExecutionMode, service.AccessMode = definition.Service.Execution.Mode, definition.Service.Access.Mode
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

// ListStateServiceIDs first discovers portable services so their initial
// desired state is materialized, then lists the store without depending on its
// physical backend.
func (s *Store) ListStateServiceIDs() ([]string, error) {
	if _, err := s.ListServices(); err != nil {
		return nil, err
	}
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
	definition, err := s.readPortableService(identity)
	if err != nil {
		return Definition{}, err
	}
	state, exists, err := s.ReadState(serviceID)
	if err != nil {
		return Definition{}, err
	}
	if !exists {
		state, err = s.initializeState(identity, definition.Service)
		if err != nil {
			return Definition{}, err
		}
		exists = true
	}
	effective, err := calculateEffective(s.defaults, definition.Service, state)
	if err != nil {
		return Definition{}, fmt.Errorf("service %s defaults/state: %w", serviceID, err)
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
	if packageManifest.Schema != manifestSchema {
		return Definition{}, fmt.Errorf("%s: schema must equal %d", packageManifestPath, manifestSchema)
	}
	var serviceManifest ServiceManifest
	serviceManifestPath := filepath.Join(servicePath, "service.toml")
	if err := decodeTOMLWithin(serviceManifestPath, servicePath, &serviceManifest); err != nil {
		return Definition{}, fmt.Errorf("%s: %w", serviceManifestPath, err)
	}
	if serviceManifest.Schema != manifestSchema {
		return Definition{}, fmt.Errorf("%s: schema must equal %d", serviceManifestPath, manifestSchema)
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
	if manifest.Execution.Mode == "" {
		manifest.Execution.Mode = ExecutionModeStateless
	}
	if manifest.Execution.Mode != ExecutionModeStateless && manifest.Execution.Mode != ExecutionModePersistent {
		return errors.New("execution.mode must be stateless or persistent")
	}
	if value := manifest.Execution.ConcurrencyPerWorker; value != nil && *value < 1 {
		return errors.New("execution.concurrency_per_worker must be at least 1")
	}
	if value := manifest.Execution.KeepAlive; value != "" {
		if parsed, err := time.ParseDuration(value); err != nil || parsed <= 0 {
			return errors.New("execution.keep_alive must be a positive duration")
		}
		if manifest.Execution.Mode != ExecutionModePersistent {
			return errors.New("execution.keep_alive is valid only for persistent services")
		}
	}
	if err := validateOptionalBounds("scaling replicas", manifest.Scaling.ReplicasMinimum, manifest.Scaling.ReplicasMaximum); err != nil {
		return err
	}
	if err := validateOptionalBounds("scaling workers per replica", manifest.Scaling.WorkersPerReplicaMinimum, manifest.Scaling.WorkersPerReplicaMaximum); err != nil {
		return err
	}
	if value := manifest.Scaling.TargetUtilization; value != nil && (*value <= 0 || *value > 1) {
		return errors.New("scaling.target_utilization must be greater than 0 and at most 1")
	}
	if manifest.Placement.SandboxGroup != strings.TrimSpace(manifest.Placement.SandboxGroup) || strings.ContainsRune(manifest.Placement.SandboxGroup, '\x00') {
		return errors.New("placement.sandbox_group must be trimmed and cannot contain a null byte")
	}
	if manifest.Access.Mode == "" {
		manifest.Access.Mode = AccessModePublic
	}
	if manifest.Access.Mode != AccessModePublic && manifest.Access.Mode != AccessModeAuthenticated {
		return errors.New("access.mode must be public or authenticated")
	}
	if manifest.Access.Mode == AccessModePublic {
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

func validateOptionalBounds(name string, minimum, maximum *int) error {
	if minimum != nil && *minimum < 0 {
		return fmt.Errorf("%s minimum cannot be negative", name)
	}
	if maximum != nil && *maximum < 1 {
		return fmt.Errorf("%s maximum must be at least 1", name)
	}
	if minimum != nil && maximum != nil && *minimum > *maximum {
		return fmt.Errorf("%s must satisfy minimum <= maximum", name)
	}
	return nil
}

func validAffinitySource(value string) bool {
	switch value {
	case "auth.user_id", "auth.username", "auth.realm":
		return true
	}
	for _, prefix := range []string{"header.", "cookie."} {
		if strings.HasPrefix(value, prefix) {
			name := strings.TrimPrefix(value, prefix)
			return namePattern.MatchString(name) && !strings.ContainsAny(name, "/\\\x00")
		}
	}
	return false
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

// MutateState serializes a desired-state mutation, increments generation once,
// and durably replaces state.toml.
func (s *Store) MutateState(ctx context.Context, serviceID string, mutate func(*DesiredServiceState) error) (DesiredServiceState, error) {
	identity, err := ParseServiceID(serviceID)
	if err != nil {
		return DesiredServiceState{}, err
	}
	portable, err := s.readPortableService(identity)
	if err != nil {
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
		state, err = initialDesiredState(s.defaults, portable.Service, serviceID)
		if err != nil {
			return DesiredServiceState{}, err
		}
	}
	if mutate != nil {
		if err := mutate(&state); err != nil {
			return DesiredServiceState{}, err
		}
	}
	state.Schema = manifestSchema
	state.Generation++
	definition, err := s.readServiceWithState(identity, state)
	if err != nil {
		return DesiredServiceState{}, err
	}
	if err := validateEffective(definition.Effective); err != nil {
		return DesiredServiceState{}, err
	}
	if err := s.state.Put(serviceID, state); err != nil {
		return DesiredServiceState{}, err
	}
	return state, nil
}

func (s *Store) initializeState(identity Identity, manifest ServiceManifest) (DesiredServiceState, error) {
	serviceID := identity.ServiceID()
	unlock, err := s.state.Lock(context.Background(), serviceID)
	if err != nil {
		return DesiredServiceState{}, err
	}
	defer func() { _ = unlock() }()
	if current, exists, getErr := s.state.Get(serviceID); getErr != nil {
		return DesiredServiceState{}, getErr
	} else if exists {
		return current, nil
	}
	state, err := initialDesiredState(s.defaults, manifest, serviceID)
	if err != nil {
		return DesiredServiceState{}, err
	}
	if err := s.state.Put(serviceID, state); err != nil {
		return DesiredServiceState{}, err
	}
	if s.logger != nil {
		s.logger.Info("service desired state initialized", "service_id", serviceID, "enabled", state.Enabled, "generation", state.Generation)
	}
	return state, nil
}

func initialDesiredState(defaults FrameworkDefaults, manifest ServiceManifest, serviceID string) (DesiredServiceState, error) {
	effective, err := calculateEffective(defaults, manifest, DesiredServiceState{Schema: manifestSchema})
	if err != nil {
		return DesiredServiceState{}, err
	}
	concurrency, keepAlive := effective.Execution.ConcurrencyPerWorker, effective.Execution.KeepAlive.String()
	replicasMinimum, replicasMaximum := effective.Scaling.ReplicasMinimum, effective.Scaling.ReplicasMaximum
	workersMinimum, workersMaximum := effective.Scaling.WorkersPerReplicaMinimum, effective.Scaling.WorkersPerReplicaMaximum
	targetUtilization, sandboxGroup := effective.Scaling.TargetUtilization, effective.Placement.SandboxGroup
	return DesiredServiceState{
		Schema: manifestSchema, Enabled: manifest.Lifecycle.DefaultEnabled,
		Execution: ExecutionOverrides{ConcurrencyPerWorker: &concurrency, KeepAlive: &keepAlive},
		Scaling: ScalingOverrides{
			ReplicasMinimum: &replicasMinimum, ReplicasMaximum: &replicasMaximum,
			WorkersPerReplicaMinimum: &workersMinimum, WorkersPerReplicaMaximum: &workersMaximum,
			TargetUtilization: &targetUtilization,
		},
		Placement: PlacementOverrides{SandboxGroup: &sandboxGroup},
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
		Execution:      ExecutionConfiguration{Mode: manifest.Execution.Mode, ConcurrencyPerWorker: defaults.ConcurrencyPerWorker, KeepAlive: defaults.PersistentKeepAlive},
		Scaling:        defaults.Scaling,
		Placement:      PlacementConfiguration{SandboxGroup: manifest.Placement.SandboxGroup},
		Timeouts:       defaults.Timeouts,
		DependencyMode: defaults.DependencyMode,
	}
	if manifest.Execution.ConcurrencyPerWorker != nil {
		result.Execution.ConcurrencyPerWorker = *manifest.Execution.ConcurrencyPerWorker
	}
	if manifest.Execution.KeepAlive != "" {
		result.Execution.KeepAlive, _ = time.ParseDuration(manifest.Execution.KeepAlive)
	}
	applyScalingManifest(&result.Scaling, manifest.Scaling)
	if state.Execution.ConcurrencyPerWorker != nil {
		result.Execution.ConcurrencyPerWorker = *state.Execution.ConcurrencyPerWorker
	}
	if state.Execution.KeepAlive != nil {
		parsed, err := time.ParseDuration(*state.Execution.KeepAlive)
		if err != nil {
			return result, fmt.Errorf("execution.keep_alive: %w", err)
		}
		result.Execution.KeepAlive = parsed
	}
	applyScalingOverrides(&result.Scaling, state.Scaling)
	if state.Placement.SandboxGroup != nil {
		result.Placement.SandboxGroup = *state.Placement.SandboxGroup
	}
	return result, validateEffective(result)
}

func applyScalingManifest(target *ScalingConfiguration, source ScalingManifest) {
	if source.ReplicasMinimum != nil {
		target.ReplicasMinimum = *source.ReplicasMinimum
	}
	if source.ReplicasMaximum != nil {
		target.ReplicasMaximum = *source.ReplicasMaximum
	}
	if source.WorkersPerReplicaMinimum != nil {
		target.WorkersPerReplicaMinimum = *source.WorkersPerReplicaMinimum
	}
	if source.WorkersPerReplicaMaximum != nil {
		target.WorkersPerReplicaMaximum = *source.WorkersPerReplicaMaximum
	}
	if source.TargetUtilization != nil {
		target.TargetUtilization = *source.TargetUtilization
	}
}

func applyScalingOverrides(target *ScalingConfiguration, source ScalingOverrides) {
	if source.ReplicasMinimum != nil {
		target.ReplicasMinimum = *source.ReplicasMinimum
	}
	if source.ReplicasMaximum != nil {
		target.ReplicasMaximum = *source.ReplicasMaximum
	}
	if source.WorkersPerReplicaMinimum != nil {
		target.WorkersPerReplicaMinimum = *source.WorkersPerReplicaMinimum
	}
	if source.WorkersPerReplicaMaximum != nil {
		target.WorkersPerReplicaMaximum = *source.WorkersPerReplicaMaximum
	}
	if source.TargetUtilization != nil {
		target.TargetUtilization = *source.TargetUtilization
	}
}

func validateEffective(value EffectiveConfiguration) error {
	if value.Execution.Mode != ExecutionModeStateless && value.Execution.Mode != ExecutionModePersistent {
		return errors.New("execution.mode must be stateless or persistent")
	}
	if value.Execution.ConcurrencyPerWorker < 1 || value.Execution.KeepAlive <= 0 {
		return errors.New("execution requires concurrency_per_worker >= 1 and a positive keep_alive")
	}
	if value.Scaling.ReplicasMinimum < 0 || value.Scaling.ReplicasMinimum > value.Scaling.ReplicasMaximum || value.Scaling.ReplicasMaximum < 1 {
		return errors.New("scaling replicas must satisfy 0 <= replicas_min <= replicas_max and replicas_max >= 1")
	}
	if value.Scaling.WorkersPerReplicaMinimum < 0 || value.Scaling.WorkersPerReplicaMinimum > value.Scaling.WorkersPerReplicaMaximum || value.Scaling.WorkersPerReplicaMaximum < 1 {
		return errors.New("scaling workers must satisfy 0 <= workers_per_replica_min <= workers_per_replica_max and workers_per_replica_max >= 1")
	}
	if value.Scaling.TargetUtilization <= 0 || value.Scaling.TargetUtilization > 1 {
		return errors.New("scaling.target_utilization must be greater than 0 and at most 1")
	}
	if value.Timeouts.Request <= 0 || value.Timeouts.Drain <= 0 || value.Timeouts.Idle < 0 {
		return errors.New("request and drain timeouts must be positive and idle timeout cannot be negative")
	}
	if value.DependencyMode != "cached-only" && value.DependencyMode != "online" {
		return errors.New("dependency_mode must be cached-only or online")
	}
	if value.Placement.SandboxGroup != strings.TrimSpace(value.Placement.SandboxGroup) || strings.ContainsRune(value.Placement.SandboxGroup, '\x00') {
		return errors.New("placement.sandbox_group must be trimmed and cannot contain a null byte")
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
