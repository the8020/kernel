package auth

import (
	"bytes"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestRouteTokensShareDeploymentKeysAndSeparateAuthentication(t *testing.T) {
	seed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32))
	first, err := OpenSigner(filepath.Join(t.TempDir(), "a", "key"), seed)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenSigner(filepath.Join(t.TempDir(), "b", "key"), seed)
	if err != nil {
		t.Fatal(err)
	}
	target := RouteTarget{NodeID: "node-a", SandboxID: "sbx-original", WorkerID: "wrk-original", ExecutionID: "persistent-original"}
	token, err := first.SignRoute(target)
	if err != nil {
		t.Fatal(err)
	}
	if actual, err := second.VerifyRoute(token); err != nil || actual != target {
		t.Fatalf("target=%#v err=%v", actual, err)
	}
	if _, err := second.VerifyToken(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("route qualified as authentication")
	}
	authToken, err := first.SignToken(testTokenClaims(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.VerifyRoute(authToken); !errors.Is(err, ErrInvalidRoute) {
		t.Fatal("authentication qualified as routing")
	}
	parts := strings.Split(token, ".")
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte(`{"node_id":"node-b","sandbox_id":"sbx-attacker","worker_id":"wrk-attacker","execution_id":"persistent-other","iss":"the8020","aud":"the8020"}`))
	if _, err := second.VerifyRoute(strings.Join(parts, ".")); !errors.Is(err, ErrInvalidRoute) {
		t.Fatal("tampered target accepted")
	}
	if _, err := testSigner(t).VerifyRoute(token); !errors.Is(err, ErrInvalidRoute) {
		t.Fatal("other deployment key accepted")
	}
	if err := second.Replace(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{21}, 32))); err != nil {
		t.Fatal(err)
	}
	if _, err := second.VerifyRoute(token); !errors.Is(err, ErrInvalidRoute) {
		t.Fatal("replaced key accepted old route")
	}
}

func TestRouteValidationRequiresEveryTargetAndExactJWTProfile(t *testing.T) {
	signer := testSigner(t)
	base := RouteTarget{NodeID: "node", SandboxID: "sandbox", WorkerID: "worker", ExecutionID: "execution"}
	for _, invalid := range []RouteTarget{
		{}, {SandboxID: "sandbox", WorkerID: "worker", ExecutionID: "execution"},
		{NodeID: "node", WorkerID: "worker", ExecutionID: "execution"},
		{NodeID: "node", SandboxID: "sandbox", ExecutionID: "execution"},
		{NodeID: "node", SandboxID: "sandbox", WorkerID: "worker"},
	} {
		if _, err := signer.SignRoute(invalid); !errors.Is(err, ErrInvalidRoute) {
			t.Fatal("incomplete target signed")
		}
	}
	for _, typ := range []string{"JWT", TokenType, ""} {
		token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, routeClaims{RouteTarget: base, RegisteredClaims: jwt.RegisteredClaims{Issuer: TokenIssuer, Audience: jwt.ClaimStrings{TokenAudience}}})
		token.Header["typ"], token.Header["kid"] = typ, signer.Fingerprint()
		encoded, err := token.SignedString(signer.key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := signer.VerifyRoute(encoded); !errors.Is(err, ErrInvalidRoute) {
			t.Fatalf("accepted wrong JWT type %q", typ)
		}
	}
}
