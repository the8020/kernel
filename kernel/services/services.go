// Package services defines the small typed dependency set used by command handlers.
package services

import (
	"context"
	"os"
	"sync"
	"time"

	"the8020/kernel/auth"
	"the8020/kernel/debugging"
	"the8020/kernel/development"
	"the8020/kernel/execution/adminrun"
	"the8020/kernel/execution/groups"
	"the8020/kernel/execution/jobs"
	executionservices "the8020/kernel/execution/services"
	"the8020/kernel/execution/workers"
	"the8020/kernel/instance"
	"the8020/kernel/lifecycle"
	"the8020/kernel/logging"
	"the8020/kernel/network"
	"the8020/kernel/nodes"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/ports"
	runtimehost "the8020/kernel/runtime"
	"the8020/kernel/sandbox/history"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/settings"
	"the8020/kernel/webservices"
)

// InstanceInfo is immutable process information exposed to system.status.
type InstanceInfo struct {
	UUID      string
	PID       int
	Paths     instance.Paths
	StartedAt time.Time
	BuildID   string
}

// Services is the complete typed dependency container for Phase 1 handlers.
type Services struct {
	Settings          *settings.Manager
	Network           *network.Manager
	Nodes             *nodes.Manager
	Logging           *logging.Manager
	Lifecycle         *lifecycle.Manager
	Auth              AuthService
	Layout            LayoutService
	Instance          InstanceInfo
	Runtime           *RuntimeServices
	Packages          PackageService
	PackageManagement PackageManagementService
	Development       DevelopmentService
	runtimeMu         sync.RWMutex
}

// LayoutService is the handler-facing bootstrap path configuration contract.
type LayoutService interface {
	Current() (instance.Layout, error)
	Set(instance.Layout) (instance.Layout, error)
}

// AuthService is the handler- and runtime-facing bootstrap authentication
// contract. It deliberately exposes summaries and opaque cookie headers only.
type AuthService interface {
	CookieName() string
	BootstrapLogin(context.Context, string, string, bool) (auth.BootstrapLoginResult, error)
	ValidateCookie(string) (auth.AuthContext, error)
	LogoutCurrent(auth.AuthContext, bool) (auth.LogoutResult, error)
	AddUser(context.Context, string, string) (auth.UserSummary, error)
	RemoveUser(context.Context, string) error
	EnableUser(context.Context, string) (auth.UserSummary, error)
	DisableUser(context.Context, string) (auth.UserSummary, error)
	SetPassword(context.Context, string, string) (auth.UserSummary, error)
	InvalidateUserSessions(context.Context, string) (auth.UserSummary, error)
	ListUsers() ([]auth.UserSummary, error)
	ListSessions() ([]auth.SessionSummary, error)
	RevokeSession(string) error
	RevokeUserSessions(string) (int, error)
	CleanupExpired() (int, error)
}

// SandboxService is the handler-facing sandbox lifecycle contract.
type SandboxService interface {
	List() ([]manager.Inspection, error)
	ListHistory(int, string) (history.Page, error)
	InspectHistory(string) (history.Inspection, error)
	Inspect(context.Context, string) (manager.Inspection, error)
	Metrics(string) (model.ResourceMetrics, error)
	Stop(context.Context, string) error
	Kill(context.Context, string) error
	Delete(context.Context, string) error
}

// WorkerService is the handler-facing Worker lifecycle contract.
type WorkerService interface {
	List(context.Context, string) ([]workers.Record, error)
	Inspect(context.Context, string) (workers.Record, error)
	Stop(context.Context, string, bool) error
}

// RuntimeServiceService is the handler-facing service-pool contract.
type RuntimeServiceService interface {
	Start(context.Context, string, string, executionservices.Options) (executionservices.Record, error)
	List() ([]executionservices.Record, error)
	Inspect(string) (executionservices.Record, error)
	Scale(context.Context, string, int) (executionservices.Record, error)
	Expose(context.Context, string, executionservices.ExposeOptions) (executionservices.Record, error)
	Unexpose(string) error
	Stop(context.Context, string) error
}

// PackageService is the handler-facing direct filesystem package contract.
type PackageService interface {
	ListPackages() ([]workspacepackages.Package, error)
	InspectPackage(string) (workspacepackages.Package, error)
}

// PackageManagementService is the generic desired-index and Git
// synchronization boundary. It contains no package-specific behavior.
type PackageManagementService interface {
	ListPackageIndexes() ([]workspacepackages.PackageIndex, error)
	InspectPackageIndex(string) (workspacepackages.PackageIndex, error)
	SetPackageIndex(context.Context, workspacepackages.PackageIndex) (workspacepackages.PackageIndex, error)
	InspectPackageSource(context.Context, string) (workspacepackages.SourceInspection, error)
	ListPackageVersions(context.Context, string, int) (workspacepackages.PackageVersions, error)
	SynchronizePackages(context.Context, []string) ([]workspacepackages.PackageSynchronization, error)
	CreateLocalPackage(context.Context, string, string, string) (workspacepackages.LocalPackage, error)
}

// DevelopmentService is the handler-facing durable developer workspace,
// activation, image, and independent package-repository contract.
type DevelopmentService interface {
	ImageStatus() (development.ImageStatus, error)
	Create(context.Context, string) (development.Workspace, error)
	List() ([]development.Workspace, error)
	Inspect(string) (development.Workspace, error)
	Start(context.Context, string) (development.Workspace, error)
	Stop(context.Context, string) (development.Workspace, error)
	Restart(context.Context, string) (development.Workspace, error)
	Kill(context.Context, string) (development.Workspace, error)
	Delete(context.Context, string, bool) error
	Shell(context.Context, string, string) (development.ShellResult, error)
	ResetSource(context.Context, string, bool) (development.Workspace, error)
	FactoryReset(context.Context, string, bool) (development.Workspace, error)
	Preview(context.Context, string, development.ActivationOptions) (development.ActivationPreview, error)
	Activate(context.Context, string, development.ActivationOptions) (development.ActivationResult, error)
	ListRepositories() ([]development.Repository, error)
	InspectRepository(string) (development.Repository, error)
	InitializeRepository(context.Context, string, string, string, string) (development.Repository, error)
	ConfigureRemote(context.Context, string, string, string) (development.Repository, error)
	RepositoryStatus(string) (development.Repository, error)
}

// WebServiceService is the handler-facing filesystem service lifecycle and
// canonical request contract.
type WebServiceService interface {
	Start(context.Context, string) (webservices.Status, error)
	Stop(context.Context, string) (webservices.Status, error)
	Restart(context.Context, string) (webservices.Status, error)
	Reload(context.Context, string) (webservices.Status, error)
	Retire(context.Context, string) error
	Scale(context.Context, string, webservices.ScaleOptions) (webservices.Status, error)
	List() ([]webservices.Status, error)
	Inspect(string) (webservices.Status, error)
	Validate(context.Context, string) webservices.ValidationResult
	Request(context.Context, string, string, string, webservices.RequestOptions) (webservices.RequestResult, error)
	OpenAPI(context.Context, string) (map[string]any, error)
}

// JobService is the handler-facing job execution contract.
type JobService interface {
	Run(context.Context, string, string, jobs.Options) (jobs.Record, error)
	List() ([]jobs.Record, error)
	Inspect(string) (jobs.Record, error)
	Cancel(context.Context, string) error
}

// PortService is the handler-facing host-port lease contract.
type PortService interface {
	Expose(context.Context, ports.Request) (ports.Lease, error)
	List() []ports.Lease
	Close(string) error
}

// DebugService is the handler-facing inspector lease contract.
type DebugService interface {
	Targets(context.Context, model.SandboxSpec) ([]debugging.Target, error)
	Open(context.Context, model.SandboxSpec, time.Duration) (debugging.Lease, error)
	Close(string) error
}

// PoolService is the handler-facing warm-pool contract.
type PoolService interface {
	Resize(string, int) error
	Status() []groups.PoolStatus
	Forget(string) error
}

// AdminRunService is the handler-facing administrative execution contract.
type AdminRunService interface {
	Eval(context.Context, string, adminrun.Options) (adminrun.Result, error)
	Run(context.Context, string, adminrun.Options) (adminrun.Result, error)
}

// RuntimeServices is the typed Phase 1B dependency set. Doctor remains
// available when host runtime initialization fails; lifecycle services are nil.
type RuntimeServices struct {
	Versions       runtimehost.Versions
	Doctor         *runtimehost.Doctor
	RootlessDoctor *runtimehost.RootlessDoctor
	Isolation      runtimehost.IsolationReport
	Failure        string
	Sandboxes      SandboxService
	Workers        WorkerService
	Services       WebServiceService
	ServicePools   RuntimeServiceService
	Jobs           JobService
	Ports          PortService
	Debugging      DebugService
	Pool           PoolService
	AdminRun       AdminRunService
}

// New constructs the dependency container without adding lookup behavior.
func New(settingManager *settings.Manager, networkManager *network.Manager, loggingManager *logging.Manager, lifecycleManager *lifecycle.Manager, authService AuthService, packageService PackageService, developmentService DevelopmentService, uuid string, paths instance.Paths, startedAt time.Time, buildID string, runtimeServices ...*RuntimeServices) *Services {
	var phase1B *RuntimeServices
	if len(runtimeServices) > 0 {
		phase1B = runtimeServices[0]
	}
	serviceSet := &Services{Settings: settingManager, Network: networkManager, Logging: loggingManager, Lifecycle: lifecycleManager, Auth: authService, Packages: packageService, Development: developmentService, Runtime: phase1B, Instance: InstanceInfo{UUID: uuid, PID: os.Getpid(), Paths: paths, StartedAt: startedAt, BuildID: buildID}}
	if management, ok := packageService.(PackageManagementService); ok {
		serviceSet.PackageManagement = management
	}
	return serviceSet
}

// RuntimeSnapshot returns the currently published runtime dependency set.
// Runtime initialization publishes a complete replacement atomically so command
// handlers never observe a partially composed runtime.
func (s *Services) RuntimeSnapshot() *RuntimeServices {
	if s == nil {
		return nil
	}
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.Runtime
}

// PublishRuntime atomically replaces the runtime dependency set exposed to
// command handlers after asynchronous runtime initialization completes.
func (s *Services) PublishRuntime(runtimeServices *RuntimeServices) {
	if s == nil {
		return
	}
	s.runtimeMu.Lock()
	s.Runtime = runtimeServices
	s.runtimeMu.Unlock()
}
