package network

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"the8020/kernel/auth"
)

// SetHTTPSIngress connects native HTTP admission to the same published TLS state.
func (m *Manager) SetHTTPSIngress(https *HTTPS) {
	m.router.mu.Lock()
	m.router.https = https
	m.router.mu.Unlock()
}

func (h *HTTPS) admit(w http.ResponseWriter, r *http.Request) *http.Request {
	name, err := requestHostname(r.Host)
	if err != nil {
		http.Error(w, "Invalid hostname", http.StatusBadRequest)
		return nil
	}
	if h.serveChallenge(w, r, name) {
		return nil
	}
	state := h.current.Load()
	if state != nil && !state.permits(name) && !localRecovery(r, name) {
		http.Error(w, "Hostname is not allowed", http.StatusMisdirectedRequest)
		return nil
	}
	values := r.Header.Values("Client-Cert")
	r.Header.Del("Client-Cert")
	r.Header.Del("Client-Cert-Chain")
	if len(values) == 0 || state == nil || !state.trusts(r.RemoteAddr) {
		return r
	}
	if len(values) != 1 {
		http.Error(w, "Invalid client certificate header", http.StatusBadRequest)
		return nil
	}
	certificate, err := forwardedCertificate(values[0])
	if err != nil {
		http.Error(w, "Invalid client certificate header", http.StatusBadRequest)
		return nil
	}
	// RFC 9440 requires responses selected by this credential to vary by it.
	w.Header().Add("Vary", "Client-Cert")
	return r.WithContext(auth.WithClientCertificate(r.Context(), certificate))
}

func requestHostname(host string) (string, error) {
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		return hostname(host[1 : len(host)-1])
	}
	if name, _, err := net.SplitHostPort(host); err == nil {
		return hostname(name)
	}
	return hostname(host)
}

func localRecovery(r *http.Request, name string) bool {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil || !peer.Addr().IsLoopback() || r.TLS != nil {
		return false
	}
	address, _ := netip.ParseAddr(name)
	return name == "localhost" || address.IsLoopback()
}

func (s *httpsSnapshot) trusts(remote string) bool {
	peer, err := netip.ParseAddrPort(remote)
	if err != nil {
		return false
	}
	for _, prefix := range s.proxies {
		if prefix.Contains(peer.Addr().Unmap()) {
			return true
		}
	}
	return false
}

func forwardedCertificate(value string) (*x509.Certificate, error) {
	value = strings.Trim(value, " \t")
	if len(value) < 3 || len(value) > base64.StdEncoding.EncodedLen(65536)+2 || value[0] != ':' || value[len(value)-1] != ':' || strings.ContainsAny(value, "\r\n") {
		return nil, errors.New("invalid certificate byte sequence")
	}
	encoded := value[1 : len(value)-1]
	// Structured Fields accepts omitted padding and non-zero pad bits.
	encoded += strings.Repeat("=", (4-len(encoded)%4)%4)
	der, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(der) > 65536 {
		return nil, errors.New("invalid certificate encoding")
	}
	return x509.ParseCertificate(der)
}

func (h *HTTPS) serveChallenge(w http.ResponseWriter, r *http.Request, name string) bool {
	kind, token := "", ""
	switch {
	case strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/"):
		kind, token = "acme", strings.TrimPrefix(r.URL.Path, "/.well-known/acme-challenge/")
	case strings.HasPrefix(r.URL.Path, "/.well-known/the8020-https/"):
		kind, token = "probe", strings.TrimPrefix(r.URL.Path, "/.well-known/the8020-https/")
	}
	if kind == "" {
		return false
	}
	if r.Method != http.MethodGet || len(token) < 16 || len(token) > 256 || h.challenge == nil {
		http.NotFound(w, r)
		return true
	}
	for _, c := range token {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			http.NotFound(w, r)
			return true
		}
	}
	select {
	case h.challengeSlots <- struct{}{}:
		defer func() { <-h.challengeSlots }()
	default:
		http.Error(w, "Challenge capacity reached", http.StatusServiceUnavailable)
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	response, err := h.challenge(ctx, name, kind, token, r.URL.RawQuery == "local=1")
	if err != nil {
		http.Error(w, "Challenge lookup unavailable", http.StatusServiceUnavailable)
		return true
	}
	if len(response) == 0 || len(response) > 2048 {
		http.NotFound(w, r)
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(response))
	return true
}
