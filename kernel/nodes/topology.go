// Package nodes owns shared application-server topology and authenticated
// kernel-to-kernel service forwarding.
package nodes

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"the8020/kernel/database"
	"the8020/kernel/execution"
)

type forwardingPathKey struct{}

const capacityPath = "/__the8020/node/capacity"
const workerInvokePath = "/__the8020/node/worker/invoke"
const maximumWorkerInvocationBytes = 1 << 20

var nodeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type Node struct {
	ID               string `json:"id"`
	URL              string `json:"url"`
	RecipientAddress string `json:"recipient_address"`
	RecipientPort    int    `json:"recipient_port"`
	Enabled          bool   `json:"enabled"`
}

type ServiceCapacity struct {
	ServiceID        string `json:"service_id"`
	SandboxCount     int    `json:"sandbox_count"`
	HealthySandboxes int    `json:"healthy_sandboxes"`
	WorkerCount      int    `json:"worker_count"`
	ExecutionSlots   int    `json:"execution_slots"`
	OccupiedSlots    int    `json:"occupied_slots"`
}

type Capacity struct {
	NodeID                         string            `json:"node_id"`
	Accepting                      bool              `json:"accepting"`
	TemporaryStorageBudgetBytes    int64             `json:"temporary_storage_budget_bytes"`
	TemporaryStorageReservedBytes  int64             `json:"temporary_storage_reserved_bytes"`
	TemporaryStorageAvailableBytes int64             `json:"temporary_storage_available_bytes"`
	MaximumSandboxes               int               `json:"maximum_sandboxes"`
	SandboxCount                   int               `json:"sandbox_count"`
	AvailableSandboxes             int               `json:"available_sandboxes"`
	MaximumWorkers                 int               `json:"maximum_workers"`
	WorkerCount                    int               `json:"worker_count"`
	AvailableWorkers               int               `json:"available_workers"`
	RunningServiceSandboxes        int               `json:"running_service_sandboxes"`
	HealthyServiceSandboxes        int               `json:"healthy_service_sandboxes"`
	ExecutionSlots                 int               `json:"execution_slots"`
	OccupiedExecutionSlots         int               `json:"occupied_execution_slots"`
	Services                       []ServiceCapacity `json:"services,omitempty"`
	UpdatedAt                      time.Time         `json:"updated_at"`
}

type CapacityProvider interface {
	NodeCapacity(context.Context) (Capacity, error)
}

type WorkerInvocationRequest struct {
	NodeID                string         `json:"node_id"`
	SandboxID             string         `json:"sandbox_id"`
	WorkerID              string         `json:"worker_id"`
	PersistentExecutionID string         `json:"persistent_execution_id,omitempty"`
	Function              string         `json:"function"`
	Input                 any            `json:"input"`
	User                  execution.User `json:"user"`
}

type WorkerInvocationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type WorkerInvocationResult struct {
	OK     bool                   `json:"ok"`
	Output any                    `json:"output,omitempty"`
	Error  *WorkerInvocationError `json:"error,omitempty"`
}

type WorkerInvoker interface {
	InvokeLocalWorker(context.Context, WorkerInvocationRequest) WorkerInvocationResult
}

type Status struct {
	Node      Node      `json:"node"`
	Local     bool      `json:"local"`
	Reachable bool      `json:"reachable"`
	Capacity  *Capacity `json:"capacity,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type Manager struct {
	mu       sync.RWMutex
	database database.Store
	secret   string
	localID  string
	nodes    map[string]Node
	server   *http.Server
	listener net.Listener
	http     *http.Client
	capacity CapacityProvider
	workers  WorkerInvoker
}

func New(store database.Store, localID, sharedSecret string) (*Manager, error) {
	if store == nil || !nodeIDPattern.MatchString(localID) || sharedSecret == "" {
		return nil, errors.New("database, valid local node ID, and shared forwarding secret are required")
	}
	manager := &Manager{database: store, secret: sharedSecret, localID: localID, nodes: map[string]Node{}, http: &http.Client{Transport: http.DefaultTransport, Timeout: 2 * time.Second}}
	if err := manager.Refresh(context.Background()); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *Manager) LocalNodeID() string { return m.localID }

// LocalIndexes deterministically partitions global service allocation indexes
// across enabled nodes. An unconfigured local node is the sole-node default;
// an explicitly disabled local node receives no indexes.
func (m *Manager) LocalIndexes(limit int) []int {
	if limit <= 0 {
		return nil
	}
	localOffset, width := m.localIndexPartition()
	if localOffset < 0 {
		return nil
	}
	result := make([]int, 0, (limit+width-1)/width)
	for index := localOffset; index < limit; index += width {
		result = append(result, index)
	}
	return result
}

// OwnsIndex reports whether this node owns one deterministic service
// allocation index. It allows unlimited services to grow without inventing a
// separate sandbox-count partition.
func (m *Manager) OwnsIndex(index int) bool {
	if index < 0 {
		return false
	}
	localOffset, width := m.localIndexPartition()
	return localOffset >= 0 && index%width == localOffset
}

func (m *Manager) localIndexPartition() (int, int) {
	nodes := m.List()
	ids := make([]string, 0, len(nodes)+1)
	localConfigured := false
	for _, node := range nodes {
		if node.ID == m.localID {
			localConfigured = true
		}
		if node.Enabled {
			ids = append(ids, node.ID)
		}
	}
	if !localConfigured {
		ids = append(ids, m.localID)
	}
	sort.Strings(ids)
	localOffset := -1
	for index, id := range ids {
		if id == m.localID {
			localOffset = index
			break
		}
	}
	if localOffset < 0 || len(ids) == 0 {
		return -1, 0
	}
	return localOffset, len(ids)
}

func (m *Manager) SetCapacityProvider(provider CapacityProvider) {
	m.mu.Lock()
	m.capacity = provider
	m.mu.Unlock()
}

func (m *Manager) SetWorkerInvoker(invoker WorkerInvoker) {
	m.mu.Lock()
	m.workers = invoker
	m.mu.Unlock()
}

func (m *Manager) InvokeWorker(ctx context.Context, input WorkerInvocationRequest) WorkerInvocationResult {
	body, err := encodeWorkerInvocation(input)
	if err != nil {
		return workerInvocationFailure("invalid_request", err.Error())
	}
	if input.NodeID == m.localID {
		m.mu.RLock()
		invoker := m.workers
		m.mu.RUnlock()
		if invoker == nil {
			return workerInvocationFailure("unavailable", "local Worker control is unavailable")
		}
		return invoker.InvokeLocalWorker(ctx, input)
	}
	node, err := m.Inspect(input.NodeID)
	if err != nil || !node.Enabled {
		return workerInvocationFailure("target_not_found", fmt.Sprintf("target node %q is unavailable", input.NodeID))
	}
	target := "http://" + net.JoinHostPort(node.RecipientAddress, strconv.Itoa(node.RecipientPort)) + workerInvokePath
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return workerInvocationFailure("unavailable", err.Error())
	}
	request.Header.Set("Authorization", "Bearer "+m.secret)
	request.Header.Set("Content-Type", "application/json")
	response, err := m.http.Do(request)
	if err != nil {
		var networkError net.Error
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout() {
			return workerInvocationFailure("timeout", "Worker invocation timed out")
		}
		return workerInvocationFailure("unavailable", "target node is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return workerInvocationFailure("unavailable", fmt.Sprintf("target node returned %s", response.Status))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumWorkerInvocationBytes+1))
	if err != nil || len(data) > maximumWorkerInvocationBytes {
		return workerInvocationFailure("unavailable", "target node returned an oversized Worker result")
	}
	var result WorkerInvocationResult
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || decoder.Decode(&struct{}{}) != io.EOF || result.OK == (result.Error != nil) {
		return workerInvocationFailure("unavailable", "target node returned an invalid Worker result")
	}
	return result
}

func (m *Manager) List() []Node {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Node, 0, len(m.nodes))
	for _, node := range m.nodes {
		result = append(result, node)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (m *Manager) Inspect(id string) (Node, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	node, exists := m.nodes[id]
	if !exists {
		return Node{}, fmt.Errorf("node %q is not configured", id)
	}
	return node, nil
}

// Refresh atomically replaces the cached topology from shared state. Hot
// routing and allocation paths read only the cache.
func (m *Manager) Refresh(ctx context.Context) error {
	rows, err := m.database.QueryContext(ctx, `SELECT "id", "url", "recipientAddress", "recipientPort", "enabled" FROM "the8020__system__nodes" ORDER BY "id"`)
	if err != nil {
		return err
	}
	defer rows.Close()
	nodes := map[string]Node{}
	for rows.Next() {
		var node Node
		if err := rows.Scan(&node.ID, &node.URL, &node.RecipientAddress, &node.RecipientPort, &node.Enabled); err != nil {
			return err
		}
		if err := validateNode(node); err != nil {
			return fmt.Errorf("node %q: %w", node.ID, err)
		}
		nodes[node.ID] = node
	}
	if err := rows.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	m.nodes = nodes
	m.mu.Unlock()
	return nil
}

func (m *Manager) Set(ctx context.Context, node Node) (Node, error) {
	if err := validateNode(node); err != nil {
		return Node{}, err
	}
	_, err := m.database.ExecContext(ctx, `INSERT INTO "the8020__system__nodes" ("id", "url", "recipientAddress", "recipientPort", "enabled", "updatedAt") VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT ("id") DO UPDATE SET "url" = excluded."url", "recipientAddress" = excluded."recipientAddress", "recipientPort" = excluded."recipientPort", "enabled" = excluded."enabled", "updatedAt" = excluded."updatedAt"`, node.ID, node.URL, node.RecipientAddress, node.RecipientPort, node.Enabled, database.EncodeTime(m.database, time.Now()))
	if err != nil {
		return Node{}, err
	}
	if err := m.Refresh(ctx); err != nil {
		return Node{}, err
	}
	return node, nil
}

func (m *Manager) Remove(ctx context.Context, id string) error {
	if !nodeIDPattern.MatchString(id) {
		return errors.New("valid node ID is required")
	}
	if id == m.localID {
		return errors.New("cannot remove the running local node")
	}
	_, err := m.database.ExecContext(ctx, `DELETE FROM "the8020__system__nodes" WHERE "id" = $1`, id)
	if err == nil {
		err = m.Refresh(ctx)
	}
	return err
}

// Start exposes the authenticated recipient listener declared for this node.
// Topology mutations affecting this listener take effect on kernel restart.
func (m *Manager) Start(handler http.Handler) error {
	if handler == nil {
		return errors.New("forwarded service handler is required")
	}
	node, err := m.Inspect(m.localID)
	if err != nil || !node.Enabled {
		return nil
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(node.RecipientAddress, strconv.Itoa(node.RecipientPort)))
	if err != nil {
		return fmt.Errorf("listen for node forwarding: %w", err)
	}
	server := &http.Server{Handler: m.authorize(m.recipientHandler(handler)), ReadHeaderTimeout: 5 * time.Second}
	m.mu.Lock()
	m.listener, m.server = listener, server
	m.mu.Unlock()
	go func() { _ = server.Serve(listener) }()
	return nil
}

// Proxy keeps the receiving node in the HTTP or WebSocket transport path.
func (m *Manager) Proxy(nodeID string, writer http.ResponseWriter, request *http.Request) error {
	if nodeID == m.localID {
		return errors.New("refusing recursive forwarding to local node")
	}
	node, err := m.Inspect(nodeID)
	if err != nil {
		return err
	}
	if !node.Enabled {
		return fmt.Errorf("node %q is disabled", nodeID)
	}
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(node.RecipientAddress, strconv.Itoa(node.RecipientPort))}
	proxy := &httputil.ReverseProxy{
		Director: func(forwarded *http.Request) {
			forwarded.URL.Scheme = target.Scheme
			forwarded.URL.Host = target.Host
			forwarded.Host = target.Host
			forwarded.Header.Set("Authorization", "Bearer "+m.secret)
			path, _ := request.Context().Value(forwardingPathKey{}).([]string)
			forwarded.Header.Set("the8020-internal-forwarded-nodes", strings.Join(append(append([]string(nil), path...), m.localID), ","))
		},
		Transport: m.http.Transport,
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, proxyErr error) {
			http.Error(response, "owning node unavailable", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(writer, request)
	return nil
}

// ProxyAvailable forwards new work to the first enabled node not already in
// the forwarding path. It returns false when no remote candidate remains.
func (m *Manager) ProxyAvailable(writer http.ResponseWriter, request *http.Request) (bool, error) {
	visited := map[string]bool{m.localID: true}
	path, _ := request.Context().Value(forwardingPathKey{}).([]string)
	for _, id := range path {
		if id = strings.TrimSpace(id); id != "" {
			visited[id] = true
		}
	}
	candidates := m.availableNodes(request.Context(), visited)
	for _, node := range candidates {
		return true, m.Proxy(node.ID, writer, request)
	}
	return false, nil
}

func (m *Manager) Statuses(ctx context.Context) []Status {
	nodes := m.List()
	foundLocal := false
	for _, node := range nodes {
		foundLocal = foundLocal || node.ID == m.localID
	}
	if !foundLocal {
		nodes = append(nodes, Node{ID: m.localID, Enabled: true})
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	}
	result := make([]Status, len(nodes))
	var wait sync.WaitGroup
	for index, node := range nodes {
		index, node := index, node
		wait.Add(1)
		go func() {
			defer wait.Done()
			status := Status{Node: node, Local: node.ID == m.localID}
			var capacity Capacity
			var err error
			if status.Local {
				capacity, err = m.localCapacity(ctx)
			} else if node.Enabled {
				capacity, err = m.fetchCapacity(ctx, node)
			} else {
				err = errors.New("node is disabled")
			}
			if err == nil {
				status.Reachable, status.Capacity = true, &capacity
			} else {
				status.Error = err.Error()
			}
			result[index] = status
		}()
	}
	wait.Wait()
	return result
}

func (m *Manager) availableNodes(ctx context.Context, visited map[string]bool) []Node {
	type result struct {
		node     Node
		capacity Capacity
	}
	var candidates []Node
	for _, node := range m.List() {
		if node.Enabled && !visited[node.ID] {
			candidates = append(candidates, node)
		}
	}
	results := make(chan result, len(candidates))
	var wait sync.WaitGroup
	for _, node := range candidates {
		node := node
		wait.Add(1)
		go func() {
			defer wait.Done()
			capacity, err := m.fetchCapacity(ctx, node)
			if err == nil && capacity.Accepting {
				results <- result{node: node, capacity: capacity}
			}
		}()
	}
	wait.Wait()
	close(results)
	available := make([]result, 0, len(candidates))
	for item := range results {
		available = append(available, item)
	}
	sort.Slice(available, func(i, j int) bool {
		left, right := available[i].capacity, available[j].capacity
		if left.AvailableWorkers != right.AvailableWorkers {
			return left.AvailableWorkers > right.AvailableWorkers
		}
		if left.AvailableSandboxes != right.AvailableSandboxes {
			return left.AvailableSandboxes > right.AvailableSandboxes
		}
		return available[i].node.ID < available[j].node.ID
	})
	ordered := make([]Node, len(available))
	for index := range available {
		ordered[index] = available[index].node
	}
	return ordered
}

func (m *Manager) recipientHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == capacityPath {
			capacity, err := m.localCapacity(request.Context())
			if err != nil {
				http.Error(writer, "node capacity unavailable", http.StatusServiceUnavailable)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(capacity)
			return
		}
		if request.Method == http.MethodPost && request.URL.Path == workerInvokePath {
			m.mu.RLock()
			invoker := m.workers
			m.mu.RUnlock()
			if invoker == nil {
				http.Error(writer, "Worker control unavailable", http.StatusServiceUnavailable)
				return
			}
			var input WorkerInvocationRequest
			decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maximumWorkerInvocationBytes))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF || input.NodeID != m.localID {
				http.Error(writer, "invalid Worker invocation", http.StatusBadRequest)
				return
			}
			if _, err := encodeWorkerInvocation(input); err != nil {
				http.Error(writer, "invalid Worker invocation", http.StatusBadRequest)
				return
			}
			result := invoker.InvokeLocalWorker(request.Context(), input)
			data, err := json.Marshal(result)
			if err != nil || len(data) > maximumWorkerInvocationBytes {
				http.Error(writer, "Worker control result exceeds limit", http.StatusInternalServerError)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write(data)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func workerInvocationFailure(code, message string) WorkerInvocationResult {
	return WorkerInvocationResult{Error: &WorkerInvocationError{Code: code, Message: message}}
}

func encodeWorkerInvocation(input WorkerInvocationRequest) ([]byte, error) {
	if input.NodeID == "" || input.SandboxID == "" || input.WorkerID == "" || input.Function == "" || len(input.Function) > 128 || !input.User.Valid() {
		return nil, errors.New("exact Worker target and registered function are required")
	}
	data, err := json.Marshal(input)
	if err != nil {
		return nil, errors.New("Worker invocation input must be JSON serializable")
	}
	if len(data) > maximumWorkerInvocationBytes {
		return nil, errors.New("Worker invocation input exceeds 1 MiB")
	}
	return data, nil
}

func (m *Manager) localCapacity(ctx context.Context) (Capacity, error) {
	m.mu.RLock()
	provider := m.capacity
	m.mu.RUnlock()
	if provider == nil {
		return Capacity{}, errors.New("node capacity provider is unavailable")
	}
	capacity, err := provider.NodeCapacity(ctx)
	capacity.NodeID = m.localID
	return capacity, err
}

func (m *Manager) fetchCapacity(ctx context.Context, node Node) (Capacity, error) {
	target := "http://" + net.JoinHostPort(node.RecipientAddress, strconv.Itoa(node.RecipientPort)) + capacityPath
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Capacity{}, err
	}
	request.Header.Set("Authorization", "Bearer "+m.secret)
	response, err := m.http.Do(request)
	if err != nil {
		return Capacity{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Capacity{}, fmt.Errorf("node %s capacity returned %s", node.ID, response.Status)
	}
	var capacity Capacity
	if err := json.NewDecoder(response.Body).Decode(&capacity); err != nil {
		return Capacity{}, err
	}
	if capacity.NodeID != node.ID {
		return Capacity{}, fmt.Errorf("node capacity identity mismatch: expected %s, received %s", node.ID, capacity.NodeID)
	}
	return capacity, nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	server, listener := m.server, m.listener
	m.server, m.listener = nil, nil
	m.mu.Unlock()
	if server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(server.Shutdown(ctx), listener.Close())
}

func (m *Manager) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		provided := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(m.secret)) != 1 {
			http.Error(writer, "node authentication required", http.StatusUnauthorized)
			return
		}
		request.Header.Del("Authorization")
		// Only authenticated peers may supply forwarding history. Public
		// request headers are never consulted for peer selection.
		path := strings.Split(request.Header.Get("the8020-internal-forwarded-nodes"), ",")
		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), forwardingPathKey{}, path)))
	})
}

func validateNode(node Node) error {
	if !nodeIDPattern.MatchString(node.ID) || strings.TrimSpace(node.RecipientAddress) == "" || node.RecipientPort < 1 || node.RecipientPort > 65535 {
		return errors.New("node requires a valid ID, recipient address, and recipient port")
	}
	parsed, err := url.Parse(node.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("node URL must be an absolute HTTP or HTTPS URL")
	}
	return nil
}
