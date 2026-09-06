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
	target := RouteTarget{NodeID: "nod-aaaaaaaaaa", SandboxID: "sbx-aaaaaaaaaa", WorkerID: "wrk-aaaaaaaaaa", ExecutionID: "pex-aaaaaaaaaa"}
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
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte(`{"node_id":"nod-bbbbbbbbbb","sandbox_id":"sbx-bbbbbbbbbb","worker_id":"wrk-bbbbbbbbbb","execution_id":"pex-bbbbbbbbbb","iss":"the8020","aud":"the8020"}`))
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
	base := RouteTarget{NodeID: "nod-aaaaaaaaaa", SandboxID: "sbx-aaaaaaaaaa", WorkerID: "wrk-aaaaaaaaaa", ExecutionID: "pex-aaaaaaaaaa"}
	for _, field := range []string{"node", "sandbox", "worker", "execution"} {
		for _, value := range []string{"", "arbitrary-name", "ctx-aaaaaaaaaa", "nod-TOO_SHORT"} {
			invalid := base
			switch field {
			case "node":
				invalid.NodeID = value
			case "sandbox":
				invalid.SandboxID = value
			case "worker":
				invalid.WorkerID = value
			case "execution":
				invalid.ExecutionID = value
			}
			if _, err := signer.SignRoute(invalid); !errors.Is(err, ErrInvalidRoute) {
				t.Fatalf("signed malformed %s", field)
			}
			// A correctly signed token still has to satisfy the runtime ID contract.
			token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, routeClaims{RouteTarget: invalid, RegisteredClaims: jwt.RegisteredClaims{Issuer: TokenIssuer, Audience: jwt.ClaimStrings{TokenAudience}}})
			token.Header["typ"], token.Header["kid"] = RouteTokenType, signer.Fingerprint()
			encoded, err := token.SignedString(signer.key)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := signer.VerifyRoute(encoded); !errors.Is(err, ErrInvalidRoute) {
				t.Fatalf("accepted signed malformed %s", field)
			}
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
