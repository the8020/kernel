package network

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func testCertificate(t *testing.T, serial int64) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func TestNativeTLSReloadPreservesConnectionsAndClientEvidence(t *testing.T) {
	m, err := New(freePort(t), testRootAlias)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	if err := m.RegisterRoute("/client", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			t.Error("TLS state lost")
			return
		}
		if len(r.TLS.PeerCertificates) == 0 {
			_, _ = io.WriteString(w, "anonymous")
			return
		}
		_, _ = fmt.Fprint(w, r.TLS.PeerCertificates[0].SerialNumber)
	})); err != nil {
		t.Fatal(err)
	}
	first, second, client := testCertificate(t, 1), testCertificate(t, 2), testCertificate(t, 3)
	port := freePort(t)
	config := &tls.Config{Certificates: []tls.Certificate{first}}
	prepared, err := m.PrepareTLS(port, config)
	if err != nil {
		t.Fatal(err)
	}
	prepared.Discard()
	prepared, err = m.PrepareTLS(port, config)
	if err != nil {
		t.Fatal(err)
	}
	prepared.Commit()
	getOK(t, m.Port(), "/health")
	roots := x509.NewCertPool()
	roots.AddCert(first.Leaf)
	roots.AddCert(second.Leaf)
	clientConfig := &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{client}, ClientSessionCache: tls.NewLRUClientSessionCache(4)}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	connection, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(connection)
	request := func() {
		t.Helper()
		_, err := fmt.Fprintf(connection, "GET /client HTTP/1.1\r\nHost: localhost\r\n\r\n")
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || string(body) != "3" {
			t.Fatalf("client certificate = %d %q", response.StatusCode, body)
		}
	}
	request()
	resumed, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	_ = resumed.SetDeadline(time.Now().Add(time.Second))
	_, _ = fmt.Fprint(resumed, "GET /client HTTP/1.1\r\nHost: localhost\r\n\r\n")
	resumeResponse, err := http.ReadResponse(bufio.NewReader(resumed), nil)
	if err != nil {
		resumed.Close()
		t.Fatal(err)
	}
	resumeBody, _ := io.ReadAll(resumeResponse.Body)
	resumeResponse.Body.Close()
	if !resumed.ConnectionState().DidResume || string(resumeBody) != "3" {
		t.Fatal("resumed TLS lost client certificate evidence")
	}
	resumed.Close()
	prepared, err = m.PrepareTLS(port, &tls.Config{Certificates: []tls.Certificate{second}})
	if err != nil {
		t.Fatal(err)
	}
	prepared.Commit()
	request() // The same connection and certificate proof survive publication.
	if connection.ConnectionState().PeerCertificates[0].SerialNumber.Int64() != 1 {
		t.Fatal("existing connection replaced")
	}
	connection.Close()
	freshClient := clientConfig.Clone()
	freshClient.ClientSessionCache = nil // Inspect the replacement in a full handshake.
	transport := &http.Transport{TLSClientConfig: freshClient, ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: time.Second}
	response, err := httpClient.Get("https://" + address + "/client")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "3" || response.ProtoMajor != 2 || response.TLS.PeerCertificates[0].SerialNumber.Int64() != 2 {
		t.Fatal("new HTTP/2 connection did not use the new certificate")
	}
	if _, err := m.PrepareTLS(port, nil); err == nil {
		t.Fatal("missing provider accepted")
	}
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	if _, err := m.PrepareTLS(occupied.Addr().(*net.TCPAddr).Port, config); err == nil {
		t.Fatal("occupied port accepted")
	}
	if m.TLSPort() != port {
		t.Fatal("failed preparation removed active HTTPS")
	}
	transport.CloseIdleConnections()
	anonymous := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	defer anonymous.CloseIdleConnections()
	response, err = (&http.Client{Transport: anonymous, Timeout: time.Second}).Get("https://" + address + "/client")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "anonymous" {
		t.Fatal("TLS required a client certificate for password/token clients")
	}
	anonymous.CloseIdleConnections()
	newPort := freePort(t)
	prepared, err = m.PrepareTLS(newPort, config)
	if err != nil {
		t.Fatal(err)
	}
	prepared.Commit()
	if m.TLSPort() != newPort {
		t.Fatal("HTTPS port did not change")
	}
	m.Close()
	if m.TLSPort() != 0 {
		t.Fatal("HTTPS port remains after close")
	}
}
