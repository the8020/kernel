// Package model defines side-effect-free sandbox and runtime-group contracts.
package model

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

type WorkloadType string

const (
	WorkloadService WorkloadType = "service"
	WorkloadJob     WorkloadType = "job"
)

func (w WorkloadType) Valid() bool {
	return w == WorkloadService || w == WorkloadJob
}

type DependencyMode string

const (
	DependencyCachedOnly DependencyMode = "cached_only"
	DependencyOnline     DependencyMode = "online"
)

func (d DependencyMode) Valid() bool { return d == DependencyCachedOnly || d == DependencyOnline }

type SandboxState string

const (
	StateCreating SandboxState = "CREATING"
	StateStarting SandboxState = "STARTING"
	StateReady    SandboxState = "READY"
	StateActive   SandboxState = "ACTIVE"
	StateDraining SandboxState = "DRAINING"
	StateStopping SandboxState = "STOPPING"
	StateStopped  SandboxState = "STOPPED"
	StateFailed   SandboxState = "FAILED"
	StateDeleting SandboxState = "DELETING"
)

var transitions = map[SandboxState]map[SandboxState]bool{
	StateCreating: {StateStarting: true, StateFailed: true, StateDeleting: true},
	StateStarting: {StateReady: true, StateStopping: true, StateFailed: true},
	StateReady:    {StateActive: true, StateDraining: true, StateStopping: true, StateFailed: true},
	StateActive:   {StateReady: true, StateDraining: true, StateStopping: true, StateFailed: true},
	StateDraining: {StateStopping: true, StateFailed: true},
	StateStopping: {StateStopped: true, StateFailed: true},
	StateStopped:  {StateDeleting: true},
	StateFailed:   {StateStopping: true, StateDeleting: true},
	StateDeleting: {},
}

func (s SandboxState) Valid() bool { _, ok := transitions[s]; return ok }

func ValidTransition(from, to SandboxState) bool {
	if from == to && from.Valid() {
		return true
	}
	return transitions[from][to]
}

type GroupingStrategy string

const (
	GroupingIsolated  GroupingStrategy = "isolated"
	GroupingOwner     GroupingStrategy = "owner"
	GroupingNamespace GroupingStrategy = "namespace"
	GroupingShared    GroupingStrategy = "shared"
)

func (s GroupingStrategy) Valid() bool {
	return s == GroupingIsolated || s == GroupingOwner || s == GroupingNamespace || s == GroupingShared
}

type Permissions struct {
	ReadPaths    []string `json:"read_paths,omitempty"`
	WritePaths   []string `json:"write_paths,omitempty"`
	NetworkHosts []string `json:"network_hosts,omitempty"`
	ImportHosts  []string `json:"import_hosts,omitempty"`
	Environment  []string `json:"environment,omitempty"`
	SystemInfo   bool     `json:"system_info"`
}

func (p Permissions) EgressHosts() []string {
	return sortedUnique(append(append([]string(nil), p.NetworkHosts...), p.ImportHosts...))
}

type Mount struct {
	Source      string `json:"source"`
	Target      string `json:"target"`
	ReadOnly    bool   `json:"read_only"`
	MaximumSize int64  `json:"maximum_size,omitempty"`
	OwnerScope  string `json:"owner_scope,omitempty"`
	Purpose     string `json:"purpose"`
	Persistence string `json:"persistence"`
}

type ResourceLimits struct {
	PIDMaximum   int64 `json:"pid_maximum"`
	TmpfsMaximum int64 `json:"tmpfs_maximum"`
}

func (r ResourceLimits) Validate() error {
	if r.PIDMaximum <= 0 || r.TmpfsMaximum <= 0 {
		return errors.New("PID and tmpfs maximums must be positive")
	}
	return nil
}

type NetworkConfiguration struct {
	Mode           string   `json:"mode"`
	NamespacePath  string   `json:"namespace_path,omitempty"`
	NetworkName    string   `json:"network_name"`
	SandboxIP      string   `json:"sandbox_ip,omitempty"`
	SupervisorPort int      `json:"supervisor_port,omitempty"`
	InspectorPort  int      `json:"inspector_port,omitempty"`
	EgressEnabled  bool     `json:"egress_enabled"`
	AllowedHosts   []string `json:"allowed_hosts,omitempty"`
}

const (
	DefaultSupervisorPort = 8000
	DefaultInspectorPort  = 9229
)

func (n NetworkConfiguration) SupervisorEndpointPort() int {
	if n.SupervisorPort != 0 {
		return n.SupervisorPort
	}
	return DefaultSupervisorPort
}

func (n NetworkConfiguration) InspectorEndpointPort() int {
	if n.InspectorPort != 0 {
		return n.InspectorPort
	}
	return DefaultInspectorPort
}

type RuntimeProfile struct {
	WorkloadType     WorkloadType   `json:"workload_type"`
	ImageDigest      string         `json:"image_digest"`
	DependencyMode   DependencyMode `json:"dependency_mode"`
	Permissions      Permissions    `json:"permissions"`
	Mounts           []Mount        `json:"mounts,omitempty"`
	NetworkMode      string         `json:"network_mode"`
	EgressAllowed    bool           `json:"egress_allowed"`
	DenoStartupFlags []string       `json:"deno_startup_flags,omitempty"`
	ResourceClass    string         `json:"resource_class"`
}

func (p RuntimeProfile) Hash() (string, error) {
	if !p.WorkloadType.Valid() || !p.DependencyMode.Valid() || !validDigest(p.ImageDigest) || p.NetworkMode == "" || p.ResourceClass == "" {
		return "", errors.New("runtime profile is incomplete")
	}
	if !p.EgressAllowed && len(p.Permissions.EgressHosts()) > 0 {
		return "", errors.New("runtime profile grants egress while egress policy is disabled")
	}
	for _, option := range p.DenoStartupFlags {
		if unsafeDenoStartupOption(option) {
			return "", fmt.Errorf("unsafe Deno startup option %q", option)
		}
	}
	canonical := p
	canonical.Permissions.ReadPaths = sortedUnique(p.Permissions.ReadPaths)
	canonical.Permissions.WritePaths = sortedUnique(p.Permissions.WritePaths)
	canonical.Permissions.NetworkHosts = sortedUnique(p.Permissions.NetworkHosts)
	canonical.Permissions.ImportHosts = sortedUnique(p.Permissions.ImportHosts)
	canonical.Permissions.Environment = sortedUnique(p.Permissions.Environment)
	canonical.DenoStartupFlags = sortedUnique(p.DenoStartupFlags)
	canonical.Mounts = append([]Mount(nil), p.Mounts...)
	sort.Slice(canonical.Mounts, func(i, j int) bool {
		left, right := canonical.Mounts[i], canonical.Mounts[j]
		return left.Target+"\x00"+left.Source+"\x00"+left.OwnerScope < right.Target+"\x00"+right.Source+"\x00"+right.OwnerScope
	})
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func unsafeDenoStartupOption(option string) bool {
	if option == "" || option == "-A" || option == "--allow-all" {
		return true
	}
	for _, prefix := range []string{"--allow-run", "--allow-ffi", "--allow-read", "--allow-write", "--allow-net", "--allow-env", "--allow-sys"} {
		if option == prefix || strings.HasPrefix(option, prefix+"=") {
			return true
		}
	}
	return false
}

type LifecyclePolicy struct {
	Warm            bool          `json:"warm"`
	DestroyWhenIdle bool          `json:"destroy_when_idle"`
	IdleTimeout     time.Duration `json:"idle_timeout"`
	StopGracePeriod time.Duration `json:"stop_grace_period"`
}

type SandboxSpec struct {
	SandboxID      string               `json:"sandbox_id"`
	RuntimeGroupID string               `json:"runtime_group_id"`
	WorkloadType   WorkloadType         `json:"workload_type"`
	GroupKey       string               `json:"group_key,omitempty"`
	PlacementGroup string               `json:"placement_group,omitempty"`
	OwnerIDs       []string             `json:"owner_ids,omitempty"`
	ServiceIDs     []string             `json:"service_ids,omitempty"`
	ImageDigest    string               `json:"image_digest"`
	RuntimeProfile RuntimeProfile       `json:"runtime_profile"`
	ProfileHash    string               `json:"profile_hash"`
	ResourceLimits ResourceLimits       `json:"resource_limits"`
	Network        NetworkConfiguration `json:"network"`
	InternalPorts  []int                `json:"internal_ports,omitempty"`
	Mounts         []Mount              `json:"mounts,omitempty"`
	Permissions    Permissions          `json:"permissions"`
	DependencyMode DependencyMode       `json:"dependency_mode"`
	DebugEnabled   bool                 `json:"debug_enabled"`
	Lifecycle      LifecyclePolicy      `json:"lifecycle"`
	Labels         map[string]string    `json:"labels,omitempty"`
	InternalToken  string               `json:"-"`
}

func (s SandboxSpec) Validate() error {
	if s.SandboxID == "" || s.RuntimeGroupID == "" {
		return errors.New("sandbox ID and runtime-group ID are required")
	}
	if !s.WorkloadType.Valid() {
		return fmt.Errorf("invalid workload type %q", s.WorkloadType)
	}
	if !s.DependencyMode.Valid() {
		return fmt.Errorf("invalid dependency mode %q", s.DependencyMode)
	}
	if !validDigest(s.ImageDigest) {
		return errors.New("image digest must be an immutable sha256 digest")
	}
	if s.RuntimeProfile.WorkloadType != s.WorkloadType || s.RuntimeProfile.ImageDigest != s.ImageDigest || s.RuntimeProfile.DependencyMode != s.DependencyMode {
		return errors.New("runtime profile identity does not match sandbox specification")
	}
	if !reflect.DeepEqual(s.Mounts, s.RuntimeProfile.Mounts) || !reflect.DeepEqual(s.Permissions, s.RuntimeProfile.Permissions) {
		return errors.New("sandbox mounts and permissions must match the immutable runtime profile")
	}
	hash, err := s.RuntimeProfile.Hash()
	if err != nil {
		return fmt.Errorf("runtime profile: %w", err)
	}
	if s.ProfileHash != hash {
		return fmt.Errorf("runtime profile hash mismatch: have %q want %q", s.ProfileHash, hash)
	}
	if err := s.ResourceLimits.Validate(); err != nil {
		return fmt.Errorf("resource limits: %w", err)
	}
	if s.Network.Mode != "netstack" {
		return errors.New("sandbox network mode must be netstack")
	}
	if !s.Lifecycle.Warm && (s.GroupKey == "" || len(s.OwnerIDs) == 0) {
		return errors.New("assigned sandbox requires a group key and at least one owner")
	}
	if s.Lifecycle.Warm && (s.GroupKey != "" || len(s.OwnerIDs) != 0) {
		return errors.New("warm sandbox cannot have a group key or owner")
	}
	owners := map[string]bool{}
	for _, owner := range s.OwnerIDs {
		if strings.TrimSpace(owner) == "" || owners[owner] {
			return errors.New("owner IDs must be non-empty and unique")
		}
		owners[owner] = true
	}
	if s.WorkloadType != WorkloadService && len(s.ServiceIDs) > 0 {
		return errors.New("service IDs are valid only for service sandboxes")
	}
	seenServices := map[string]bool{}
	for _, serviceID := range s.ServiceIDs {
		if strings.TrimSpace(serviceID) == "" || seenServices[serviceID] {
			return errors.New("service sandbox IDs must be non-empty and unique")
		}
		seenServices[serviceID] = true
	}
	ports := map[int]bool{}
	for _, port := range s.InternalPorts {
		if port < 1 || port > 65535 || ports[port] {
			return fmt.Errorf("invalid or duplicate internal port %d", port)
		}
		ports[port] = true
	}
	if s.Network.SandboxIP != "" && net.ParseIP(s.Network.SandboxIP) == nil {
		return errors.New("sandbox IP is invalid")
	}
	for name, port := range map[string]int{"supervisor": s.Network.SupervisorPort, "inspector": s.Network.InspectorPort} {
		if port < 0 || port > 65535 {
			return fmt.Errorf("sandbox %s port is invalid", name)
		}
	}
	if s.Network.SupervisorPort != 0 && s.Network.SupervisorPort == s.Network.InspectorPort {
		return errors.New("sandbox supervisor and inspector ports must differ")
	}
	for _, mount := range s.Mounts {
		if !filepath.IsAbs(mount.Target) || mount.Target != filepath.Clean(mount.Target) || mount.Purpose == "" || mount.Persistence == "" {
			return fmt.Errorf("invalid mount target or metadata for %q", mount.Target)
		}
	}
	return nil
}

type PortStatus struct {
	LeaseID      string    `json:"lease_id"`
	OwnerID      string    `json:"owner_id"`
	BindAddress  string    `json:"bind_address"`
	HostPort     int       `json:"host_port"`
	InternalPort int       `json:"internal_port"`
	Protocol     string    `json:"protocol"`
	Purpose      string    `json:"purpose"`
	State        string    `json:"state"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

type DebugLeaseStatus struct {
	LeaseID   string    `json:"lease_id"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ResourceMetrics struct {
	CPUUsageMicros int64             `json:"cpu_usage_micros"`
	MemoryCurrent  int64             `json:"memory_current"`
	MemoryPeak     int64             `json:"memory_peak"`
	PIDCurrent     int64             `json:"pid_current"`
	SampledAt      time.Time         `json:"sampled_at,omitempty"`
	MemoryEvents   map[string]uint64 `json:"memory_events,omitempty"`
	PIDEvents      map[string]uint64 `json:"pid_events,omitempty"`
	CPUStat        map[string]uint64 `json:"cpu_stat,omitempty"`
	CgroupEvents   map[string]uint64 `json:"cgroup_events,omitempty"`
}

type SandboxStatus struct {
	DesiredState      SandboxState      `json:"desired_state"`
	ObservedState     SandboxState      `json:"observed_state"`
	ContainerID       string            `json:"containerd_container_id,omitempty"`
	TaskPID           uint32            `json:"containerd_task_pid,omitempty"`
	SandboxIP         string            `json:"sandbox_ip,omitempty"`
	SupervisorHealthy bool              `json:"supervisor_health"`
	SupervisorVersion string            `json:"supervisor_version,omitempty"`
	DenoVersion       string            `json:"deno_version,omitempty"`
	CurrentOwners     []string          `json:"current_owners,omitempty"`
	WorkerCount       int               `json:"worker_count"`
	Metrics           ResourceMetrics   `json:"resources"`
	StartedAt         time.Time         `json:"start_time,omitempty"`
	LastHeartbeat     time.Time         `json:"last_heartbeat,omitempty"`
	FailureReason     string            `json:"failure_reason,omitempty"`
	ExposedPorts      []PortStatus      `json:"exposed_ports,omitempty"`
	DebugLease        *DebugLeaseStatus `json:"debug_lease,omitempty"`
}

func NewID(prefix string) (string, error) {
	if prefix == "" {
		return "", errors.New("ID prefix is required")
	}
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate ID: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(bytes[:]), nil
}

// NewSandboxID returns the compact public sandbox identifier.
func NewSandboxID() (string, error) {
	return newCompactID("sbx")
}

// IsSandboxID reports whether value is a public sandbox identifier.
func IsSandboxID(value string) bool {
	return validCompactID(value, "sbx")
}

// NewRuntimeGroupID returns the compact public runtime-group identifier.
func NewRuntimeGroupID() (string, error) {
	return newCompactID("rgp")
}

// NewWorkerID returns the compact public Worker identifier.
func NewWorkerID() (string, error) {
	return newCompactID("wrk")
}

// newCompactID uses rejection sampling so every lowercase alphanumeric
// character is uniformly distributed.
func newCompactID(prefix string) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	const randomLength = 8
	result := make([]byte, 0, len(prefix)+1+randomLength)
	result = append(result, prefix...)
	result = append(result, '-')
	buffer := make([]byte, randomLength*2)
	for len(result) < len(prefix)+1+randomLength {
		if _, err := rand.Read(buffer); err != nil {
			return "", fmt.Errorf("generate %s ID: %w", prefix, err)
		}
		for _, value := range buffer {
			// 252 is the largest multiple of 36 that fits in one byte.
			if value >= 252 {
				continue
			}
			result = append(result, alphabet[int(value)%len(alphabet)])
			if len(result) == len(prefix)+1+randomLength {
				break
			}
		}
	}
	return string(result), nil
}

func validCompactID(value, prefix string) bool {
	if len(value) != len(prefix)+1+8 || !strings.HasPrefix(value, prefix+"-") {
		return false
	}
	for _, character := range value[len(prefix)+1:] {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func sortedUnique(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	write := 0
	for _, value := range result {
		if write == 0 || value != result[write-1] {
			result[write] = value
			write++
		}
	}
	return result[:write]
}
