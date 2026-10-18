package auth

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientCertificateEvidenceAndUsage(t *testing.T) {
	certificate := &x509.Certificate{Raw: []byte("certificate DER"), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	want := sha256.Sum256(certificate.Raw)
	fingerprint, err := ClientCertificateFingerprint(certificate)
	if err != nil || fingerprint != hex.EncodeToString(want[:]) {
		t.Fatalf("fingerprint=%q, error=%v", fingerprint, err)
	}
	request := httptest.NewRequest("GET", "http://example.test", nil)
	request.Header.Set("Client-Cert", "forged")
	request.Header.Set("the8020-internal-client-certificate", "forged")
	if ClientCertificate(request) != nil {
		t.Fatal("untrusted headers established certificate evidence")
	}
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}}
	if ClientCertificate(request) != nil {
		t.Fatal("incomplete handshake established certificate evidence")
	}
	request.TLS.HandshakeComplete = true
	if ClientCertificate(request) != certificate {
		t.Fatal("TLS evidence was lost")
	}
	request.TLS = nil
	if ClientCertificate(request.WithContext(WithClientCertificate(request.Context(), certificate))) != certificate {
		t.Fatal("trusted peer evidence was lost")
	}
	for name, change := range map[string]func(*x509.Certificate){
		"expired":     func(c *x509.Certificate) { c.NotAfter = time.Now().Add(-time.Second) },
		"future":      func(c *x509.Certificate) { c.NotBefore = time.Now().Add(time.Hour) },
		"CA":          func(c *x509.Certificate) { c.IsCA = true },
		"server only": func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth} },
		"unknown usage": func(c *x509.Certificate) {
			c.ExtKeyUsage = nil
			c.UnknownExtKeyUsage = []asn1.ObjectIdentifier{{1, 2, 3}}
		},
		"no signing":         func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageKeyEncipherment },
		"critical extension": func(c *x509.Certificate) { c.UnhandledCriticalExtensions = []asn1.ObjectIdentifier{{1, 2, 3}} },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := *certificate
			change(&invalid)
			if _, err := ClientCertificateFingerprint(&invalid); err == nil {
				t.Fatal("invalid certificate accepted")
			}
		})
	}
}
