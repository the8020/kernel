// Package network owns the minimal main HTTP listener and runtime replacement.
package network

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"the8020/kernel/settings"
)

// ErrPortUnavailable identifies failure to prepare a replacement listener.
var ErrPortUnavailable = errors.New("main HTTP port is unavailable")

type running struct {
	listener net.Listener
	server   *http.Server
	port     int
}

// Manager owns the main HTTP listener and optional additional HTTP/TLS listeners.
type Manager struct {
	mu         sync.RWMutex
	active     *running
	additional *running
	secure     *running
	closed     bool
	tlsUpdate  sync.Mutex
	tlsConfig  atomic.Pointer[tls.Config]
	router     *routeTable
}

type routeTable struct {
	mu              sync.RWMutex
	routes          map[string]http.Handler
	serviceBoundary http.Handler
	rootAlias       string
	available       bool
	unavailable     string
	https           *HTTPS
}

// New binds and starts the initial listener.
func New(port int, rootAlias string) (*Manager, error) {
	rootTarget, err := rootAliasTarget(rootAlias)
	if err != nil {
		return nil, err
	}
	router := &routeTable{routes: map[string]http.Handler{}, rootAlias: rootTarget, available: true}
	candidate, err := prepareListener(port, router)
	if err != nil {
		return nil, err
	}
	manager := &Manager{active: candidate, router: router}
	serve(candidate)
	return manager, nil
}

func prepareListener(port int, handler http.Handler) (*running, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("%w: invalid port %d", ErrPortUnavailable, port)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPortUnavailable, err)
	}
	return &running{listener: listener, server: &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}, port: port}, nil
}

func serve(value *running) { go func() { _ = value.server.Serve(value.listener) }() }

// Port returns the currently active main HTTP port.
func (m *Manager) Port() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.active == nil {
		return 0
	}
	return m.active.port
}

// SetAvailable gates the complete public plane while shared database state is
// unavailable. The listener remains bound so readiness and recovery are
// observable without restarting the kernel.
func (m *Manager) SetAvailable(available bool, reason string) {
	if reason == "" {
		reason = "service plane is unavailable"
	}
	m.router.mu.Lock()
	m.router.available = available
	m.router.unavailable = reason
	m.router.mu.Unlock()
}

// RegisterRoute adds a longest-prefix HTTP route to the active and future listeners.
func (m *Manager) RegisterRoute(prefix string, handler http.Handler) error {
	if handler == nil || !strings.HasPrefix(prefix, "/") || prefix == "/" || prefix == "/health" || strings.Contains(prefix, "//") {
		return errors.New("canonical non-reserved path prefix and handler are required")
	}
	prefix = strings.TrimSuffix(prefix, "/")
	m.router.mu.Lock()
	defer m.router.mu.Unlock()
	if _, exists := m.router.routes[prefix]; exists {
		return fmt.Errorf("route prefix %s is already registered", prefix)
	}
	m.router.routes[prefix] = handler
	return nil
}

// RegisterServiceBoundary installs the single filesystem-derived service
// resolver used when no explicit infrastructure route matches.
func (m *Manager) RegisterServiceBoundary(handler http.Handler) error {
	if handler == nil {
		return errors.New("service boundary handler is required")
	}
	m.router.mu.Lock()
	defer m.router.mu.Unlock()
	if m.router.serviceBoundary != nil {
		return errors.New("service boundary handler is already registered")
	}
	m.router.serviceBoundary = handler
	return nil
}

// UnregisterRoute removes a dynamic route without affecting the listener.
func (m *Manager) UnregisterRoute(prefix string) {
	prefix = strings.TrimSuffix(prefix, "/")
	m.router.mu.Lock()
	delete(m.router.routes, prefix)
	m.router.mu.Unlock()
}

func (r *routeTable) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	r.mu.RLock()
	available, unavailable := r.available, r.unavailable
	https := r.https
	r.mu.RUnlock()
	if !available {
		http.Error(writer, unavailable, http.StatusServiceUnavailable)
		return
	}
	if https != nil {
		request = https.admit(writer, request)
		if request == nil {
			return
		}
	}
	if request.URL.Path == "/health" {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("OK"))
		return
	}
	if request.URL.Path == "/" {
		target := r.rootAlias
		if request.URL.RawQuery != "" {
			target += "?" + request.URL.RawQuery
		}
		http.Redirect(writer, request, target, http.StatusTemporaryRedirect)
		return
	}
	r.mu.RLock()
	best := ""
	var handler http.Handler
	for prefix, candidate := range r.routes {
		if (request.URL.Path == prefix || strings.HasPrefix(request.URL.Path, prefix+"/")) && len(prefix) > len(best) {
			best, handler = prefix, candidate
		}
	}
	serviceBoundary := r.serviceBoundary
	r.mu.RUnlock()
	if handler == nil {
		if serviceBoundary != nil {
			serviceBoundary.ServeHTTP(writer, request)
		} else {
			http.NotFound(writer, request)
		}
		return
	}
	handler.ServeHTTP(writer, request)
}

func rootAliasTarget(value string) (string, error) {
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\?#") || strings.Contains(value, "//") {
		return "", errors.New("network root alias must be a non-empty relative path")
	}
	trimmed := strings.TrimSuffix(value, "/")
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", errors.New("network root alias must be a canonical relative path")
		}
		for _, character := range segment {
			if !((character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_' || character == '-' || character == '.') {
				return "", errors.New("network root alias contains an unsupported path character")
			}
		}
	}
	return "/" + value, nil
}

// Prepare binds a replacement while the current listener remains active.
func (m *Manager) Prepare(_ context.Context, values settings.Values) (settings.Prepared, error) {
	raw, ok := values["network.main_port"].(int64)
	if !ok {
		return nil, fmt.Errorf("network.main_port is not an integer")
	}
	port := int(raw)
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("%w: invalid port %d", ErrPortUnavailable, port)
	}
	return m.prepareHTTP(port, &m.active)
}

// PrepareAdditionalHTTP publishes another port on the same router; zero disables it.
func (m *Manager) PrepareAdditionalHTTP(port int) (settings.Prepared, error) {
	return m.prepareHTTP(port, &m.additional)
}

func (m *Manager) prepareHTTP(port int, slot **running) (settings.Prepared, error) {
	m.mu.RLock()
	current, closed := *slot, m.closed
	same := current == nil && port == 0 || current != nil && current.port == port
	m.mu.RUnlock()
	if closed {
		return nil, errors.New("HTTP listeners are closed")
	}
	if same {
		return noopPrepared{}, nil
	}
	var candidate *running
	if port != 0 {
		var err error
		candidate, err = prepareListener(port, m.router)
		if err != nil {
			return nil, err
		}
	}
	return &prepared{manager: m, slot: slot, candidate: candidate}, nil
}

type prepared struct {
	manager   *Manager
	slot      **running
	candidate *running
	once      sync.Once
}

func (p *prepared) Commit() {
	p.once.Do(func() {
		p.manager.mu.Lock()
		if p.manager.closed {
			p.manager.mu.Unlock()
			if p.candidate != nil {
				_ = p.candidate.listener.Close()
			}
			return
		}
		old := *p.slot
		*p.slot = p.candidate
		if p.candidate != nil {
			serve(p.candidate)
		}
		p.manager.mu.Unlock()
		if old != nil {
			shutdown(old)
		}
	})
}
func (p *prepared) Discard() {
	p.once.Do(func() {
		if p.candidate != nil {
			_ = p.candidate.listener.Close()
		}
	})
}

type noopPrepared struct{}

func (noopPrepared) Commit()  {}
func (noopPrepared) Discard() {}

func shutdown(value *running) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = value.server.Shutdown(ctx)
	_ = value.listener.Close()
}

// Close gracefully releases the active listener.
func (m *Manager) Close() {
	m.router.mu.RLock()
	https := m.router.https
	m.router.mu.RUnlock()
	if https != nil {
		https.Close()
	}
	m.tlsUpdate.Lock()
	m.mu.Lock()
	active, additional, secure := m.active, m.additional, m.secure
	m.active = nil
	m.additional = nil
	m.secure = nil
	m.closed = true
	m.mu.Unlock()
	m.tlsUpdate.Unlock()
	if secure != nil {
		shutdown(secure)
	}
	if active != nil {
		shutdown(active)
	}
	if additional != nil {
		shutdown(additional)
	}
}
