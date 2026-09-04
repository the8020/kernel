package webservices

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"the8020/kernel/database"
	"the8020/kernel/sandbox/model"
)

const RouteHeader = "X-80-20-Route"
const routesTable = `"the8020__services__routes"`

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
	mu       sync.Mutex
	nodeID   string
	database database.Store
	now      func() time.Time
}

func newPersistentRouteRegistry(nodeID string, store database.Store) *persistentRouteRegistry {
	if nodeID == "" {
		nodeID = "local"
	}
	return &persistentRouteRegistry{
		nodeID: nodeID, database: store,
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (r *persistentRouteRegistry) create(serviceID, poolID, runtimeGroupID, sandboxID, userID string, keepAlive time.Duration, connected bool) (string, persistentRoute, error) {
	if r.database == nil {
		return "", persistentRoute{}, errors.New("persistent route database is unavailable")
	}
	if serviceID == "" || poolID == "" || runtimeGroupID == "" || sandboxID == "" || keepAlive <= 0 {
		return "", persistentRoute{}, errors.New("persistent route requires service, sandbox pool, and positive keepalive")
	}
	executionID, err := model.NewID("persistent")
	if err != nil {
		return "", persistentRoute{}, err
	}
	for attempt := 0; attempt < 16; attempt++ {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", persistentRoute{}, err
		}
		token := base64.RawURLEncoding.EncodeToString(raw)
		now := r.now().UTC()
		record := persistentRoute{
			ServiceID: serviceID, NodeID: r.nodeID, PoolID: poolID,
			RuntimeGroupID: runtimeGroupID, SandboxID: sandboxID,
			ExecutionID: executionID, UserID: userID, KeepAlive: keepAlive,
			ExpiresAt: now.Add(keepAlive),
		}
		if connected {
			record.Connected = 1
		}
		result, err := r.database.ExecContext(context.Background(), `INSERT INTO `+routesTable+` ("tokenHash", "serviceId", "nodeId", "poolId", "runtimeGroupId", "sandboxId", "workerId", "executionId", "userId", "keepAliveMs", "expiresAt", "connected") VALUES ($1, $2, $3, $4, $5, $6, '', $7, $8, $9, $10, $11) ON CONFLICT ("tokenHash") DO NOTHING`,
			tokenKey(token), record.ServiceID, record.NodeID, record.PoolID,
			record.RuntimeGroupID, record.SandboxID, record.ExecutionID,
			record.UserID, record.KeepAlive.Milliseconds(),
			database.EncodeTime(r.database, record.ExpiresAt), record.Connected)
		if err != nil {
			return "", persistentRoute{}, err
		}
		if affected, err := result.RowsAffected(); err != nil {
			return "", persistentRoute{}, err
		} else if affected == 0 {
			continue
		}
		return token, record, nil
	}
	return "", persistentRoute{}, errors.New("persistent route token collision limit exceeded")
}

func (r *persistentRouteRegistry) lookup(token, serviceID, userID string) (persistentRoute, error) {
	return r.resolveRoute(token, serviceID, userID, false, false)
}

func (r *persistentRouteRegistry) resolve(token, serviceID, userID string, connect bool) (persistentRoute, error) {
	return r.resolveRoute(token, serviceID, userID, connect, true)
}

func (r *persistentRouteRegistry) resolveRoute(token, serviceID, userID string, connect, mutateConnection bool) (persistentRoute, error) {
	if token == "" || r.database == nil {
		return persistentRoute{}, errRouteNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, err := r.read(tokenKey(token), serviceID, userID)
	if err != nil {
		return persistentRoute{}, err
	}
	if !record.ExpiresAt.After(r.now().UTC()) {
		_, _ = r.database.ExecContext(context.Background(), `DELETE FROM `+routesTable+` WHERE "tokenHash" = $1`, tokenKey(token))
		return persistentRoute{}, errRouteExpired
	}
	if mutateConnection && connect {
		if _, err := r.database.ExecContext(context.Background(), `UPDATE `+routesTable+` SET "connected" = "connected" + 1 WHERE "tokenHash" = $1`, tokenKey(token)); err != nil {
			return persistentRoute{}, err
		}
		record.Connected++
	}
	return record, nil
}

func (r *persistentRouteRegistry) read(key, serviceID, userID string) (persistentRoute, error) {
	row := r.database.QueryRowContext(context.Background(), `SELECT "serviceId", "nodeId", "poolId", "runtimeGroupId", "sandboxId", "workerId", "executionId", "userId", "keepAliveMs", "expiresAt", "connected" FROM `+routesTable+` WHERE "tokenHash" = $1 AND "serviceId" = $2 AND "userId" = $3`, key, serviceID, userID)
	var record persistentRoute
	var keepAlive int64
	var expires any
	if err := row.Scan(&record.ServiceID, &record.NodeID, &record.PoolID, &record.RuntimeGroupID, &record.SandboxID, &record.WorkerID, &record.ExecutionID, &record.UserID, &keepAlive, &expires, &record.Connected); errors.Is(err, sql.ErrNoRows) {
		return persistentRoute{}, errRouteNotFound
	} else if err != nil {
		return persistentRoute{}, err
	}
	record.KeepAlive = time.Duration(keepAlive) * time.Millisecond
	var err error
	record.ExpiresAt, err = database.DecodeTime(expires)
	return record, err
}

func (r *persistentRouteRegistry) succeed(token, workerID string) {
	if token == "" || r.database == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := tokenKey(token)
	var keepAlive int64
	if err := r.database.QueryRowContext(context.Background(), `SELECT "keepAliveMs" FROM `+routesTable+` WHERE "tokenHash" = $1`, key).Scan(&keepAlive); err != nil {
		return
	}
	expires := r.now().UTC().Add(time.Duration(keepAlive) * time.Millisecond)
	_, _ = r.database.ExecContext(context.Background(), `UPDATE `+routesTable+` SET "workerId" = CASE WHEN $1 = '' THEN "workerId" ELSE $1 END, "expiresAt" = $2 WHERE "tokenHash" = $3`, workerID, database.EncodeTime(r.database, expires), key)
}

func (r *persistentRouteRegistry) disconnect(token string, successful bool) {
	if token == "" || r.database == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := tokenKey(token)
	var keepAlive int64
	if err := r.database.QueryRowContext(context.Background(), `SELECT "keepAliveMs" FROM `+routesTable+` WHERE "tokenHash" = $1`, key).Scan(&keepAlive); err != nil {
		return
	}
	if successful {
		expires := r.now().UTC().Add(time.Duration(keepAlive) * time.Millisecond)
		_, _ = r.database.ExecContext(context.Background(), `UPDATE `+routesTable+` SET "connected" = CASE WHEN "connected" > 0 THEN "connected" - 1 ELSE 0 END, "expiresAt" = $1 WHERE "tokenHash" = $2`, database.EncodeTime(r.database, expires), key)
		return
	}
	_, _ = r.database.ExecContext(context.Background(), `UPDATE `+routesTable+` SET "connected" = CASE WHEN "connected" > 0 THEN "connected" - 1 ELSE 0 END WHERE "tokenHash" = $1`, key)
}

func (r *persistentRouteRegistry) discard(token string) {
	if token != "" && r.database != nil {
		_, _ = r.database.ExecContext(context.Background(), `DELETE FROM `+routesTable+` WHERE "tokenHash" = $1`, tokenKey(token))
	}
}

func (r *persistentRouteRegistry) discardExecution(executionID string) {
	if executionID != "" && r.database != nil {
		_, _ = r.database.ExecContext(context.Background(), `DELETE FROM `+routesTable+` WHERE "executionId" = $1`, executionID)
	}
}

func (r *persistentRouteRegistry) discardService(serviceID string) {
	if serviceID != "" && r.database != nil {
		_, _ = r.database.ExecContext(context.Background(), `DELETE FROM `+routesTable+` WHERE "serviceId" = $1`, serviceID)
	}
}

func (r *persistentRouteRegistry) complete(executionID, serviceID, runtimeGroupID, sandboxID, workerID string) error {
	if executionID == "" || serviceID == "" || runtimeGroupID == "" || sandboxID == "" || workerID == "" {
		return errors.New("complete persistent execution requires exact identity")
	}
	if r.database == nil {
		return errors.New("persistent route database is unavailable")
	}
	result, err := r.database.ExecContext(context.Background(), `DELETE FROM `+routesTable+` WHERE "executionId" = $1 AND "nodeId" = $2 AND "serviceId" = $3 AND "runtimeGroupId" = $4 AND "sandboxId" = $5 AND "workerId" = $6`, executionID, r.nodeID, serviceID, runtimeGroupID, sandboxID, workerID)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected > 0 {
		return nil
	}
	var count int
	if err := r.database.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM `+routesTable+` WHERE "executionId" = $1`, executionID).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return errRouteNotFound
	}
	return errors.New("persistent execution target does not match route")
}

func (r *persistentRouteRegistry) hasPool(poolID string) bool {
	if poolID == "" || r.database == nil {
		return false
	}
	now := database.EncodeTime(r.database, r.now())
	_, _ = r.database.ExecContext(context.Background(), `DELETE FROM `+routesTable+` WHERE "expiresAt" <= $1`, now)
	var count int
	if err := r.database.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM `+routesTable+` WHERE "poolId" = $1 AND "expiresAt" > $2`, poolID, now).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (r persistentRoute) validate() error {
	if r.ServiceID == "" || r.NodeID == "" || r.PoolID == "" || r.RuntimeGroupID == "" || r.SandboxID == "" || r.ExecutionID == "" || r.KeepAlive <= 0 || r.ExpiresAt.IsZero() || r.Connected < 0 {
		return fmt.Errorf("invalid persistent route")
	}
	return nil
}
