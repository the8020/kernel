package webservices

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"the8020/kernel/auth"
)

func newTestRouteSigner(t testing.TB) *auth.Signer {
	t.Helper()
	signer, err := auth.OpenSigner(filepath.Join(t.TempDir(), "keys", "signing.key"), base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func TestPersistentRouteTransportsRejectAmbiguousTokens(t *testing.T) {
	for _, test := range []struct {
		name           string
		headers, query []string
		want           string
		invalid        bool
	}{
		{name: "absent"},
		{name: "header", headers: []string{"token"}, want: "token"},
		{name: "query", query: []string{"token"}, want: "token"},
		{name: "matching", headers: []string{"token"}, query: []string{"token"}, want: "token"},
		{name: "conflicting", headers: []string{"token"}, query: []string{"other"}, invalid: true},
		{name: "empty", headers: []string{""}, invalid: true},
		{name: "duplicate header", headers: []string{"token", "token"}, invalid: true},
		{name: "duplicate query", query: []string{"token", "token"}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := websocketRequest("/example/realtime/channel", "")
			for _, value := range test.headers {
				request.Header.Add(RouteHeader, value)
			}
			query := request.URL.Query()
			for _, value := range test.query {
				query.Add("route", value)
			}
			request.URL.RawQuery = query.Encode()
			value, err := persistentRequestToken(request)
			if value != test.want || (err != nil) != test.invalid {
				t.Fatalf("token=%q err=%v", value, err)
			}
		})
	}
	request := httptest.NewRequest(http.MethodGet, "/example/realtime/channel?route=application-data", nil)
	if token, err := persistentRequestToken(request); err != nil || token != "" {
		t.Fatal("ordinary HTTP query was interpreted as a route")
	}
}
