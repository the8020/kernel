// Package debugging maps Deno inspector targets and manages temporary loopback leases.
package debugging

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"the8020/kernel/ports"
	"the8020/kernel/sandbox/model"
)

const InspectorPort = 9229

type PortManager interface {
	ExposeHTTP(context.Context, ports.Request, http.Handler) (ports.Lease, error)
	List() []ports.Lease
	Close(string) error
}

type Config struct {
	Ports             PortManager
	HTTPClient        *http.Client
	InspectorEndpoint func(model.SandboxSpec) (string, error)
	Enabled           bool
	BindAddress       string
	DefaultDuration   time.Duration
	MaximumDuration   time.Duration
}

type Manager struct {
	ports             PortManager
	httpClient        *http.Client
	inspectorEndpoint func(model.SandboxSpec) (string, error)
	enabled           bool
	bindAddress       string
	defaultDuration   time.Duration
	maximumDuration   time.Duration
}

type Target struct {
	ID                string `json:"id"`
	Type              string `json:"type"`
	Title             string `json:"title"`
	Description       string `json:"description,omitempty"`
	URL               string `json:"url,omitempty"`
	WebSocketDebugger string `json:"websocket_debugger_url,omitempty"`
	ExecutionID       string `json:"execution_id,omitempty"`
}

type Lease struct {
	PortLease      ports.Lease `json:"port_lease"`
	AccessToken    string      `json:"access_token"`
	Targets        []Target    `json:"targets"`
	DiscoveryURL   string      `json:"discovery_url"`
	ConnectionHint string      `json:"connection_instructions"`
}

func New(config Config) (*Manager, error) {
	if config.Ports == nil {
		return nil, errors.New("port manager is required")
	}
	if config.HTTPClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		config.HTTPClient = &http.Client{Transport: transport}
	}
	if config.InspectorEndpoint == nil {
		config.InspectorEndpoint = func(spec model.SandboxSpec) (string, error) {
			if net.ParseIP(spec.Network.SandboxIP) == nil {
				return "", errors.New("sandbox IP is required")
			}
			return "http://" + net.JoinHostPort(spec.Network.SandboxIP, strconv.Itoa(spec.Network.InspectorEndpointPort())), nil
		}
	}
	if config.DefaultDuration <= 0 {
		config.DefaultDuration = 15 * time.Minute
	}
	if config.MaximumDuration <= 0 {
		config.MaximumDuration = time.Hour
	}
	if config.DefaultDuration > config.MaximumDuration {
		return nil, errors.New("default debug duration exceeds maximum")
	}
	if config.BindAddress == "" {
		config.BindAddress = "127.0.0.1"
	}
	bindIP := net.ParseIP(config.BindAddress)
	if bindIP == nil || !bindIP.IsLoopback() {
		return nil, errors.New("debug bind address must be a loopback IP")
	}
	return &Manager{ports: config.Ports, httpClient: config.HTTPClient, inspectorEndpoint: config.InspectorEndpoint, enabled: config.Enabled, bindAddress: config.BindAddress, defaultDuration: config.DefaultDuration, maximumDuration: config.MaximumDuration}, nil
}

func (m *Manager) Targets(ctx context.Context, spec model.SandboxSpec) ([]Target, error) {
	endpoint, err := m.inspectorEndpoint(spec)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/json/list", nil)
	if err != nil {
		return nil, err
	}
	response, err := m.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		return nil, fmt.Errorf("inspector returned %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	var raw []struct {
		ID                string `json:"id"`
		Type              string `json:"type"`
		Title             string `json:"title"`
		Description       string `json:"description"`
		URL               string `json:"url"`
		WebSocketDebugger string `json:"webSocketDebuggerUrl"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode inspector targets: %w", err)
	}
	result := make([]Target, 0, len(raw))
	for _, item := range raw {
		if item.ID == "" || item.Title == "" {
			return nil, errors.New("inspector target identity and title are required")
		}
		result = append(result, Target{ID: item.ID, Type: item.Type, Title: item.Title, Description: item.Description, URL: item.URL, WebSocketDebugger: item.WebSocketDebugger, ExecutionID: executionID(item.Title)})
	}
	return result, nil
}

func (m *Manager) Open(ctx context.Context, spec model.SandboxSpec, duration time.Duration) (Lease, error) {
	if !m.enabled {
		return Lease{}, errors.New("debug leases are disabled by sandbox.debug.enabled")
	}
	if duration == 0 {
		duration = m.defaultDuration
	}
	if duration <= 0 || duration > m.maximumDuration {
		return Lease{}, errors.New("debug lease duration is outside allowed range")
	}
	endpoint, err := m.inspectorEndpoint(spec)
	if err != nil {
		return Lease{}, err
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		return Lease{}, err
	}
	token, err := accessToken()
	if err != nil {
		return Lease{}, err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		provided := request.URL.Query().Get("token")
		if provided == "" {
			provided = strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		}
		if len(provided) != len(token) || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		query := request.URL.Query()
		query.Del("token")
		request.URL.RawQuery = query.Encode()
		request.Header.Del("Authorization")
		proxy.ServeHTTP(writer, request)
	})
	portLease, err := m.ports.ExposeHTTP(ctx, ports.Request{SandboxID: spec.SandboxID, OwnerID: spec.RuntimeGroupID, SandboxIP: spec.Network.SandboxIP, InternalPort: InspectorPort, TargetPort: spec.Network.InspectorEndpointPort(), BindAddress: m.bindAddress, Purpose: "debug", ExpiresAt: time.Now().UTC().Add(duration)}, handler)
	if err != nil {
		return Lease{}, err
	}
	targets, err := m.Targets(ctx, spec)
	if err != nil {
		_ = m.ports.Close(portLease.LeaseID)
		return Lease{}, err
	}
	local := net.JoinHostPort(portLease.BindAddress, strconv.Itoa(portLease.HostPort))
	for index := range targets {
		if targets[index].WebSocketDebugger != "" {
			targets[index].WebSocketDebugger = localDebugURL(targets[index].WebSocketDebugger, "ws", local, token)
		}
		if targets[index].URL != "" {
			targets[index].URL = localDebugURL(targets[index].URL, "http", local, token)
		}
	}
	discovery := "http://" + local + "/json/list?token=" + url.QueryEscape(token)
	return Lease{PortLease: portLease, AccessToken: token, Targets: targets, DiscoveryURL: discovery, ConnectionHint: "Use the discovery URL or a returned WebSocket debugger URL before the lease expires."}, nil
}

func (m *Manager) List() []ports.Lease {
	all := m.ports.List()
	result := make([]ports.Lease, 0, len(all))
	for _, lease := range all {
		if lease.Purpose == "debug" {
			result = append(result, lease)
		}
	}
	return result
}
func (m *Manager) Close(leaseID string) error { return m.ports.Close(leaseID) }

func executionID(title string) string {
	parts := strings.Split(title, ":")
	if len(parts) >= 4 {
		return parts[len(parts)-2]
	}
	return ""
}

func accessToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func localDebugURL(raw, scheme, host, token string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	parsed.Scheme, parsed.Host = scheme, host
	query := parsed.Query()
	query.Set("token", token)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}
