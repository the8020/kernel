package network

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/idna"
)

// HTTPSState is public package output. Private keys are resolved separately.
type HTTPSState struct {
	Revision      uint64 `json:"revision"`
	Configuration struct {
		Enabled        bool     `json:"enabled"`
		LetsEncrypt    bool     `json:"letsEncrypt"`
		AllowedDomains []string `json:"allowedDomains"`
		AutomaticNames []string `json:"automaticNames"`
		IPAddress      string   `json:"ipAddress"`
		TrustedProxies []string `json:"trustedProxies"`
	} `json:"configuration"`
	Certificates []ServerCertificate `json:"certificates"`
}

type ServerCertificate struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Chain     string `json:"chain"`
	KeySecret string `json:"keySecret"`
}

type certificateVersion struct {
	record      ServerCertificate
	certificate *tls.Certificate
}

type httpsSnapshot struct {
	revision           uint64
	enabled, automatic bool
	defaultName        string
	allowed            map[string]bool
	automaticNames     map[string]bool
	proxies            []netip.Prefix
	versions           map[string]certificateVersion
	names              map[string]*tls.Certificate
}

// HTTPS keeps transport state only; callbacks use the owning package programs.
type HTTPS struct {
	current        atomic.Pointer[httpsSnapshot]
	desired        atomic.Uint64
	failure        atomic.Pointer[string]
	ctx            context.Context
	cancel         context.CancelFunc
	issue          func(context.Context, string) error
	challenge      func(context.Context, string, string, string, bool) (string, error)
	mu             sync.Mutex
	pending        map[string]*certificateRequest
	waiters        int
	work           sync.WaitGroup
	challengeSlots chan struct{}
}

type certificateRequest struct {
	done chan struct{}
	err  error
}

func NewHTTPS(ctx context.Context, issue func(context.Context, string) error, challenge func(context.Context, string, string, string, bool) (string, error)) *HTTPS {
	ctx, cancel := context.WithCancel(ctx)
	return &HTTPS{ctx: ctx, cancel: cancel, issue: issue, challenge: challenge, pending: map[string]*certificateRequest{}, challengeSlots: make(chan struct{}, 16)}
}

func (h *HTTPS) Close() {
	h.mu.Lock()
	h.cancel()
	h.mu.Unlock()
	h.work.Wait()
}

// Prepare parses outside publication locks and reuses immutable loaded versions.
// The returned closure must run only after the listener has been prepared.
func (h *HTTPS) Prepare(ctx context.Context, state HTTPSState, key func(context.Context, string) (string, error)) (func(), error) {
	h.desired.Store(state.Revision)
	if len(state.Certificates) > 10000 || len(state.Configuration.AllowedDomains) > 1000 || len(state.Configuration.TrustedProxies) > 128 || len(state.Configuration.AutomaticNames) > 16 {
		return nil, errors.New("HTTPS publication exceeds transport bounds")
	}
	next := &httpsSnapshot{revision: state.Revision, enabled: state.Configuration.Enabled, automatic: state.Configuration.LetsEncrypt,
		allowed: map[string]bool{}, automaticNames: map[string]bool{}, versions: map[string]certificateVersion{}, names: map[string]*tls.Certificate{}, defaultName: "127.0.0.1"}
	for _, raw := range state.Configuration.AutomaticNames {
		name, err := hostname(raw)
		if err != nil {
			return nil, errors.New("invalid automatic certificate name")
		}
		next.automaticNames[name] = true
	}
	for _, pattern := range state.Configuration.AllowedDomains {
		normalized, err := certificatePattern(pattern)
		if err != nil || normalized != pattern {
			return nil, errors.New("invalid HTTPS hostname policy")
		}
		next.allowed[pattern] = true
	}
	if state.Configuration.IPAddress != "" {
		address, err := netip.ParseAddr(state.Configuration.IPAddress)
		if err != nil || address.Zone() != "" {
			return nil, errors.New("invalid HTTPS default address")
		}
		next.defaultName = address.Unmap().String()
	}
	for _, value := range state.Configuration.TrustedProxies {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			if address, addressErr := netip.ParseAddr(value); addressErr == nil && address.Zone() == "" {
				address = address.Unmap()
				prefix, err = netip.PrefixFrom(address, address.BitLen()), nil
			}
		}
		if err != nil {
			return nil, errors.New("invalid trusted proxy network")
		}
		if prefix.Addr().Is4In6() {
			if prefix.Bits() < 96 {
				return nil, errors.New("invalid mapped IPv4 proxy network")
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		next.proxies = append(next.proxies, prefix.Masked())
	}
	previous := h.current.Load()
	encodedBytes := 0
	for _, record := range state.Certificates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if record.ID == "" || record.KeySecret == "" || len(record.Chain) > 256*1024 {
			return nil, errors.New("invalid server certificate record")
		}
		encodedBytes += len(record.Chain)
		if encodedBytes > 32*1024*1024 {
			return nil, errors.New("HTTPS certificate publication exceeds 32 MiB")
		}
		name, err := certificatePattern(record.Name)
		if err != nil || name == "*" || name != record.Name {
			return nil, errors.New("invalid server certificate name")
		}
		if _, exists := next.versions[record.ID]; exists {
			return nil, errors.New("duplicate certificate version")
		}
		var loaded *tls.Certificate
		if previous != nil {
			if cached, found := previous.versions[record.ID]; found {
				if cached.record != record {
					return nil, errors.New("immutable certificate version changed")
				}
				loaded = cached.certificate
			}
		}
		if loaded == nil {
			value, err := key(ctx, record.KeySecret)
			if err != nil {
				return nil, fmt.Errorf("load certificate %s key: %w", name, err)
			}
			if len(value) > 64*1024 {
				return nil, errors.New("server private key exceeds 64 KiB")
			}
			pair, err := tls.X509KeyPair([]byte(record.Chain), []byte(value))
			if err != nil {
				return nil, fmt.Errorf("invalid certificate/key pair for %s", name)
			}
			if len(pair.Certificate) > 16 {
				return nil, errors.New("server chain exceeds 16 certificates")
			}
			if pair.Leaf == nil {
				pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
			}
			if err != nil {
				return nil, fmt.Errorf("invalid certificate for %s", name)
			}
			loaded = &pair
		}
		leaf := loaded.Leaf
		if len(leaf.DNSNames)+len(leaf.IPAddresses) > 100 {
			return nil, errors.New("certificate exceeds 100 covered names")
		}
		if leaf.IsCA || len(leaf.UnhandledCriticalExtensions) != 0 || time.Now().Before(leaf.NotBefore) ||
			leaf.KeyUsage != 0 && leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 ||
			(len(leaf.ExtKeyUsage) != 0 || len(leaf.UnknownExtKeyUsage) != 0) && !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) && !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageAny) {
			return nil, fmt.Errorf("invalid server certificate usage for %s", name)
		}
		switch loaded.PrivateKey.(type) {
		case *rsa.PrivateKey, *ecdsa.PrivateKey:
		default:
			return nil, errors.New("server certificates require RSA or ECDSA")
		}
		verifyName := name
		if strings.HasPrefix(name, "*.") {
			if !slices.Contains(leaf.DNSNames, name) {
				return nil, fmt.Errorf("certificate does not contain wildcard %s", name)
			}
			verifyName = "wildcard." + name[2:]
		}
		if err := leaf.VerifyHostname(verifyName); err != nil {
			return nil, fmt.Errorf("certificate does not cover %s", name)
		}
		if _, exists := next.names[name]; exists {
			return nil, errors.New("duplicate certificate name")
		}
		next.versions[record.ID] = certificateVersion{record, loaded}
		next.names[name] = loaded
	}
	owners := map[string]string{}
	for _, record := range state.Certificates {
		certificate := next.versions[record.ID].certificate
		names := slices.Clone(certificate.Leaf.DNSNames)
		for _, address := range certificate.Leaf.IPAddresses {
			names = append(names, address.String())
		}
		for _, raw := range names {
			name, err := certificatePattern(raw)
			if err != nil || name == "*" {
				return nil, errors.New("invalid certificate SAN")
			}
			owner, alias := owners[name]
			if _, exists := next.names[name]; exists && (!alias || owner < record.Name) {
				continue
			}
			next.names[name], owners[name] = certificate, record.Name
		}
	}
	return func() { h.current.Store(next) }, nil
}

type HTTPSStatus struct {
	DesiredRevision uint64 `json:"desired_revision"`
	AppliedRevision uint64 `json:"applied_revision"`
	Ready           bool   `json:"ready"`
	Enabled         bool   `json:"enabled"`
	Certificates    int    `json:"certificates"`
	Error           string `json:"error,omitempty"`
}

func (h *HTTPS) Status() HTTPSStatus {
	status := HTTPSStatus{DesiredRevision: h.desired.Load()}
	if state := h.current.Load(); state != nil {
		status.AppliedRevision, status.Ready = state.revision, true
		status.Enabled, status.Certificates = state.enabled, len(state.versions)
	}
	if message := h.failure.Load(); message != nil {
		status.Error = *message
	}
	return status
}

func (h *HTTPS) SetFailure(err error) {
	if err == nil {
		h.failure.Store(nil)
		return
	}
	message := err.Error()
	if len(message) > 2048 {
		message = message[:2048]
	}
	h.failure.Store(&message)
}

func (h *HTTPS) Clear() {
	h.current.Store(nil)
	h.desired.Store(0)
	h.failure.Store(nil)
}

func (m *Manager) HTTPSStatus() HTTPSStatus {
	m.router.mu.RLock()
	https := m.router.https
	m.router.mu.RUnlock()
	if https == nil {
		return HTTPSStatus{}
	}
	return https.Status()
}

func (h *HTTPS) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequestClientCert, GetCertificate: h.getCertificate,
		VerifyConnection: func(connection tls.ConnectionState) error {
			_, _, err := h.selectedName(connection.ServerName)
			return err
		},
	}
}

func (h *HTTPS) selectedName(raw string) (string, *httpsSnapshot, error) {
	state := h.current.Load()
	if state == nil || !state.enabled {
		return "", state, errors.New("HTTPS is unavailable")
	}
	if raw == "" {
		raw = state.defaultName
	}
	name, err := hostname(raw)
	if err != nil || !state.permits(name) {
		return "", state, errors.New("HTTPS hostname is not allowed")
	}
	return name, state, nil
}

func (s *httpsSnapshot) permits(name string) bool {
	return s.allowed["*"] || s.allowed[name] || s.allowed[wildcardName(name)]
}

func wildcardName(name string) string {
	if net.ParseIP(name) != nil {
		return ""
	}
	_, suffix, found := strings.Cut(name, ".")
	if !found {
		return ""
	}
	return "*." + suffix
}

func (s *httpsSnapshot) certificate(name string) *tls.Certificate {
	certificate := s.names[name]
	if certificate == nil {
		certificate = s.names[wildcardName(name)]
	}
	if certificate != nil && !time.Now().Before(certificate.Leaf.NotAfter) {
		return nil
	}
	return certificate
}

func (h *HTTPS) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name, state, err := h.selectedName(hello.ServerName)
	if err != nil {
		return nil, err
	}
	if certificate := state.certificate(name); certificate != nil {
		return certificate, nil
	}
	if !state.automatic && !state.automaticNames[name] || h.issue == nil {
		return nil, errors.New("no certificate is available for this hostname")
	}
	h.mu.Lock()
	if h.ctx.Err() != nil || h.waiters >= 128 {
		h.mu.Unlock()
		return nil, errors.New("certificate admission is unavailable")
	}
	request := h.pending[name]
	if request == nil {
		if len(h.pending) >= 32 {
			h.mu.Unlock()
			return nil, errors.New("certificate admission capacity reached")
		}
		request = &certificateRequest{done: make(chan struct{})}
		h.pending[name] = request
		h.work.Add(1)
		go func() {
			defer h.work.Done()
			ctx, cancel := context.WithTimeout(h.ctx, 4*time.Minute+15*time.Second)
			defer cancel()
			err := h.issue(ctx, name)
			h.mu.Lock()
			request.err = err
			delete(h.pending, name)
			close(request.done)
			h.mu.Unlock()
		}()
	}
	h.waiters++
	h.mu.Unlock()
	defer func() { h.mu.Lock(); h.waiters--; h.mu.Unlock() }()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case <-hello.Context().Done():
		return nil, hello.Context().Err()
	case <-h.ctx.Done():
		return nil, h.ctx.Err()
	case <-timer.C:
		return nil, errors.New("certificate is being prepared; retry shortly")
	case <-request.done:
		if request.err != nil {
			return nil, errors.New("certificate issuance failed; inspect HTTPS diagnostics")
		}
		_, current, err := h.selectedName(name)
		if err != nil {
			return nil, err
		}
		if certificate := current.certificate(name); certificate != nil {
			return certificate, nil
		}
		return nil, errors.New("certificate is not yet available; retry shortly")
	}
}

func hostname(value string) (string, error) {
	value = strings.ToLower(strings.TrimSuffix(value, "."))
	if address, err := netip.ParseAddr(value); err == nil && address.Zone() == "" {
		return address.Unmap().String(), nil
	}
	value, err := idna.Lookup.ToASCII(value)
	if err != nil || len(value) == 0 || len(value) > 253 {
		return "", errors.New("invalid hostname")
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid hostname")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", errors.New("invalid hostname")
			}
		}
	}
	return value, nil
}

func certificatePattern(value string) (string, error) {
	if value == "*" {
		return value, nil
	}
	if strings.HasPrefix(value, "*.") {
		name, err := hostname(value[2:])
		if err != nil || net.ParseIP(name) != nil || !strings.Contains(name, ".") {
			return "", errors.New("invalid wildcard hostname")
		}
		return "*." + name, nil
	}
	return hostname(value)
}
