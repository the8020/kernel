package auth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"time"
)

// ForwardingTLSConfig authenticates native peers using a purpose-specific key.
// Package byte/JWT signing never uses this key. TLS protects streaming requests,
// responses and WebSocket frames without buffering or replaying application work.
func (s *Signer) ForwardingTLSConfig() *tls.Config {
	certificate := func() (*tls.Certificate, error) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.forwarding, nil
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"http/1.1"},
		ClientAuth: tls.RequireAnyClientCert,
		// Trust the current derived public key, not DNS names or public CAs.
		InsecureSkipVerify:     true,
		VerifyConnection:       s.VerifyForwardingPeer,
		SessionTicketsDisabled: true,
		GetCertificate:         func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return certificate() },
		GetClientCertificate:   func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return certificate() },
	}
}

// VerifyForwardingPeer also runs on each received HTTP request so replacement
// rejects old credentials on pooled connections. Already-running streams finish.
func (s *Signer) VerifyForwardingPeer(state tls.ConnectionState) error {
	if len(state.PeerCertificates) != 1 {
		return errors.New("native node certificate required")
	}
	key, ok := state.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !ok || !bytes.Equal(key, s.forwarding.Leaf.PublicKey.(ed25519.PublicKey)) {
		return errors.New("native node signing key mismatch")
	}
	return nil
}

func forwardingCertificate(key ed25519.PrivateKey) (*tls.Certificate, error) {
	// The certificate is a TLS envelope. Explicit root replacement controls trust;
	// no independently provisioned certificate, renewal process or CA is required.
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "the8020-node-forwarding"},
		NotBefore:    time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}
