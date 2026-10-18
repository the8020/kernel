package auth

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"time"
)

type clientCertificateKey struct{}

// WithClientCertificate carries evidence established by a TLS terminator or an
// authenticated native peer. Public headers never set this context directly.
func WithClientCertificate(ctx context.Context, certificate *x509.Certificate) context.Context {
	return context.WithValue(ctx, clientCertificateKey{}, certificate)
}

func ClientCertificate(request *http.Request) *x509.Certificate {
	if certificate, ok := request.Context().Value(clientCertificateKey{}).(*x509.Certificate); ok {
		return certificate
	}
	if request.TLS != nil && request.TLS.HandshakeComplete && len(request.TLS.PeerCertificates) > 0 {
		return request.TLS.PeerCertificates[0]
	}
	return nil
}

// ClientCertificateFingerprint validates use of a certificate whose key
// possession was established by TLS. Enrollment and account policy stay in users.
func ClientCertificateFingerprint(certificate *x509.Certificate) (string, error) {
	now := time.Now()
	if certificate == nil || len(certificate.Raw) == 0 || certificate.IsCA ||
		now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) ||
		len(certificate.UnhandledCriticalExtensions) != 0 ||
		(certificate.KeyUsage != 0 && certificate.KeyUsage&x509.KeyUsageDigitalSignature == 0) ||
		((len(certificate.ExtKeyUsage) != 0 || len(certificate.UnknownExtKeyUsage) != 0) &&
			!slices.Contains(certificate.ExtKeyUsage, x509.ExtKeyUsageClientAuth) &&
			!slices.Contains(certificate.ExtKeyUsage, x509.ExtKeyUsageAny)) {
		return "", errors.New("invalid client certificate")
	}
	fingerprint := sha256.Sum256(certificate.Raw)
	return hex.EncodeToString(fingerprint[:]), nil
}
