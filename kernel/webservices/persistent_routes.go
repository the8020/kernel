package webservices

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"the8020/kernel/sandbox/model"
)

const RouteHeader = "X-80-20-Route"

var (
	errRouteNotFound = errors.New("persistent route not found")
	errRouteExpired  = errors.New("persistent route expired")
)

type persistentRoute struct {
	ServiceID      string        `json:"service_id"`
	NodeID         string        `json:"node_id"`
	PoolID         string        `json:"pool_id"`
	RuntimeGroupID string        `json:"runtime_group_id"`
	SandboxID      string        `json:"sandbox_id"`
	WorkerID       string        `json:"worker_id,omitempty"`
	ExecutionID    string        `json:"execution_id"`
	UserID         string        `json:"user_id,omitempty"`
	KeepAlive      time.Duration `json:"keep_alive"`
	ExpiresAt      time.Time     `json:"expires_at"`
	Connected      int           `json:"connected"`
}

type persistentRouteRegistry struct {
	mu        sync.Mutex
	nodeID    string
	statePath string
	now       func() time.Time
	routes    map[string]persistentRoute
}

type persistentRouteState struct {
	Schema int                        `json:"schema"`
	Routes map[string]persistentRoute `json:"routes"`
}

func newPersistentRouteRegistry(nodeID string, statePath ...string) *persistentRouteRegistry {
	if nodeID == "" {
		nodeID = "local"
	}
	path := ""
	if len(statePath) > 0 {
		path = statePath[0]
	}
	return &persistentRouteRegistry{
		nodeID: nodeID, statePath: path,
		now:    func() time.Time { return time.Now().UTC() },
		routes: map[string]persistentRoute{},
	}
}

func (r *persistentRouteRegistry) create(serviceID, poolID, runtimeGroupID, sandboxID, userID string, keepAlive time.Duration, connected bool) (string, persistentRoute, error) {
	if serviceID == "" || poolID == "" || runtimeGroupID == "" || sandboxID == "" || keepAlive <= 0 {
		return "", persistentRoute{}, errors.New("persistent route requires service, sandbox pool, and positive keepalive")
	}
	executionID, err := model.NewID("persistent")
	if err != nil {
		return "", persistentRoute{}, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", persistentRoute{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := r.now()
	record := persistentRoute{ServiceID: serviceID, NodeID: r.nodeID, PoolID: poolID, RuntimeGroupID: runtimeGroupID, SandboxID: sandboxID, ExecutionID: executionID, UserID: userID, KeepAlive: keepAlive, ExpiresAt: now.Add(keepAlive)}
	if connected {
		record.Connected = 1
	}
	err = r.update(func(routes map[string]persistentRoute) {
		sweepRoutes(routes, now)
		routes[tokenKey(token)] = record
	})
	return token, record, err
}

func (r *persistentRouteRegistry) lookup(token, serviceID, userID string) (persistentRoute, error) {
	return r.resolveRoute(token, serviceID, userID, false, false)
}

func (r *persistentRouteRegistry) resolve(token, serviceID, userID string, connect bool) (persistentRoute, error) {
	return r.resolveRoute(token, serviceID, userID, connect, true)
}

func (r *persistentRouteRegistry) resolveRoute(token, serviceID, userID string, connect, mutateConnection bool) (persistentRoute, error) {
	if token == "" {
		return persistentRoute{}, errRouteNotFound
	}
	var record persistentRoute
	var resultErr error
	err := r.update(func(routes map[string]persistentRoute) {
		key := tokenKey(token)
		item, exists := routes[key]
		if !exists || item.ServiceID != serviceID || item.UserID != userID {
			resultErr = errRouteNotFound
			return
		}
		if !item.ExpiresAt.After(r.now()) {
			delete(routes, key)
			resultErr = errRouteExpired
			return
		}
		if mutateConnection && connect {
			item.Connected++
			routes[key] = item
		}
		record = item
	})
	if err != nil {
		return persistentRoute{}, err
	}
	return record, resultErr
}

func (r *persistentRouteRegistry) succeed(token, workerID string) {
	_ = r.update(func(routes map[string]persistentRoute) {
		key := tokenKey(token)
		record, exists := routes[key]
		if !exists {
			return
		}
		if workerID != "" {
			record.WorkerID = workerID
		}
		record.ExpiresAt = r.now().Add(record.KeepAlive)
		routes[key] = record
	})
}

func (r *persistentRouteRegistry) disconnect(token string, successful bool) {
	_ = r.update(func(routes map[string]persistentRoute) {
		key := tokenKey(token)
		record, exists := routes[key]
		if !exists {
			return
		}
		record.Connected = max(record.Connected-1, 0)
		if successful {
			record.ExpiresAt = r.now().Add(record.KeepAlive)
		}
		routes[key] = record
	})
}

func (r *persistentRouteRegistry) discard(token string) {
	_ = r.update(func(routes map[string]persistentRoute) { delete(routes, tokenKey(token)) })
}

func (r *persistentRouteRegistry) discardExecution(executionID string) {
	if executionID == "" {
		return
	}
	_ = r.update(func(routes map[string]persistentRoute) {
		for key, record := range routes {
			if record.ExecutionID == executionID {
				delete(routes, key)
			}
		}
	})
}

func (r *persistentRouteRegistry) discardService(serviceID string) {
	if serviceID == "" {
		return
	}
	_ = r.update(func(routes map[string]persistentRoute) {
		for key, record := range routes {
			if record.ServiceID == serviceID {
				delete(routes, key)
			}
		}
	})
}

func (r *persistentRouteRegistry) complete(executionID, serviceID, runtimeGroupID, sandboxID, workerID string) error {
	if executionID == "" || serviceID == "" || runtimeGroupID == "" || sandboxID == "" || workerID == "" {
		return errors.New("complete persistent execution requires exact identity")
	}
	found := false
	mismatch := false
	err := r.update(func(routes map[string]persistentRoute) {
		for key, record := range routes {
			if record.ExecutionID != executionID {
				continue
			}
			found = true
			if record.NodeID != r.nodeID || record.ServiceID != serviceID || record.RuntimeGroupID != runtimeGroupID || record.SandboxID != sandboxID || record.WorkerID != workerID {
				mismatch = true
				return
			}
			delete(routes, key)
		}
	})
	if err != nil {
		return err
	}
	if !found {
		return errRouteNotFound
	}
	if mismatch {
		return errors.New("persistent execution target does not match route")
	}
	return nil
}

func (r *persistentRouteRegistry) hasPool(poolID string) bool {
	result := false
	_ = r.update(func(routes map[string]persistentRoute) {
		sweepRoutes(routes, r.now())
		for _, record := range routes {
			if record.PoolID == poolID {
				result = true
				return
			}
		}
	})
	return result
}

func (r *persistentRouteRegistry) update(action func(map[string]persistentRoute)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.statePath == "" {
		action(r.routes)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.statePath), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(r.statePath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN) //nolint:errcheck
	routes, err := readPersistentRoutes(r.statePath)
	if err != nil {
		return err
	}
	action(routes)
	return writePersistentRoutes(r.statePath, routes)
}

func readPersistentRoutes(path string) (map[string]persistentRoute, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]persistentRoute{}, nil
	}
	if err != nil {
		return nil, err
	}
	var state persistentRouteState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode persistent routes: %w", err)
	}
	if state.Schema != 1 || state.Routes == nil {
		return nil, errors.New("persistent route state has unsupported schema")
	}
	return state.Routes, nil
}

func writePersistentRoutes(path string, routes map[string]persistentRoute) error {
	data, err := json.MarshalIndent(persistentRouteState{Schema: 1, Routes: routes}, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".persistent-routes-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(append(data, '\n'))
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func sweepRoutes(routes map[string]persistentRoute, now time.Time) {
	for key, record := range routes {
		if !record.ExpiresAt.After(now) {
			delete(routes, key)
		}
	}
}
