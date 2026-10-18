package webservices

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"the8020/kernel/auth"
	"the8020/kernel/execution"
)

func TestCertificateAuthenticationUsesSharedBoundaryOnEveryRequest(t *testing.T) {
	root := t.TempDir()
	index := newTestServiceIndex(t, root, "the8020/demo/protected", func(spec *Specification) { spec.Access.Mode = "authenticated" })
	pools := newFakePools()
	pools.dispatched = make(chan dispatchedRequest, 1)
	m := newTestManager(t, index, pools, &fakeRouter{}, filepath.Join(root, "services"))
	m.authentication = &fakeAuthentication{}
	m.authenticator = "/p/the8020/users/mod.ts"
	certificate := &x509.Certificate{Raw: []byte("client certificate"), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	fingerprint, _ := auth.ClientCertificateFingerprint(certificate)
	calls, enrolled := 0, true
	m.authenticateCertificate = func(_ context.Context, got string) (execution.User, error) {
		calls++
		if !enrolled || got != fingerprint {
			return execution.User{}, errors.New("denied")
		}
		return execution.UserForUsername("alice")
	}
	if _, err := m.Reconcile(t.Context(), "the8020/demo/protected"); err != nil {
		t.Fatal(err)
	}
	request := func() *http.Request {
		r := httptest.NewRequest("GET", "https://example.test/the8020/demo/protected/value", nil)
		r.TLS = &tls.ConnectionState{HandshakeComplete: true, DidResume: true, PeerCertificates: []*x509.Certificate{certificate}}
		r.AddCookie(&http.Cookie{Name: "the8020_auth_sys-aaaaaaaaaa", Value: "valid-jwt"})
		return r
	}
	accepted := httptest.NewRecorder()
	m.ServeHTTP(accepted, request())
	if accepted.Code != http.StatusOK || calls != 1 {
		t.Fatalf("certificate admission: %d, calls=%d", accepted.Code, calls)
	}
	forwarded := <-pools.dispatched
	if forwarded.header.Get("the8020-internal-user-id") != "user:alice" {
		t.Fatal("certificate principal lost")
	}
	enrolled = false
	denied := httptest.NewRecorder()
	m.ServeHTTP(denied, request())
	if denied.Code != http.StatusUnauthorized || calls != 2 {
		t.Fatal("removed certificate reused approval or fell back to cookie")
	}
	explicit := request()
	explicit.Header.Set(auth.TokenHeader, "Bearer valid-jwt")
	response := httptest.NewRecorder()
	m.ServeHTTP(response, explicit)
	if response.Code != http.StatusOK || calls != 2 {
		t.Fatal("explicit token did not take precedence")
	}
	<-pools.dispatched
	for _, authorization := range []string{"", "Bearer unsupported", "Basic !!!"} {
		explicit = request()
		explicit.Header.Set("Authorization", authorization)
		response = httptest.NewRecorder()
		m.ServeHTTP(response, explicit)
		if response.Code != http.StatusUnauthorized || calls != 2 {
			t.Fatal("malformed explicit credentials fell back to certificate or cookie")
		}
	}
	certificate.NotAfter = time.Now().Add(-time.Hour)
	response = httptest.NewRecorder()
	m.ServeHTTP(response, request())
	if response.Code != http.StatusUnauthorized || calls != 2 {
		t.Fatal("expired certificate reached users or fell back to cookie")
	}
	forged := request()
	forged.TLS = nil
	forged.Header.Del("Cookie")
	forged.Header.Set("Client-Cert", "forged")
	forged.Header.Set("the8020-internal-client-certificate", "forged")
	response = httptest.NewRecorder()
	m.ServeHTTP(response, forged)
	if response.Code != http.StatusUnauthorized || calls != 2 {
		t.Fatal("public header authenticated")
	}
}
