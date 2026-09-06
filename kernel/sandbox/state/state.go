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
	mu             sync.RWMutex
	root           string
	records        map[string]cachedRecord
	ids            map[string]bool
	heartbeats     heartbeatQueue
	heartbeatItems map[string]*heartbeatItem
	locks          [64]sync.Mutex
}

type heartbeatItem struct {
	sandboxID  string
	observedAt time.Time
	index      int
}

type heartbeatQueue []*heartbeatItem

func (q heartbeatQueue) Len() int { return len(q) }
func (q heartbeatQueue) Less(i, j int) bool {
	if q[i].observedAt.Equal(q[j].observedAt) {
		return q[i].sandboxID < q[j].sandboxID
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
		return nil, errors.New("sandbox state root is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create sandbox state: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("restrict sandbox state: %w", err)
	}
	store := &Store{root: root, records: map[string]cachedRecord{}, ids: map[string]bool{}, heartbeatItems: map[string]*heartbeatItem{}}
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
			store.updateHeartbeatLocked(entry.Name(), record.status)
		}
	}
	return store, nil
}

func (s *Store) SaveSpec(spec model.SandboxSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	lock := s.recordLock(spec.SandboxID)
	lock.Lock()
	defer lock.Unlock()
	if err := s.write(spec.SandboxID, "spec.json", spec); err != nil {
		return err
	}
	if spec.InternalToken != "" {
		if err := s.write(spec.SandboxID, "secret.json", map[string]string{"internal_token": spec.InternalToken}); err != nil {
			return err
		}
	}
	s.mu.Lock()
	record := s.records[spec.SandboxID]
	record.spec = cloneSpec(spec)
	record.complete = record.status.DesiredState.Valid() && record.status.ObservedState.Valid()
	s.records[spec.SandboxID] = record
	s.ids[spec.SandboxID] = true
	s.updateHeartbeatLocked(spec.SandboxID, record.status)
	s.mu.Unlock()
	return nil
}

func (s *Store) SaveStatus(sandboxID string, status model.SandboxStatus) error {
	if sandboxID == "" || !status.DesiredState.Valid() || !status.ObservedState.Valid() {
		return errors.New("sandbox ID and valid desired/observed states are required")
	}
	lock := s.recordLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	if err := s.write(sandboxID, "state.json", status); err != nil {
		return err
	}
	s.mu.Lock()
	record := s.records[sandboxID]
	record.status = cloneStatus(status)
	record.complete = record.spec.SandboxID == sandboxID
	s.records[sandboxID] = record
	s.ids[sandboxID] = true
	s.updateHeartbeatLocked(sandboxID, status)
	s.mu.Unlock()
	return nil
}

// UpdateStatus applies a read-modify-write while holding the store lock. It is
// used when independent callback and monitor paths update different fields.
func (s *Store) UpdateStatus(sandboxID string, update func(*model.SandboxStatus) error) (model.SandboxStatus, error) {
	lock := s.recordLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	record, err := s.loadRecord(sandboxID)
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
	if err := s.write(sandboxID, "state.json", status); err != nil {
		return model.SandboxStatus{}, err
	}
	record.status = cloneStatus(status)
	s.putRecord(sandboxID, record)
	return cloneStatus(status), nil
}

func (s *Store) Transition(sandboxID string, observed model.SandboxState, update func(*model.SandboxStatus)) (model.SandboxStatus, error) {
	lock := s.recordLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	record, err := s.loadRecord(sandboxID)
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
	if err := s.write(sandboxID, "state.json", status); err != nil {
		return model.SandboxStatus{}, err
	}
	record.status = cloneStatus(status)
	s.putRecord(sandboxID, record)
	return cloneStatus(status), nil
}

// TransitionIf atomically re-checks a lifecycle predicate before publishing a
// transition. A false predicate leaves state unchanged.
func (s *Store) TransitionIf(sandboxID string, observed model.SandboxState, condition func(model.SandboxStatus) bool, update func(*model.SandboxStatus)) (model.SandboxStatus, bool, error) {
	lock := s.recordLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	record, err := s.loadRecord(sandboxID)
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
	if err := s.write(sandboxID, "state.json", status); err != nil {
		return model.SandboxStatus{}, false, err
	}
	record.status = cloneStatus(status)
	s.putRecord(sandboxID, record)
	return cloneStatus(status), true, nil
}

func (s *Store) Load(sandboxID string) (model.SandboxSpec, model.SandboxStatus, error) {
	lock := s.recordLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	record, err := s.loadRecord(sandboxID)
	if err != nil {
		return model.SandboxSpec{}, model.SandboxStatus{}, err
	}
	return cloneSpec(record.spec), cloneStatus(record.status), nil
}

// Cached returns one completely preloaded sandbox record without any
// recovery filesystem fallback. Authentication and routing hot paths use this
// method so unknown identities are cache misses rather than disk probes.
func (s *Store) Cached(sandboxID string) (model.SandboxSpec, model.SandboxStatus, bool) {
	s.mu.RLock()
	record, ok := s.records[sandboxID]
	if ok && record.complete {
		spec, status := cloneSpec(record.spec), cloneStatus(record.status)
		s.mu.RUnlock()
		return spec, status, true
	}
	s.mu.RUnlock()
	return model.SandboxSpec{}, model.SandboxStatus{}, false
}

// Contains reports whether a sandbox identity is already indexed. It never
// probes recovery files.
func (s *Store) Contains(identity string) bool {
	s.mu.RLock()
	_, exists := s.ids[identity]
	s.mu.RUnlock()
	return exists
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

func (s *Store) Delete(sandboxID string) error {
	lock := s.recordLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	directory := filepath.Join(s.root, sandboxID)
	for _, name := range []string{"spec.json", "state.json", "secret.json"} {
		if err := os.Remove(filepath.Join(directory, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.mu.Lock()
	s.removeHeartbeatLocked(sandboxID)
	delete(s.records, sandboxID)
	delete(s.ids, sandboxID)
	s.mu.Unlock()
	return nil
}

// Observe records one authenticated absolute supervisor snapshot in memory.
// Equal-epoch revisions refresh heartbeat freshness; older supervisor epochs
// cannot keep a replacement supervisor healthy or roll state backwards. No
// durable file is touched on this hot path.
func (s *Store) Observe(sandboxID string, snapshot model.RuntimeSnapshot, observedAt time.Time) (bool, error) {
	if sandboxID == "" || snapshot.Revision == 0 || snapshot.SandboxID != sandboxID {
		return false, errors.New("invalid runtime snapshot identity or revision")
	}
	lock := s.recordLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	record, err := s.loadRecord(sandboxID)
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
	s.putRecord(sandboxID, record)
	return applied, nil
}

func (s *Store) Snapshot(sandboxID string) (model.RuntimeSnapshot, bool) {
	s.mu.RLock()
	record, ok := s.records[sandboxID]
	s.mu.RUnlock()
	if !ok || record.snapshot.Revision == 0 {
		return model.RuntimeSnapshot{}, false
	}
	return cloneSnapshot(record.snapshot), true
}

// ClaimStaleHeartbeats removes at most limit sandboxes whose cached
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
		delete(s.heartbeatItems, item.sandboxID)
		result = append(result, item.sandboxID)
	}
	s.mu.Unlock()
	return result
}

// RescheduleHeartbeat restores one claimed sandbox from its newest
// cached status. Terminal or concurrently deleted groups remain absent.
func (s *Store) RescheduleHeartbeat(sandboxID string) {
	s.mu.Lock()
	if record, ok := s.records[sandboxID]; ok && record.complete {
		s.updateHeartbeatLocked(sandboxID, record.status)
	}
	s.mu.Unlock()
}

// ObserveMetrics refreshes non-durable diagnostic metrics for one sandbox.
func (s *Store) ObserveMetrics(sandboxID string, metrics model.ResourceMetrics) error {
	lock := s.recordLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	record, err := s.loadRecord(sandboxID)
	if err != nil {
		return err
	}
	record.status.Metrics = metrics
	s.putRecord(sandboxID, record)
	return nil
}

func (s *Store) recordLock(sandboxID string) *sync.Mutex {
	var hash uint64 = 1469598103934665603
	for index := 0; index < len(sandboxID); index++ {
		hash ^= uint64(sandboxID[index])
		hash *= 1099511628211
	}
	return &s.locks[hash%uint64(len(s.locks))]
}

func (s *Store) loadRecord(sandboxID string) (cachedRecord, error) {
	s.mu.RLock()
	record, ok := s.records[sandboxID]
	s.mu.RUnlock()
	if ok && record.complete {
		return record, nil
	}
	record, err := s.readRecord(sandboxID)
	if err != nil {
		return cachedRecord{}, err
	}
	s.putRecord(sandboxID, record)
	return record, nil
}

func (s *Store) readRecord(sandboxID string) (cachedRecord, error) {
	var record cachedRecord
	if err := s.read(sandboxID, "spec.json", &record.spec); err != nil {
		return record, err
	}
	if err := s.read(sandboxID, "state.json", &record.status); err != nil {
		return record, err
	}
	if record.spec.SandboxID != sandboxID {
		return record, errors.New("sandbox state identity mismatch")
	}
	var secret struct {
		InternalToken string `json:"internal_token"`
	}
	if err := s.read(sandboxID, "secret.json", &secret); err == nil {
		record.spec.InternalToken = secret.InternalToken
	} else if !errors.Is(err, os.ErrNotExist) {
		return record, err
	}
	record.complete = true
	return record, nil
}

func (s *Store) putRecord(sandboxID string, record cachedRecord) {
	s.mu.Lock()
	s.records[sandboxID] = record
	s.ids[sandboxID] = true
	s.updateHeartbeatLocked(sandboxID, record.status)
	s.mu.Unlock()
}

func (s *Store) updateHeartbeatLocked(sandboxID string, status model.SandboxStatus) {
	record, complete := s.records[sandboxID]
	monitor := complete && record.complete && (status.ObservedState == model.StateReady || status.ObservedState == model.StateActive || status.ObservedState == model.StateDraining)
	item, exists := s.heartbeatItems[sandboxID]
	if !monitor {
		if exists {
			heap.Remove(&s.heartbeats, item.index)
			delete(s.heartbeatItems, sandboxID)
		}
		return
	}
	if exists {
		item.observedAt = status.LastHeartbeat
		heap.Fix(&s.heartbeats, item.index)
		return
	}
	item = &heartbeatItem{sandboxID: sandboxID, observedAt: status.LastHeartbeat}
	s.heartbeatItems[sandboxID] = item
	heap.Push(&s.heartbeats, item)
}

func (s *Store) removeHeartbeatLocked(sandboxID string) {
	if item, ok := s.heartbeatItems[sandboxID]; ok {
		heap.Remove(&s.heartbeats, item.index)
		delete(s.heartbeatItems, sandboxID)
	}
}

func (s *Store) write(sandboxID, name string, value any) error {
	if sandboxID == "" || filepath.Base(sandboxID) != sandboxID {
		return errors.New("invalid sandbox ID")
	}
	directory := filepath.Join(s.root, sandboxID)
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

func (s *Store) read(sandboxID, name string, output any) error {
	if sandboxID == "" || filepath.Base(sandboxID) != sandboxID {
		return errors.New("invalid sandbox ID")
	}
	data, err := os.ReadFile(filepath.Join(s.root, sandboxID, name))
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode %s for %s: %w", name, sandboxID, err)
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
