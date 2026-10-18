package network

import (
	"crypto/tls"
	"errors"
	"sync"
	"time"

	"the8020/kernel/settings"
)

// TLSPort is zero while native HTTPS is disabled.
func (m *Manager) TLSPort() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.secure == nil {
		return 0
	}
	return m.secure.port
}

// PrepareTLS reserves a listener and an immutable TLS configuration. Complete
// every preparation with Commit or Discard before preparing another TLS change.
// Port zero disables HTTPS. Certificate policy and loading belong to the caller.
func (m *Manager) PrepareTLS(port int, config *tls.Config) (settings.Prepared, error) {
	var candidateConfig *tls.Config
	if port != 0 {
		if config == nil || (len(config.Certificates) == 0 && config.GetCertificate == nil) {
			return nil, errors.New("HTTPS requires a certificate provider")
		}
		candidateConfig = config.Clone()
		candidateConfig.MinVersion = max(candidateConfig.MinVersion, tls.VersionTLS12)
		candidateConfig.ClientAuth = tls.RequestClientCert
		candidateConfig.NextProtos = []string{"h2", "http/1.1"}
	}
	m.tlsUpdate.Lock()
	m.mu.RLock()
	current, closed := m.secure, m.closed
	m.mu.RUnlock()
	if closed {
		m.tlsUpdate.Unlock()
		return nil, errors.New("network is closed")
	}
	prepared := &preparedTLS{manager: m, config: candidateConfig}
	if current != nil && current.port == port {
		prepared.candidate = current
		return prepared, nil
	}
	if port != 0 {
		candidate, err := prepareListener(port, m.router)
		if err != nil {
			m.tlsUpdate.Unlock()
			return nil, err
		}
		candidate.server.ReadHeaderTimeout = 30 * time.Second
		candidate.server.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
				config := m.tlsConfig.Load()
				if config == nil {
					return nil, errors.New("HTTPS is disabled")
				}
				return config, nil
			},
		}
		prepared.candidate, prepared.replacement = candidate, true
	}
	return prepared, nil
}

type preparedTLS struct {
	manager     *Manager
	candidate   *running
	config      *tls.Config
	replacement bool
	once        sync.Once
}

func (p *preparedTLS) Commit() {
	p.once.Do(func() {
		p.manager.tlsConfig.Store(p.config)
		if p.replacement {
			go func() { _ = p.candidate.server.ServeTLS(p.candidate.listener, "", "") }()
		}
		p.manager.mu.Lock()
		old := p.manager.secure
		p.manager.secure = p.candidate
		p.manager.mu.Unlock()
		p.manager.tlsUpdate.Unlock()
		if old != nil && old != p.candidate {
			shutdown(old)
		}
	})
}

func (p *preparedTLS) Discard() {
	p.once.Do(func() {
		if p.replacement {
			_ = p.candidate.listener.Close()
		}
		p.manager.tlsUpdate.Unlock()
	})
}
