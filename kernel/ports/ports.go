// Package ports owns host listeners that stream traffic into sandbox endpoints.
package ports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"the8020/kernel/sandbox/model"
)

type Request struct {
	SandboxID    string
	OwnerID      string
	SandboxIP    string
	InternalPort int
	TargetPort   int
	BindAddress  string
	HostPort     int
	Protocol     string
	Purpose      string
	ExpiresAt    time.Time
}

type Lease struct {
	LeaseID      string    `json:"lease_id"`
	SandboxID    string    `json:"sandbox_id"`
	OwnerID      string    `json:"owner_id"`
	SandboxIP    string    `json:"sandbox_ip"`
	BindAddress  string    `json:"bind_address"`
	HostPort     int       `json:"host_port"`
	InternalPort int       `json:"internal_port"`
	TargetPort   int       `json:"target_port,omitempty"`
	Protocol     string    `json:"protocol"`
	Purpose      string    `json:"purpose"`
	State        string    `json:"state"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

type Manager struct {
	mu          sync.Mutex
	root        string
	allowPublic bool
	logger      *slog.Logger
	leases      map[string]Lease
	listeners   map[string]net.Listener
	cancel      map[string]context.CancelFunc
	now         func() time.Time
	dial        func(context.Context, string, string) (net.Conn, error)
}

// ExposeHTTP creates a host-owned HTTP listener whose handler remains in Go.
func (m *Manager) ExposeHTTP(ctx context.Context, request Request, handler http.Handler) (Lease, error) {
	if handler == nil {
		return Lease{}, errors.New("HTTP handler is required")
	}
	request.Protocol = "http"
	return m.expose(ctx, request, handler)
}

// AttachHTTP replaces a restored raw HTTP lease with a Go-owned HTTP handler
// while preserving the durable lease identity and creation time.
func (m *Manager) AttachHTTP(ctx context.Context, leaseID string, handler http.Handler) (Lease, error) {
	if handler == nil {
		return Lease{}, errors.New("HTTP handler is required")
	}
	m.mu.Lock()
	prior, exists := m.leases[leaseID]
	m.mu.Unlock()
	if !exists {
		return Lease{}, fmt.Errorf("port lease %q is unavailable", leaseID)
	}
	if prior.Protocol != "http" {
		return Lease{}, fmt.Errorf("port lease %q is not HTTP", leaseID)
	}
	if err := m.Close(leaseID); err != nil {
		return Lease{}, fmt.Errorf("close restored HTTP lease %s: %w", leaseID, err)
	}
	request := Request{SandboxID: prior.SandboxID, OwnerID: prior.OwnerID, SandboxIP: prior.SandboxIP, InternalPort: prior.InternalPort, TargetPort: prior.TargetPort, BindAddress: prior.BindAddress, HostPort: prior.HostPort, Protocol: "http", Purpose: prior.Purpose, ExpiresAt: prior.ExpiresAt}
	lease, err := m.exposeIdentity(ctx, request, handler, prior.LeaseID, prior.CreatedAt)
	if err == nil {
		return lease, nil
	}
	_, rollbackErr := m.exposeIdentity(context.Background(), request, nil, prior.LeaseID, prior.CreatedAt)
	if rollbackErr != nil {
		return Lease{}, errors.Join(fmt.Errorf("attach HTTP handler to lease %s: %w", leaseID, err), fmt.Errorf("restore raw lease after attach failure: %w", rollbackErr))
	}
	return Lease{}, fmt.Errorf("attach HTTP handler to lease %s: %w", leaseID, err)
}

func New(root string, allowPublic bool, logger *slog.Logger) (*Manager, error) {
	if root == "" {
		return nil, errors.New("port lease state root is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, err
	}
	dialer := &net.Dialer{}
	return &Manager{root: root, allowPublic: allowPublic, logger: logger, leases: map[string]Lease{}, listeners: map[string]net.Listener{}, cancel: map[string]context.CancelFunc{}, now: func() time.Time { return time.Now().UTC() }, dial: dialer.DialContext}, nil
}

func (m *Manager) Expose(ctx context.Context, request Request) (Lease, error) {
	return m.expose(ctx, request, nil)
}

func (m *Manager) expose(ctx context.Context, request Request, handler http.Handler) (Lease, error) {
	return m.exposeIdentity(ctx, request, handler, "", time.Time{})
}

func (m *Manager) exposeIdentity(ctx context.Context, request Request, handler http.Handler, leaseID string, createdAt time.Time) (Lease, error) {
	if request.SandboxID == "" || net.ParseIP(request.SandboxIP) == nil || request.InternalPort < 1 || request.InternalPort > 65535 || request.HostPort < 0 || request.HostPort > 65535 {
		return Lease{}, errors.New("sandbox identity, IP, and valid internal/host ports are required")
	}
	if request.TargetPort == 0 {
		request.TargetPort = request.InternalPort
	}
	if request.TargetPort < 1 || request.TargetPort > 65535 {
		return Lease{}, errors.New("valid target port is required")
	}
	if request.BindAddress == "" {
		request.BindAddress = "127.0.0.1"
	}
	bindIP := net.ParseIP(request.BindAddress)
	if bindIP == nil {
		return Lease{}, errors.New("bind address must be an IP address")
	}
	if !bindIP.IsLoopback() && !m.allowPublic {
		return Lease{}, errors.New("public host-port exposure is disabled")
	}
	if request.Protocol == "" {
		request.Protocol = "tcp"
	}
	if request.Protocol != "tcp" && request.Protocol != "http" {
		return Lease{}, fmt.Errorf("unsupported port protocol %q", request.Protocol)
	}
	if request.Purpose == "" {
		return Lease{}, errors.New("port lease purpose is required")
	}
	if request.OwnerID == "" {
		request.OwnerID = request.SandboxID
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(request.BindAddress, fmt.Sprintf("%d", request.HostPort)))
	if err != nil {
		return Lease{}, fmt.Errorf("listen on requested host port: %w", err)
	}
	if leaseID == "" {
		leaseID, err = model.NewID("port")
		if err != nil {
			_ = listener.Close()
			return Lease{}, err
		}
	} else if !safeLeaseID(leaseID) {
		_ = listener.Close()
		return Lease{}, errors.New("persisted port lease ID is unsafe")
	}
	if createdAt.IsZero() {
		createdAt = m.now()
	}
	lease := Lease{LeaseID: leaseID, SandboxID: request.SandboxID, OwnerID: request.OwnerID, SandboxIP: request.SandboxIP, BindAddress: request.BindAddress, HostPort: listener.Addr().(*net.TCPAddr).Port, InternalPort: request.InternalPort, TargetPort: request.TargetPort, Protocol: request.Protocol, Purpose: request.Purpose, State: "ACTIVE", CreatedAt: createdAt, ExpiresAt: request.ExpiresAt}
	if !lease.ExpiresAt.IsZero() && !lease.ExpiresAt.After(lease.CreatedAt) {
		_ = listener.Close()
		return Lease{}, errors.New("port lease expiration must be in the future")
	}
	leaseContext, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	if err := m.write(lease); err != nil {
		m.mu.Unlock()
		cancel()
		_ = listener.Close()
		return Lease{}, err
	}
	m.leases[leaseID], m.listeners[leaseID], m.cancel[leaseID] = lease, listener, cancel
	m.mu.Unlock()
	if handler == nil {
		go m.serve(leaseContext, lease, listener)
	} else {
		go func() {
			server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
			go func() { <-leaseContext.Done(); _ = server.Close() }()
			_ = server.Serve(listener)
		}()
	}
	if !lease.ExpiresAt.IsZero() {
		go m.expire(leaseContext, lease)
	}
	if m.logger != nil {
		m.logger.Info("port exposed", "port_lease_id", lease.LeaseID, "sandbox_id", lease.SandboxID, "bind_address", lease.BindAddress, "host_port", lease.HostPort, "internal_port", lease.InternalPort)
	}
	return lease, nil
}

func (m *Manager) List() []Lease {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Lease, 0, len(m.leases))
	for _, lease := range m.leases {
		result = append(result, lease)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].LeaseID < result[j].LeaseID })
	return result
}

func (m *Manager) Close(leaseID string) error {
	m.mu.Lock()
	lease, exists := m.leases[leaseID]
	listener := m.listeners[leaseID]
	cancel := m.cancel[leaseID]
	if exists {
		delete(m.leases, leaseID)
		delete(m.listeners, leaseID)
		delete(m.cancel, leaseID)
	}
	m.mu.Unlock()
	if !exists {
		return nil
	}
	if cancel != nil {
		cancel()
	}
	var err error
	if listener != nil {
		err = listener.Close()
		if errors.Is(err, net.ErrClosed) {
			err = nil
		}
	}
	if removeErr := os.Remove(m.path(leaseID)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		err = errors.Join(err, removeErr)
		// Retain the identity without a live listener so a later lifecycle retry
		// can remove the durable record instead of losing cleanup ownership.
		m.mu.Lock()
		if _, replaced := m.leases[leaseID]; !replaced {
			m.leases[leaseID] = lease
		}
		m.mu.Unlock()
	}
	if m.logger != nil {
		m.logger.Info("port closed", "port_lease_id", lease.LeaseID, "sandbox_id", lease.SandboxID)
	}
	return err
}

func (m *Manager) CloseAll() error {
	leases := m.List()
	var joined error
	for _, lease := range leases {
		if err := m.Close(lease.LeaseID); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

// CloseForSandbox closes every active listener owned by sandboxID and removes
// its durable lease records. It is safe to call repeatedly during lifecycle
// cleanup.
func (m *Manager) CloseForSandbox(sandboxID string) error {
	if sandboxID == "" {
		return errors.New("sandbox ID is required")
	}
	leases := m.List()
	var joined error
	for _, lease := range leases {
		if lease.SandboxID != sandboxID {
			continue
		}
		if err := m.Close(lease.LeaseID); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

func (m *Manager) Restore(ctx context.Context) ([]Lease, error) {
	return m.RestoreFor(ctx, nil)
}

// RestoreFor rebinds durable leases accepted by allow. Rejected records are
// removed so a stale listener cannot reappear on a later restart.
func (m *Manager) RestoreFor(ctx context.Context, allow func(Lease) bool) ([]Lease, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil, err
	}
	var restored []Lease
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var prior Lease
		if err := readLease(filepath.Join(m.root, entry.Name()), &prior); err != nil {
			return restored, err
		}
		path := filepath.Join(m.root, entry.Name())
		if !prior.ExpiresAt.IsZero() && !prior.ExpiresAt.After(m.now()) {
			if err := removeRecord(path); err != nil {
				return restored, err
			}
			continue
		}
		if prior.Purpose == "debug" {
			if err := removeRecord(path); err != nil {
				return restored, err
			}
			continue
		}
		if allow != nil && !allow(prior) {
			if err := removeRecord(path); err != nil {
				return restored, err
			}
			if m.logger != nil {
				m.logger.Info("discarded unsafe port lease during restore", "port_lease_id", prior.LeaseID, "sandbox_id", prior.SandboxID, "purpose", prior.Purpose)
			}
			continue
		}
		if err := removeRecord(path); err != nil {
			return restored, err
		}
		lease, exposeErr := m.exposeIdentity(ctx, Request{SandboxID: prior.SandboxID, OwnerID: prior.OwnerID, SandboxIP: prior.SandboxIP, InternalPort: prior.InternalPort, TargetPort: prior.TargetPort, BindAddress: prior.BindAddress, HostPort: prior.HostPort, Protocol: prior.Protocol, Purpose: prior.Purpose, ExpiresAt: prior.ExpiresAt}, nil, prior.LeaseID, prior.CreatedAt)
		if exposeErr != nil {
			return restored, fmt.Errorf("restore lease %s: %w", prior.LeaseID, exposeErr)
		}
		restored = append(restored, lease)
	}
	return restored, nil
}

func removeRecord(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove port lease record %s: %w", filepath.Base(path), err)
	}
	return nil
}

func (m *Manager) serve(ctx context.Context, lease Lease, listener net.Listener) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil && m.logger != nil {
				m.logger.Error("port accept failed", "port_lease_id", lease.LeaseID, "error", err)
			}
			return
		}
		go m.proxy(ctx, lease, connection)
	}
}

func (m *Manager) proxy(ctx context.Context, lease Lease, client net.Conn) {
	defer client.Close()
	targetPort := lease.TargetPort
	if targetPort == 0 {
		targetPort = lease.InternalPort
	}
	target := net.JoinHostPort(lease.SandboxIP, fmt.Sprintf("%d", targetPort))
	upstream, err := m.dial(ctx, "tcp", target)
	if err != nil {
		if m.logger != nil {
			m.logger.Error("port dial failed", "port_lease_id", lease.LeaseID, "sandbox_id", lease.SandboxID, "error", err)
		}
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	copyStream := func(destination, source net.Conn) {
		_, _ = io.Copy(destination, source)
		if tcp, ok := destination.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}
	go copyStream(upstream, client)
	go copyStream(client, upstream)
	select {
	case <-ctx.Done():
	case <-done:
	}
}

func (m *Manager) expire(ctx context.Context, lease Lease) {
	timer := time.NewTimer(time.Until(lease.ExpiresAt))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
		_ = m.Close(lease.LeaseID)
	}
}

func (m *Manager) write(lease Lease) error {
	data, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(m.root, ".port-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
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
	return os.Rename(name, m.path(lease.LeaseID))
}

func (m *Manager) path(leaseID string) string { return filepath.Join(m.root, leaseID+".json") }

func safeLeaseID(value string) bool {
	if !strings.HasPrefix(value, "port-") {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-') {
			return false
		}
	}
	return true
}

func readLease(path string, lease *Lease) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(lease)
}
