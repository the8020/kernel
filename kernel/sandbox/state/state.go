// Package state persists sandbox desired and observed state atomically.
package state

import (
	"bytes"
	"container/heap"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"the8020/kernel/sandbox/model"
)

type Store struct {
	mu               sync.RWMutex
	root             string
	records          map[string]cachedRecord
	ids              map[string]bool
	sandboxToRuntime map[string]string
	heartbeats       heartbeatQueue
	heartbeatItems   map[string]*heartbeatItem
	locks            [64]sync.Mutex
}

type heartbeatItem struct {
	runtimeGroupID string
	observedAt     time.Time
	index          int
}

type heartbeatQueue []*heartbeatItem

func (q heartbeatQueue) Len() int { return len(q) }
func (q heartbeatQueue) Less(i, j int) bool {
	if q[i].observedAt.Equal(q[j].observedAt) {
		return q[i].runtimeGroupID < q[j].runtimeGroupID
	}
	return q[i].observedAt.Before(q[j].observedAt)
}
func (q heartbeatQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index, q[j].index = i, j
}
func (q *heartbeatQueue) Push(value any) {
	item := value.(*heartbeatItem)
	item.index = len(*q)
	*q = append(*q, item)
}
func (q *heartbeatQueue) Pop() any {
	prior := *q
	last := len(prior) - 1
	item := prior[last]
	prior[last] = nil
	item.index = -1
	*q = prior[:last]
	return item
}

type cachedRecord struct {
	spec     model.SandboxSpec
	status   model.SandboxStatus
	snapshot model.RuntimeSnapshot
	complete bool
}

func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("runtime-group state root is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create runtime-group state: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("restrict runtime-group state: %w", err)
	}
	store := &Store{root: root, records: map[string]cachedRecord{}, ids: map[string]bool{}, sandboxToRuntime: map[string]string{}, heartbeatItems: map[string]*heartbeatItem{}}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		store.ids[entry.Name()] = true
		if record, loadErr := store.readRecord(entry.Name()); loadErr == nil {
			store.records[entry.Name()] = record
			store.sandboxToRuntime[record.spec.SandboxID] = entry.Name()
			store.updateHeartbeatLocked(entry.Name(), record.status)
		}
	}
	return store, nil
}

func (s *Store) SaveSpec(spec model.SandboxSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	lock := s.recordLock(spec.RuntimeGroupID)
	lock.Lock()
	defer lock.Unlock()
	if err := s.write(spec.RuntimeGroupID, "spec.json", spec); err != nil {
		return err
	}
	if spec.InternalToken != "" {
		if err := s.write(spec.RuntimeGroupID, "secret.json", map[string]string{"internal_token": spec.InternalToken}); err != nil {
			return err
		}
	}
	s.mu.Lock()
	record := s.records[spec.RuntimeGroupID]
	if record.spec.SandboxID != "" && record.spec.SandboxID != spec.SandboxID {
		delete(s.sandboxToRuntime, record.spec.SandboxID)
	}
	record.spec = cloneSpec(spec)
	record.complete = record.status.DesiredState.Valid() && record.status.ObservedState.Valid()
	s.records[spec.RuntimeGroupID] = record
	s.ids[spec.RuntimeGroupID] = true
	s.sandboxToRuntime[spec.SandboxID] = spec.RuntimeGroupID
	s.updateHeartbeatLocked(spec.RuntimeGroupID, record.status)
	s.mu.Unlock()
	return nil
}

func (s *Store) SaveStatus(runtimeGroupID string, status model.SandboxStatus) error {
	if runtimeGroupID == "" || !status.DesiredState.Valid() || !status.ObservedState.Valid() {
		return errors.New("runtime-group ID and valid desired/observed states are required")
	}
	lock := s.recordLock(runtimeGroupID)
	lock.Lock()
	defer lock.Unlock()
	if err := s.write(runtimeGroupID, "state.json", status); err != nil {
		return err
	}
	s.mu.Lock()
	record := s.records[runtimeGroupID]
	record.status = cloneStatus(status)
	record.complete = record.spec.RuntimeGroupID == runtimeGroupID
	s.records[runtimeGroupID] = record
	s.ids[runtimeGroupID] = true
	s.updateHeartbeatLocked(runtimeGroupID, status)
	s.mu.Unlock()
	return nil
}

// UpdateStatus applies a read-modify-write while holding the store lock. It is
// used when independent callback and monitor paths update different fields.
func (s *Store) UpdateStatus(runtimeGroupID string, update func(*model.SandboxStatus) error) (model.SandboxStatus, error) {
	lock := s.recordLock(runtimeGroupID)
	lock.Lock()
	defer lock.Unlock()
	record, err := s.loadRecord(runtimeGroupID)
	if err != nil {
		return model.SandboxStatus{}, err
	}
	status := cloneStatus(record.status)
	if update != nil {
		if err := update(&status); err != nil {
			return status, err
		}
	}
	if !status.DesiredState.Valid() || !status.ObservedState.Valid() {
		return status, errors.New("valid desired and observed states are required")
	}
	if err := s.write(runtimeGroupID, "state.json", status); err != nil {
		return model.SandboxStatus{}, err
	}
	record.status = cloneStatus(status)
	s.putRecord(runtimeGroupID, record)
	return cloneStatus(status), nil
}

func (s *Store) Transition(runtimeGroupID string, observed model.SandboxState, update func(*model.SandboxStatus)) (model.SandboxStatus, error) {
	lock := s.recordLock(runtimeGroupID)
	lock.Lock()
	defer lock.Unlock()
	record, err := s.loadRecord(runtimeGroupID)
	if err != nil {
		return model.SandboxStatus{}, err
	}
	status := cloneStatus(record.status)
	if !model.ValidTransition(status.ObservedState, observed) {
		return status, fmt.Errorf("invalid sandbox transition %s -> %s", status.ObservedState, observed)
	}
	status.ObservedState = observed
	if update != nil {
		update(&status)
	}
	if err := s.write(runtimeGroupID, "state.json", status); err != nil {
		return model.SandboxStatus{}, err
	}
	record.status = cloneStatus(status)
	s.putRecord(runtimeGroupID, record)
	return cloneStatus(status), nil
}

// TransitionIf atomically re-checks a lifecycle predicate before publishing a
// transition. A false predicate leaves state unchanged.
func (s *Store) TransitionIf(runtimeGroupID string, observed model.SandboxState, condition func(model.SandboxStatus) bool, update func(*model.SandboxStatus)) (model.SandboxStatus, bool, error) {
	lock := s.recordLock(runtimeGroupID)
	lock.Lock()
	defer lock.Unlock()
	record, err := s.loadRecord(runtimeGroupID)
	if err != nil {
		return model.SandboxStatus{}, false, err
	}
	status := cloneStatus(record.status)
	if condition != nil && !condition(status) {
		return status, false, nil
	}
	if !model.ValidTransition(status.ObservedState, observed) {
		return status, false, fmt.Errorf("invalid sandbox transition %s -> %s", status.ObservedState, observed)
	}
	status.ObservedState = observed
	if update != nil {
		update(&status)
	}
	if err := s.write(runtimeGroupID, "state.json", status); err != nil {
		return model.SandboxStatus{}, false, err
	}
	record.status = cloneStatus(status)
	s.putRecord(runtimeGroupID, record)
	return cloneStatus(status), true, nil
}

func (s *Store) Load(runtimeGroupID string) (model.SandboxSpec, model.SandboxStatus, error) {
	lock := s.recordLock(runtimeGroupID)
	lock.Lock()
	defer lock.Unlock()
	record, err := s.loadRecord(runtimeGroupID)
	if err != nil {
		return model.SandboxSpec{}, model.SandboxStatus{}, err
	}
	return cloneSpec(record.spec), cloneStatus(record.status), nil
}

// Cached returns one completely preloaded runtime-group record without any
// recovery filesystem fallback. Authentication and routing hot paths use this
// method so unknown identities are cache misses rather than disk probes.
func (s *Store) Cached(runtimeGroupID string) (model.SandboxSpec, model.SandboxStatus, bool) {
	s.mu.RLock()
	record, ok := s.records[runtimeGroupID]
	if ok && record.complete {
		spec, status := cloneSpec(record.spec), cloneStatus(record.status)
		s.mu.RUnlock()
		return spec, status, true
	}
	s.mu.RUnlock()
	return model.SandboxSpec{}, model.SandboxStatus{}, false
}

// Contains reports whether an identity is already indexed as either a runtime
// group or sandbox. It never probes recovery files.
func (s *Store) Contains(identity string) bool {
	s.mu.RLock()
	_, runtimeGroupExists := s.ids[identity]
	_, sandboxExists := s.sandboxToRuntime[identity]
	s.mu.RUnlock()
	return runtimeGroupExists || sandboxExists
}

func (s *Store) List() ([]string, error) {
	s.mu.RLock()
	result := make([]string, 0, len(s.ids))
	for id := range s.ids {
		result = append(result, id)
	}
	s.mu.RUnlock()
	sort.Strings(result)
	return result, nil
}

// Resolve returns one cached record by either sandbox or runtime-group ID.
// The secondary index avoids an O(n) scan on routing and administration paths.
func (s *Store) Resolve(identity string) (model.SandboxSpec, model.SandboxStatus, error) {
	s.mu.RLock()
	runtimeGroupID := identity
	if mapped := s.sandboxToRuntime[identity]; mapped != "" {
		runtimeGroupID = mapped
	}
	s.mu.RUnlock()
	return s.Load(runtimeGroupID)
}

func (s *Store) Delete(runtimeGroupID string) error {
	lock := s.recordLock(runtimeGroupID)
	lock.Lock()
	defer lock.Unlock()
	directory := filepath.Join(s.root, runtimeGroupID)
	for _, name := range []string{"spec.json", "state.json", "secret.json"} {
		if err := os.Remove(filepath.Join(directory, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.mu.Lock()
	if record, ok := s.records[runtimeGroupID]; ok {
		delete(s.sandboxToRuntime, record.spec.SandboxID)
	}
	s.removeHeartbeatLocked(runtimeGroupID)
	delete(s.records, runtimeGroupID)
	delete(s.ids, runtimeGroupID)
	s.mu.Unlock()
	return nil
}

// Observe records one authenticated absolute supervisor snapshot in memory.
// Equal-epoch revisions refresh heartbeat freshness; older supervisor epochs
// cannot keep a replacement supervisor healthy or roll state backwards. No
// durable file is touched on this hot path.
func (s *Store) Observe(runtimeGroupID string, snapshot model.RuntimeSnapshot, observedAt time.Time) (bool, error) {
	if runtimeGroupID == "" || snapshot.Revision == 0 || snapshot.RuntimeGroupID != runtimeGroupID {
		return false, errors.New("invalid runtime snapshot identity or revision")
	}
	lock := s.recordLock(runtimeGroupID)
	lock.Lock()
	defer lock.Unlock()
	record, err := s.loadRecord(runtimeGroupID)
	if err != nil {
		return false, err
	}
	if snapshot.SandboxID != record.spec.SandboxID || snapshot.WorkloadType != record.spec.WorkloadType {
		return false, errors.New("runtime snapshot does not match sandbox specification")
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	snapshot.ObservedAt = observedAt
	current := record.snapshot
	applied := snapshot.SupervisorStartedAtMS > current.SupervisorStartedAtMS ||
		(snapshot.SupervisorStartedAtMS == current.SupervisorStartedAtMS && snapshot.Revision > current.Revision)
	if applied {
		record.snapshot = cloneSnapshot(snapshot)
		record.status.SupervisorHealthy = true
		record.status.SupervisorVersion = snapshot.SupervisorVersion
		record.status.DenoVersion = snapshot.DenoVersion
		record.status.WorkerCount = snapshot.WorkerCount
	}
	if snapshot.SupervisorStartedAtMS >= current.SupervisorStartedAtMS && observedAt.After(record.status.LastHeartbeat) {
		record.status.LastHeartbeat = observedAt
	}
	s.putRecord(runtimeGroupID, record)
	return applied, nil
}

func (s *Store) Snapshot(runtimeGroupID string) (model.RuntimeSnapshot, bool) {
	s.mu.RLock()
	record, ok := s.records[runtimeGroupID]
	s.mu.RUnlock()
	if !ok || record.snapshot.Revision == 0 {
		return model.RuntimeSnapshot{}, false
	}
	return cloneSnapshot(record.snapshot), true
}

// ClaimStaleHeartbeats removes at most limit runtime groups whose cached
// heartbeat is no newer than cutoff. Callers inspect only these candidates and
// then call RescheduleHeartbeat; a concurrent heartbeat inserts its own newer
// deadline immediately.
func (s *Store) ClaimStaleHeartbeats(cutoff time.Time, limit int) []string {
	if limit < 1 {
		return nil
	}
	s.mu.Lock()
	result := make([]string, 0, min(limit, len(s.heartbeats)))
	for len(s.heartbeats) > 0 && len(result) < limit {
		item := s.heartbeats[0]
		if item.observedAt.After(cutoff) {
			break
		}
		heap.Pop(&s.heartbeats)
		delete(s.heartbeatItems, item.runtimeGroupID)
		result = append(result, item.runtimeGroupID)
	}
	s.mu.Unlock()
	return result
}

// RescheduleHeartbeat restores one claimed runtime group from its newest
// cached status. Terminal or concurrently deleted groups remain absent.
func (s *Store) RescheduleHeartbeat(runtimeGroupID string) {
	s.mu.Lock()
	if record, ok := s.records[runtimeGroupID]; ok && record.complete {
		s.updateHeartbeatLocked(runtimeGroupID, record.status)
	}
	s.mu.Unlock()
}

// ObserveMetrics refreshes non-durable diagnostic metrics for one sandbox.
func (s *Store) ObserveMetrics(runtimeGroupID string, metrics model.ResourceMetrics) error {
	lock := s.recordLock(runtimeGroupID)
	lock.Lock()
	defer lock.Unlock()
	record, err := s.loadRecord(runtimeGroupID)
	if err != nil {
		return err
	}
	record.status.Metrics = metrics
	s.putRecord(runtimeGroupID, record)
	return nil
}

func (s *Store) recordLock(runtimeGroupID string) *sync.Mutex {
	var hash uint64 = 1469598103934665603
	for index := 0; index < len(runtimeGroupID); index++ {
		hash ^= uint64(runtimeGroupID[index])
		hash *= 1099511628211
	}
	return &s.locks[hash%uint64(len(s.locks))]
}

func (s *Store) loadRecord(runtimeGroupID string) (cachedRecord, error) {
	s.mu.RLock()
	record, ok := s.records[runtimeGroupID]
	s.mu.RUnlock()
	if ok && record.complete {
		return record, nil
	}
	record, err := s.readRecord(runtimeGroupID)
	if err != nil {
		return cachedRecord{}, err
	}
	s.putRecord(runtimeGroupID, record)
	return record, nil
}

func (s *Store) readRecord(runtimeGroupID string) (cachedRecord, error) {
	var record cachedRecord
	if err := s.read(runtimeGroupID, "spec.json", &record.spec); err != nil {
		return record, err
	}
	if err := s.read(runtimeGroupID, "state.json", &record.status); err != nil {
		return record, err
	}
	if record.spec.RuntimeGroupID != runtimeGroupID {
		return record, errors.New("runtime-group state identity mismatch")
	}
	var secret struct {
		InternalToken string `json:"internal_token"`
	}
	if err := s.read(runtimeGroupID, "secret.json", &secret); err == nil {
		record.spec.InternalToken = secret.InternalToken
	} else if !errors.Is(err, os.ErrNotExist) {
		return record, err
	}
	record.complete = true
	return record, nil
}

func (s *Store) putRecord(runtimeGroupID string, record cachedRecord) {
	s.mu.Lock()
	if prior, ok := s.records[runtimeGroupID]; ok && prior.spec.SandboxID != "" && prior.spec.SandboxID != record.spec.SandboxID {
		delete(s.sandboxToRuntime, prior.spec.SandboxID)
	}
	s.records[runtimeGroupID] = record
	s.ids[runtimeGroupID] = true
	if record.spec.SandboxID != "" {
		s.sandboxToRuntime[record.spec.SandboxID] = runtimeGroupID
	}
	s.updateHeartbeatLocked(runtimeGroupID, record.status)
	s.mu.Unlock()
}

func (s *Store) updateHeartbeatLocked(runtimeGroupID string, status model.SandboxStatus) {
	record, complete := s.records[runtimeGroupID]
	monitor := complete && record.complete && (status.ObservedState == model.StateReady || status.ObservedState == model.StateActive || status.ObservedState == model.StateDraining)
	item, exists := s.heartbeatItems[runtimeGroupID]
	if !monitor {
		if exists {
			heap.Remove(&s.heartbeats, item.index)
			delete(s.heartbeatItems, runtimeGroupID)
		}
		return
	}
	if exists {
		item.observedAt = status.LastHeartbeat
		heap.Fix(&s.heartbeats, item.index)
		return
	}
	item = &heartbeatItem{runtimeGroupID: runtimeGroupID, observedAt: status.LastHeartbeat}
	s.heartbeatItems[runtimeGroupID] = item
	heap.Push(&s.heartbeats, item)
}

func (s *Store) removeHeartbeatLocked(runtimeGroupID string) {
	if item, ok := s.heartbeatItems[runtimeGroupID]; ok {
		heap.Remove(&s.heartbeats, item.index)
		delete(s.heartbeatItems, runtimeGroupID)
	}
}

func (s *Store) write(runtimeGroupID, name string, value any) error {
	if runtimeGroupID == "" || filepath.Base(runtimeGroupID) != runtimeGroupID {
		return errors.New("invalid runtime-group ID")
	}
	directory := filepath.Join(s.root, runtimeGroupID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".state-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, filepath.Join(directory, name))
}

func (s *Store) read(runtimeGroupID, name string, output any) error {
	if runtimeGroupID == "" || filepath.Base(runtimeGroupID) != runtimeGroupID {
		return errors.New("invalid runtime-group ID")
	}
	data, err := os.ReadFile(filepath.Join(s.root, runtimeGroupID, name))
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode %s for %s: %w", name, runtimeGroupID, err)
	}
	return nil
}

func cloneSpec(value model.SandboxSpec) model.SandboxSpec {
	value.OwnerIDs = append([]string(nil), value.OwnerIDs...)
	value.ServiceIDs = append([]string(nil), value.ServiceIDs...)
	value.InternalPorts = append([]int(nil), value.InternalPorts...)
	value.Mounts = append([]model.Mount(nil), value.Mounts...)
	value.Network.AllowedHosts = append([]string(nil), value.Network.AllowedHosts...)
	value.Permissions = clonePermissions(value.Permissions)
	value.RuntimeProfile.Permissions = clonePermissions(value.RuntimeProfile.Permissions)
	value.RuntimeProfile.Mounts = append([]model.Mount(nil), value.RuntimeProfile.Mounts...)
	value.RuntimeProfile.DenoStartupFlags = append([]string(nil), value.RuntimeProfile.DenoStartupFlags...)
	if value.Labels != nil {
		labels := make(map[string]string, len(value.Labels))
		for key, item := range value.Labels {
			labels[key] = item
		}
		value.Labels = labels
	}
	return value
}

func clonePermissions(value model.Permissions) model.Permissions {
	value.ReadPaths = append([]string(nil), value.ReadPaths...)
	value.WritePaths = append([]string(nil), value.WritePaths...)
	value.NetworkHosts = append([]string(nil), value.NetworkHosts...)
	value.ImportHosts = append([]string(nil), value.ImportHosts...)
	value.Environment = append([]string(nil), value.Environment...)
	return value
}

func cloneStatus(value model.SandboxStatus) model.SandboxStatus {
	value.CurrentOwners = append([]string(nil), value.CurrentOwners...)
	value.ExposedPorts = append([]model.PortStatus(nil), value.ExposedPorts...)
	if value.DebugLease != nil {
		lease := *value.DebugLease
		value.DebugLease = &lease
	}
	value.Metrics.MemoryEvents = cloneCounterMap(value.Metrics.MemoryEvents)
	value.Metrics.PIDEvents = cloneCounterMap(value.Metrics.PIDEvents)
	value.Metrics.CPUStat = cloneCounterMap(value.Metrics.CPUStat)
	value.Metrics.CgroupEvents = cloneCounterMap(value.Metrics.CgroupEvents)
	return value
}

func cloneCounterMap(value map[string]uint64) map[string]uint64 {
	if value == nil {
		return nil
	}
	result := make(map[string]uint64, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func cloneSnapshot(value model.RuntimeSnapshot) model.RuntimeSnapshot {
	value.RecentFailures = append([]model.RuntimeFailure(nil), value.RecentFailures...)
	value.Workers = append([]model.RuntimeWorkerStatus(nil), value.Workers...)
	return value
}
