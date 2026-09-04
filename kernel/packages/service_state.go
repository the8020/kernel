package packages

import "context"

type UnlockFunc func() error

type StoredServiceState struct {
	ServiceID string              `json:"service_id"`
	State     DesiredServiceState `json:"state"`
}

// ServiceChange is the latest published desired-state change for one logical
// service. Active is false for a retired or deleted service.
type ServiceChange struct {
	ServiceID string
	Active    bool
}

// ServiceStateStore persists service enablement and explicit operator
// overrides. Callers hold Lock across read-modify-write operations.
type ServiceStateStore interface {
	Get(serviceID string) (DesiredServiceState, bool, error)
	Put(serviceID string, state DesiredServiceState) error
	Delete(serviceID string) error
	List() ([]StoredServiceState, error)
	Lock(ctx context.Context, serviceID string) (UnlockFunc, error)
}

// ServiceDefinitionStore atomically creates the package declaration and first
// immutable effective-policy version before desired state becomes visible.
type ServiceDefinitionStore interface {
	InstallDefinition(context.Context, Definition, DesiredServiceState, EffectiveConfiguration, string) error
	RetirePackage(context.Context, string, []string) error
}

// ServiceDesiredStateStore publishes an operator mutation and its immutable
// policy version together with the shared revision that wakes other nodes.
type ServiceDesiredStateStore interface {
	UpdateDesiredDefinition(context.Context, Definition, DesiredServiceState, EffectiveConfiguration, string) error
}

// ServiceRevisionStore supplies the constant-cost detector and targeted
// change set used by each node's shared-state monitor.
type ServiceRevisionStore interface {
	ServiceRevision(context.Context) (uint64, error)
	ServiceChanges(context.Context, uint64, uint64) ([]ServiceChange, error)
}
