package webservices

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"the8020/kernel/auth"
)

func TestJWTGateRejectsBeforeColdExecutionAndUsesHeaderPrecedence(t *testing.T) {
	for _, websocket := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "websocket"}[websocket], func(t *testing.T) {
			root := t.TempDir()
			const serviceID = "example/test/protected"
			store := newTestServiceIndex(t, root, serviceID, func(spec *Specification) { spec.Access.Mode = "authenticated" })
			if _, err := editTestSpecification(store, serviceID, func(state *Specification) error { state.Enabled = true; return nil }); err != nil {
				t.Fatal(err)
			}
			pools := newFakePools()
			pools.dispatched = make(chan dispatchedRequest, 1)
			manager := newTestManager(t, store, pools, &fakeRouter{}, filepath.Join(root, "observed"))
			signer, err := auth.OpenSigner(filepath.Join(root, "keys", "signing.key"), "")
			if err != nil {
				t.Fatal(err)
			}
			manager.authentication = signer
			claims := auth.TokenClaims{"iss": auth.TokenIssuer, "aud": auth.TokenAudience, "sub": "user:alice", "sid": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "ver": 1, "iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Minute).Unix()}
			valid, err := signer.SignToken(claims)
			if err != nil {
				t.Fatal(err)
			}
			claims["exp"] = time.Now().Add(-time.Second).Unix()
			expired, err := signer.SignToken(claims)
			if err != nil {
				t.Fatal(err)
			}
			request := func(cookie string, header *string) *http.Request {
				value := httptest.NewRequest(http.MethodGet, "/"+serviceID+"/", nil)
				if websocket {
					value = websocketRequest("/"+serviceID+"/", "")
				}
				if cookie != "" {
					value.AddCookie(&http.Cookie{Name: auth.TokenCookie, Value: cookie})
				}
				if header != nil {
					value.Header.Set(auth.TokenHeader, *header)
				}
				return value
			}
			badHeader := "Bearer invalid"
			emptyHeader := ""
			for _, input := range []struct {
				cookie string
				header *string
				clear  bool
			}{
				{"", nil, false}, {"invalid", nil, true}, {expired, nil, true}, {valid, &badHeader, false}, {valid, &emptyHeader, false},
			} {
				response := httptest.NewRecorder()
				manager.ServeHTTP(response, request(input.cookie, input.header))
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("invalid token status %d", response.Code)
				}
				if len(response.Result().Cookies()) > 0 != input.clear {
					t.Fatal("rejected cookie removal mismatch")
				}
				if len(pools.events) != 0 || len(pools.websockets) != 0 || len(pools.dispatched) != 0 || len(manager.services) != 0 {
					t.Fatal("invalid token reached execution setup")
				}
			}
			header := "Bearer " + valid
			response := httptest.NewRecorder()
			manager.ServeHTTP(response, request("invalid", &header))
			if response.Code != http.StatusOK {
				t.Fatalf("valid header status %d: %s", response.Code, response.Body.String())
			}
			var forwarded dispatchedRequest
			if websocket {
				forwarded = pools.websockets[0]
			} else {
				forwarded = <-pools.dispatched
			}
			if forwarded.header.Get("the8020-internal-username") != "alice" || forwarded.header.Get("the8020-internal-authentication") == "" {
				t.Fatal("verified identity lost before Worker")
			}
		})
	}
}
