package network

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"the8020/kernel/auth"
)

func publishedCertificate(t *testing.T, cert tls.Certificate, id string) (ServerCertificate, string) {
	t.Helper()
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return ServerCertificate{ID: id, Name: "localhost", KeySecret: id, Chain: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}))}, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
}

func TestHTTPSPublicationCachesParsedKeysAndEnforcesLivePolicy(t *testing.T) {
	h := NewHTTPS(context.Background(), nil, nil)
	t.Cleanup(h.Close)
	record, key := publishedCertificate(t, testCertificate(t, 71), "one")
	state := HTTPSState{Revision: 1, Certificates: []ServerCertificate{record}}
	state.Configuration.Enabled = true
	state.Configuration.AllowedDomains = []string{"*"}
	reads := 0
	load := func(context.Context, string) (string, error) { reads++; return key, nil }
	commit, err := h.Prepare(context.Background(), state, load)
	if err != nil {
		t.Fatal(err)
	}
	commit()
	for _, name := range []string{"localhost", "127.0.0.1", "::ffff:127.0.0.1", ""} {
		cert, err := h.getCertificate(&tls.ClientHelloInfo{ServerName: name})
		if err != nil || cert.Leaf.SerialNumber.Int64() != 71 {
			t.Fatalf("%s: %v", name, err)
		}
	}
	manager, err := New(freePort(t), testRootAlias)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	manager.SetHTTPSIngress(h)
	port := freePort(t)
	listener, err := manager.PrepareTLS(port, h.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	listener.Commit()
	client := &tls.Config{ServerName: "localhost", InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12, ClientSessionCache: tls.NewLRUClientSessionCache(1)}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for attempt := range 2 {
		connection, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, client)
		if err != nil {
			t.Fatal(err)
		}
		if connection.ConnectionState().DidResume != (attempt == 1) {
			t.Fatal("session resumption not exercised")
		}
		connection.Close()
	}
	state.Revision++
	state.Configuration.AllowedDomains = []string{"*.example.test"}
	commit, err = h.Prepare(context.Background(), state, load)
	if err != nil {
		t.Fatal(err)
	}
	commit()
	connection, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, client)
	if err == nil {
		connection.Close()
		t.Fatal("resumed TLS bypassed changed hostname policy")
	}
	if reads != 1 {
		t.Fatalf("unchanged keys loaded %d times", reads)
	}
	for name, allowed := range map[string]bool{"app.example.test": true, "example.test": false, "a.b.example.test": false, "localhost": false} {
		err := h.TLSConfig().VerifyConnection(tls.ConnectionState{ServerName: name, DidResume: true})
		if (err == nil) != allowed {
			t.Fatalf("resumed %s allowed=%t: %v", name, allowed, err)
		}
		r := httptest.NewRequest("GET", "https://"+name+"/health", nil)
		w := httptest.NewRecorder()
		if (h.admit(w, r) != nil) != allowed {
			t.Fatalf("HTTP %s allowed=%t: %d", name, allowed, w.Code)
		}
	}
	state.Certificates[0].Chain = "invalid"
	if _, err := h.Prepare(context.Background(), state, load); err == nil {
		t.Fatal("immutable version changed")
	}
	if h.current.Load().revision != 2 {
		t.Fatal("failed publication changed accepted state")
	}
	r := httptest.NewRequest("GET", "http://localhost/health", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	if h.admit(httptest.NewRecorder(), r) == nil {
		t.Fatal("local recovery denied")
	}
}

func TestHTTPSColdHandshakesShareOneOperation(t *testing.T) {
	manager, err := New(freePort(t), testRootAlias)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	ready := make(chan struct{})
	var calls atomic.Int32
	var h *HTTPS
	record, key := publishedCertificate(t, testCertificate(t, 81), "cold")
	state := HTTPSState{}
	state.Configuration.Enabled, state.Configuration.LetsEncrypt = true, true
	state.Configuration.AllowedDomains = []string{"localhost"}
	h = NewHTTPS(context.Background(), func(ctx context.Context, name string) error {
		calls.Add(1)
		select {
		case <-ready:
		case <-ctx.Done():
			return ctx.Err()
		}
		loaded := state
		loaded.Certificates = []ServerCertificate{record}
		commit, err := h.Prepare(ctx, loaded, func(context.Context, string) (string, error) { return key, nil })
		if err == nil {
			commit()
		}
		return err
	}, nil)
	manager.SetHTTPSIngress(h)
	commit, err := h.Prepare(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	commit()
	port := freePort(t)
	listener, err := manager.PrepareTLS(port, h.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	listener.Commit()
	var wait sync.WaitGroup
	for range 12 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &tls.Config{ServerName: "localhost", InsecureSkipVerify: true})
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
		}()
	}
	deadline := time.Now().Add(time.Second)
	for {
		h.mu.Lock()
		count := h.waiters
		h.mu.Unlock()
		if count == 12 {
			break
		}
		if time.Now().After(deadline) {
			close(ready)
			wait.Wait()
			t.Fatalf("only %d waiting handshakes", count)
		}
		time.Sleep(time.Millisecond)
	}
	close(ready)
	wait.Wait()
	if calls.Load() != 1 {
		t.Fatalf("%d issuance operations", calls.Load())
	}
}

func TestHTTPSAutomaticNamesWorkWithoutACME(t *testing.T) {
	var calls atomic.Int32
	h := NewHTTPS(context.Background(), func(ctx context.Context, name string) error {
		calls.Add(1)
		return fmt.Errorf("test issuer: %s", name)
	}, nil)
	t.Cleanup(h.Close)
	state := HTTPSState{}
	state.Configuration.Enabled = true
	state.Configuration.AllowedDomains = []string{"*"}
	state.Configuration.AutomaticNames = []string{"localhost", "127.0.0.1"}
	commit, err := h.Prepare(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	commit()
	manager, err := New(freePort(t), testRootAlias)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	manager.SetHTTPSIngress(h)
	port := freePort(t)
	listener, err := manager.PrepareTLS(port, h.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	listener.Commit()
	dial := func(name string) error {
		connection, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &tls.Config{ServerName: name, InsecureSkipVerify: true})
		if connection != nil {
			connection.Close()
		}
		return err
	}
	for _, name := range []string{"localhost", ""} {
		if err := dial(name); err == nil {
			t.Fatal("failed issuer returned a certificate")
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("%d local issuance calls", calls.Load())
	}
	if err := dial("public.example.com"); err == nil || calls.Load() != 2 {
		t.Fatal("disabled ACME invoked issuance")
	}
}

func TestHTTPSForwardedCertificateRequiresTrustedPeerAndSingletonDER(t *testing.T) {
	h := NewHTTPS(context.Background(), nil, nil)
	t.Cleanup(h.Close)
	state := HTTPSState{}
	state.Configuration.AllowedDomains = []string{"*"}
	cert := testCertificate(t, 91)
	header := ":" + base64.StdEncoding.EncodeToString(cert.Certificate[0]) + ":"
	apply := func() {
		t.Helper()
		commit, err := h.Prepare(context.Background(), state, nil)
		if err != nil {
			t.Fatal(err)
		}
		commit()
	}
	apply()
	request := func(values ...string) (*http.Request, *httptest.ResponseRecorder) {
		r := httptest.NewRequest("GET", "http://localhost/test", nil)
		r.RemoteAddr = "127.0.0.1:1234"
		for _, value := range values {
			r.Header.Add("Client-Cert", value)
		}
		r.Header.Set("Client-Cert-Chain", "untrusted")
		w := httptest.NewRecorder()
		return h.admit(w, r), w
	}
	r, _ := request(header)
	if r == nil || auth.ClientCertificate(r) != nil || len(r.Header) != 0 {
		t.Fatal("untrusted forwarding conferred authority or retained headers")
	}
	for _, proxy := range []string{"127.0.0.1", "127.0.0.1/32", "::ffff:127.0.0.1/128"} {
		state.Configuration.TrustedProxies = []string{proxy}
		apply()
		r, _ := request(header)
		if r == nil || auth.ClientCertificate(r) == nil {
			t.Fatalf("trusted proxy %s rejected", proxy)
		}
	}
	r, w := request(header)
	if r == nil || auth.ClientCertificate(r).SerialNumber.Int64() != 91 || w.Header().Get("Vary") != "Client-Cert" {
		t.Fatal("trusted certificate evidence missing")
	}
	r, _ = request(":" + base64.RawStdEncoding.EncodeToString(cert.Certificate[0]) + ":")
	if r == nil || auth.ClientCertificate(r) == nil {
		t.Fatal("unpadded structured byte sequence rejected")
	}
	for _, values := range [][]string{{header, header}, {header + ", " + header}, {""}, {"PEM"}, {":AA==:"}, {header + ";x=y"}} {
		r, w := request(values...)
		if r != nil || w.Code != 400 {
			t.Fatalf("invalid header accepted: %v", values)
		}
	}
}

func TestHTTPSChallengeIsExactBoundedAndPrecedesHostnameAdmission(t *testing.T) {
	calls := 0
	lastLocalOnly := false
	h := NewHTTPS(context.Background(), nil, func(ctx context.Context, name, kind, token string, localOnly bool) (string, error) {
		calls++
		lastLocalOnly = localOnly
		if _, ok := ctx.Deadline(); !ok {
			t.Error("unbounded challenge lookup")
		}
		return fmt.Sprintf("%s:%s:%s", name, kind, token), nil
	})
	t.Cleanup(h.Close)
	state := HTTPSState{}
	state.Configuration.Enabled = true
	commit, err := h.Prepare(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	commit()
	for path, status := range map[string]int{"/.well-known/acme-challenge/0123456789abcdef": 200, "/.well-known/the8020-https/0123456789abcdef": 200, "/.well-known/acme-challenge/short": 404, "/.well-known/acme-challenge/0123456789abcdef/extra": 404} {
		r := httptest.NewRequest("GET", "http://other.example.test"+path, nil)
		w := httptest.NewRecorder()
		if h.admit(w, r) != nil || w.Code != status {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	if calls != 2 {
		t.Fatalf("%d challenge calls", calls)
	}
	r := httptest.NewRequest("GET", "http://other.example.test/.well-known/acme-challenge/0123456789abcdef?local=1", nil)
	w := httptest.NewRecorder()
	h.admit(w, r)
	if calls != 3 || !lastLocalOnly || w.Code != http.StatusOK { t.Fatal("local-only challenge routing was not preserved") }
}
