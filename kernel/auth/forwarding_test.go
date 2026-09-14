package auth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestForwardingKeyProvisioningIsolationAndReplacement(t *testing.T) {
	seed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	t.Setenv(SigningKeyEnvironment, seed)
	first := testSigner(t) // Existing automatically generated key must be overridden.
	first, err := OpenSigner(first.path, os.Getenv(SigningKeyEnvironment))
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenSigner(filepath.Join(t.TempDir(), "signing.key"), os.Getenv(SigningKeyEnvironment))
	if err != nil {
		t.Fatal(err)
	}
	clientConfig, serverConfig := first.ForwardingTLSConfig(), second.ForwardingTLSConfig()
	certificate, _ := clientConfig.GetClientCertificate(nil)
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate.Leaf}}
	if first.Fingerprint() != second.Fingerprint() || serverConfig.VerifyConnection(state) != nil {
		t.Fatal("nodes provisioned with the same environment key disagree")
	}
	reloaded, err := OpenSigner(first.path, "")
	if err != nil || reloaded.VerifyForwardingPeer(state) != nil {
		t.Fatal("forwarding key changed after restart without environment override")
	}
	if testSigner(t).VerifyForwardingPeer(state) == nil || first.VerifyForwardingPeer(tls.ConnectionState{}) == nil {
		t.Fatal("missing or unrelated peer key accepted")
	}
	message := []byte("native forwarding handshake")
	encoded, err := first.Sign("app-example", message)
	if err != nil {
		t.Fatal(err)
	}
	signature, _ := base64.RawURLEncoding.DecodeString(encoded)
	if ed25519.Verify(certificate.Leaf.PublicKey.(ed25519.PublicKey), message, signature) {
		t.Fatal("package arbitrary-byte signing can impersonate a native peer")
	}
	if first.VerifyForwardingPeer(tls.ConnectionState{PeerCertificates: []*x509.Certificate{{PublicKey: first.session.Public()}}}) == nil {
		t.Fatal("application signing key accepted as native peer key")
	}
	if err := first.Replace("invalid-provisioned-value"); err == nil || first.VerifyForwardingPeer(state) != nil {
		t.Fatal("failed replacement changed forwarding authority")
	}
	nextSeed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, ed25519.SeedSize))
	if err := first.Replace(nextSeed); err != nil {
		t.Fatal(err)
	}
	if clientConfig.VerifyConnection(state) == nil {
		t.Fatal("existing TLS config accepted the replaced key")
	}
	nextCertificate, _ := clientConfig.GetClientCertificate(nil)
	if bytes.Equal(certificate.Certificate[0], nextCertificate.Certificate[0]) {
		t.Fatal("existing TLS config retained the old client certificate")
	}
	if err := second.Replace(nextSeed); err != nil {
		t.Fatal(err)
	}
	if serverConfig.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{nextCertificate.Leaf}}) != nil {
		t.Fatal("provisioned replacement did not converge")
	}
	// A configured environment override wins again at restart.
	first, err = OpenSigner(first.path, os.Getenv(SigningKeyEnvironment))
	if err != nil || first.VerifyForwardingPeer(state) != nil {
		t.Fatal("environment override did not replace persisted forwarding authority")
	}
}

func TestForwardingTLSRequiresNativeKeysAtBothPeers(t *testing.T) {
	owner := testSigner(t)
	peer, err := OpenSigner(filepath.Join(t.TempDir(), "signing.key"), base64.StdEncoding.EncodeToString(owner.master))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		TLSConfig: owner.ForwardingTLSConfig(), ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	go func() { _ = server.ServeTLS(listener, "", "") }()
	t.Cleanup(func() { _ = server.Close() })
	endpoint := "https://" + listener.Addr().String()
	transport := &http.Transport{TLSClientConfig: peer.ForwardingTLSConfig()}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: time.Second}
	response, err := client.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.TLS.Version != tls.VersionTLS13 {
		t.Fatal("native TLS request failed")
	}
	wrongClient := owner.ForwardingTLSConfig()
	wrongClient.GetClientCertificate = testSigner(t).ForwardingTLSConfig().GetClientCertificate
	for name, config := range map[string]*tls.Config{
		"wrong server key": testSigner(t).ForwardingTLSConfig(),
		"wrong client key": wrongClient,
		"no client key":    {InsecureSkipVerify: true}, // Test an uncredentialed attacker.
	} {
		t.Run(name, func(t *testing.T) {
			transport := &http.Transport{TLSClientConfig: config}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: time.Second}
			response, err := client.Get(endpoint)
			if err == nil {
				response.Body.Close()
				t.Fatal("uncredentialed peer accepted")
			}
		})
	}
}
