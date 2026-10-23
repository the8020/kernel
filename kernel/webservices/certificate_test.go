package webservices

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"the8020/kernel/auth"
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
	if accepted.Code != http.StatusOK {
		t.Fatalf("certificate admission: %d", accepted.Code)
	}
	forwarded := <-pools.dispatched
	var setup authenticationSetup
	encoded, err := base64.StdEncoding.DecodeString(forwarded.header.Get("the8020-internal-authentication"))
	if err != nil || json.Unmarshal(encoded, &setup) != nil || setup.Approved || setup.Principal != "" || setup.Input.(map[string]any)["fingerprint"] != fingerprint || setup.Module != m.authenticator {
		t.Fatal("certificate must reach target hook without an approved identity")
	}
	// Every request supplies fresh evidence; no native approval is reused.
	for i := 0; i < 2; i++ {
		response := httptest.NewRecorder()
		m.ServeHTTP(response, request())
		if response.Code != http.StatusOK || (<-pools.dispatched).header.Get("the8020-internal-authentication") != forwarded.header.Get("the8020-internal-authentication") {
			t.Fatal("certificate evidence changed")
		}
	}
	explicit := request()
	explicit.Header.Set(auth.TokenHeader, "Bearer valid-jwt")
	response := httptest.NewRecorder()
	m.ServeHTTP(response, explicit)
	if response.Code != http.StatusOK {
		t.Fatal("explicit token did not take precedence")
	}
	<-pools.dispatched
	for _, authorization := range []string{"", "Bearer unsupported", "Basic !!!"} {
		explicit = request()
		explicit.Header.Set("Authorization", authorization)
		response = httptest.NewRecorder()
		m.ServeHTTP(response, explicit)
		if response.Code != http.StatusUnauthorized {
			t.Fatal("malformed explicit credentials fell back to certificate or cookie")
		}
	}
	certificate.NotAfter = time.Now().Add(-time.Hour)
	response = httptest.NewRecorder()
	m.ServeHTTP(response, request())
	if response.Code != http.StatusUnauthorized {
		t.Fatal("expired certificate reached users or fell back to cookie")
	}
	forged := request()
	forged.TLS = nil
	forged.Header.Del("Cookie")
	forged.Header.Set("Client-Cert", "forged")
	forged.Header.Set("the8020-internal-client-certificate", "forged")
	response = httptest.NewRecorder()
	m.ServeHTTP(response, forged)
	if response.Code != http.StatusUnauthorized {
		t.Fatal("public header authenticated")
	}
}
