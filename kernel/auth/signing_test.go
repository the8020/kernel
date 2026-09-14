package auth

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testSigner(t *testing.T) *Signer {
	t.Helper()
	signer, err := OpenSigner(filepath.Join(t.TempDir(), "keys", "signing.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func testTokenClaims(now time.Time) TokenClaims {
	return TokenClaims{"iss": TokenIssuer, "aud": TokenAudience, "sub": "user:alice",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "sid": "0123456789abcdef0123456789abcdef", "ver": 1}
}

func TestSigningKeyPersistenceProvisioningAndReplacement(t *testing.T) {
	first := testSigner(t)
	reloaded, err := OpenSigner(first.path, "")
	if err != nil || reloaded.Fingerprint() != first.Fingerprint() {
		t.Fatalf("key changed on restart: %v", err)
	}
	for path, wanted := range map[string]os.FileMode{first.path: 0600, filepath.Dir(first.path): 0700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != wanted {
			t.Fatalf("private key permissions are incorrect: %v", err)
		}
	}
	value := []byte("arbitrary data, unrelated to authentication")
	signature, err := first.Sign("app-example", value)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := reloaded.Verify("app-example", value, signature)
	wrong, _ := reloaded.Verify("app-example", []byte("different data"), signature)
	if err != nil || !valid || wrong {
		t.Fatal("arbitrary signature verification failed")
	}
	seed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	if err := first.Replace(seed); err != nil {
		t.Fatal(err)
	}
	second, err := OpenSigner(filepath.Join(t.TempDir(), "signing.key"), seed)
	if err != nil || first.Fingerprint() != second.Fingerprint() {
		t.Fatalf("provisioned nodes disagree: %v", err)
	}
	if valid, _ := first.Verify("app-example", value, signature); valid {
		t.Fatal("old key survived replacement")
	}
	reloaded, err = OpenSigner(first.path, "")
	if err != nil || reloaded.Fingerprint() != first.Fingerprint() {
		t.Fatalf("replacement was not persisted: %v", err)
	}
	before := first.Fingerprint()
	if err := first.Replace("invalid-private-value"); err == nil || strings.Contains(err.Error(), "invalid-private-value") {
		t.Fatal("invalid key was accepted or exposed")
	}
	if first.Fingerprint() != before {
		t.Fatal("failed replacement changed the live key")
	}
	if _, err := OpenSigner(first.path, seed); err != nil {
		t.Fatal(err)
	}
}

func TestTokenProfileAndCrossNodeVerification(t *testing.T) {
	signer := testSigner(t)
	other, err := OpenSigner(filepath.Join(t.TempDir(), "signing.key"), base64.StdEncoding.EncodeToString(signer.master))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	encoded, err := signer.SignToken(testTokenClaims(now))
	if err != nil {
		t.Fatal(err)
	}
	claims, err := other.verifyTokenAt(encoded, now)
	if err != nil || claims["sub"] != "user:alice" {
		t.Fatalf("shared-key token verification: %v", err)
	}
	if _, err := testSigner(t).verifyTokenAt(encoded, now); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("another key accepted the token")
	}
	for name, mutate := range map[string]func(*jwt.Token){
		"wrong type":          func(token *jwt.Token) { token.Header["typ"] = "other+jwt" },
		"wrong key ID":        func(token *jwt.Token) { token.Header["kid"] = "other" },
		"wrong issuer":        func(token *jwt.Token) { token.Claims.(TokenClaims)["iss"] = "other" },
		"wrong audience":      func(token *jwt.Token) { token.Claims.(TokenClaims)["aud"] = "other" },
		"expired":             func(token *jwt.Token) { token.Claims.(TokenClaims)["exp"] = now.Unix() },
		"missing expiry":      func(token *jwt.Token) { delete(token.Claims.(TokenClaims), "exp") },
		"missing issued time": func(token *jwt.Token) { delete(token.Claims.(TokenClaims), "iat") },
		"future issued time":  func(token *jwt.Token) { token.Claims.(TokenClaims)["iat"] = now.Add(time.Minute).Unix() },
		"not yet valid":       func(token *jwt.Token) { token.Claims.(TokenClaims)["nbf"] = now.Add(time.Minute).Unix() },
		"invalid principal":   func(token *jwt.Token) { token.Claims.(TokenClaims)["sub"] = "../../alice" },
	} {
		t.Run(name, func(t *testing.T) {
			token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, testTokenClaims(now))
			token.Header["typ"], token.Header["kid"] = TokenType, signer.Fingerprint()
			mutate(token)
			encoded, err := token.SignedString(signer.session)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := signer.verifyTokenAt(encoded, now); !errors.Is(err, ErrInvalidToken) {
				t.Fatal("invalid token profile accepted")
			}
		})
	}
	rawSignature, err := signer.Sign("app-example", []byte("data"))
	if err != nil {
		t.Fatal(err)
	}
	for _, encoded := range []string{"", "malformed", rawSignature, strings.Repeat("x", MaximumTokenBytes+1)} {
		if _, err := signer.verifyTokenAt(encoded, now); !errors.Is(err, ErrInvalidToken) {
			t.Fatal("non-token accepted")
		}
	}
}

func TestRequestTokenUsesHeaderWithoutCookieFallback(t *testing.T) {
	request := httptest.NewRequest("GET", "/", nil)
	request.AddCookie(&http.Cookie{Name: TokenCookie, Value: "cookie-token"})
	if token, cookie := RequestToken(request); token != "cookie-token" || !cookie {
		t.Fatal("cookie was not selected")
	}
	request.Header.Set(TokenHeader, "Bearer header-token")
	if token, cookie := RequestToken(request); token != "header-token" || cookie {
		t.Fatal("explicit header did not take precedence")
	}
	for _, value := range []string{"", "Basic value", "Bearer", "Bearer one two"} {
		request.Header.Set(TokenHeader, value)
		if token, cookie := RequestToken(request); token != "" || cookie {
			t.Fatal("malformed header fell back to cookie")
		}
	}
	request.Header.Del(TokenHeader)
	request.AddCookie(&http.Cookie{Name: TokenCookie, Value: "second-cookie"})
	if token, cookie := RequestToken(request); token != "" || !cookie {
		t.Fatal("ambiguous cookie accepted")
	}
	response := httptest.NewRecorder()
	ClearTokenCookie(response, true)
	cleared := response.Result().Cookies()
	if len(cleared) != 1 || cleared[0].Name != TokenCookie || cleared[0].Path != "/" || cleared[0].MaxAge != -1 || !cleared[0].HttpOnly || !cleared[0].Secure {
		t.Fatal("rejected cookie does not clear the issuing scope")
	}
}

func TestDerivedSigningPurposesAreSeparated(t *testing.T) {
	signer := testSigner(t)
	message := []byte("same bytes, different purposes")
	signature, err := signer.Sign("app-example", message)
	if err != nil {
		t.Fatal(err)
	}
	if valid, err := signer.Verify("app-other", message, signature); err != nil || valid {
		t.Fatal("another application purpose accepted the signature")
	}
	for _, purpose := range []string{"", "app-", "node-forwarding", "service-routing", "App-example", "app-../node-forwarding", "app-é", "app-x\x00", "app-" + strings.Repeat("x", 125)} {
		if _, err := signer.Sign(purpose, message); err == nil {
			t.Fatalf("signed with invalid purpose %q", purpose)
		}
		if _, err := signer.Verify(purpose, message, signature); err == nil {
			t.Fatalf("verified with invalid purpose %q", purpose)
		}
	}
	// Even correctly profiled JWTs cannot substitute another purpose's key or
	// use the master as an Ed25519 seed. Verification chooses the expected key.
	master := ed25519.NewKeyFromSeed(signer.master)
	application, err := deriveSigningKey(signer.master, "app-example")
	if err != nil {
		t.Fatal(err)
	}
	peer := signer.forwarding.PrivateKey.(ed25519.PrivateKey)
	for _, profile := range []struct {
		typ string
		key ed25519.PrivateKey
	}{
		{TokenType, signer.session}, {RouteTokenType, signer.routing},
	} {
		claims := testTokenClaims(time.Now())
		claims["node_id"], claims["sandbox_id"] = "nod-aaaaaaaaaa", "sbx-aaaaaaaaaa"
		claims["worker_id"], claims["execution_id"] = "wrk-aaaaaaaaaa", "pex-aaaaaaaaaa"
		token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
		token.Header["typ"], token.Header["kid"] = profile.typ, keyFingerprint(profile.key)
		for purpose, key := range map[string]ed25519.PrivateKey{
			"master": master, "application": application, "peer": peer,
			TokenType: signer.session, RouteTokenType: signer.routing,
		} {
			encoded, err := token.SignedString(key)
			if err != nil {
				t.Fatal(err)
			}
			if profile.typ == TokenType {
				_, err = signer.VerifyToken(encoded)
			} else {
				_, err = signer.VerifyRoute(encoded)
			}
			if (err == nil) != (purpose == profile.typ) {
				t.Fatal("JWT accepted the wrong signing purpose or rejected its own")
			}
		}
	}
	decoded, _ := base64.RawURLEncoding.DecodeString(signature)
	for _, key := range []ed25519.PrivateKey{master, signer.session, signer.routing, peer} {
		if ed25519.Verify(key.Public().(ed25519.PublicKey), message, decoded) {
			t.Fatal("application signing reused another key")
		}
	}
}

func TestNativeTokenVerificationLeavesSessionRepresentationToPackages(t *testing.T) {
	signer := testSigner(t)
	now := time.Now()
	for _, fields := range []TokenClaims{
		{}, {"sid": "", "ver": 0}, {"sid": strings.Repeat("x", 129), "ver": 1.5},
		{"sid": map[string]any{"new": "representation"}, "ver": "v2"},
	} {
		claims := testTokenClaims(now)
		delete(claims, "sid")
		delete(claims, "ver")
		for key, value := range fields {
			claims[key] = value
		}
		encoded, err := signer.SignToken(claims)
		if err != nil {
			t.Fatal(err)
		}
		verified, err := signer.VerifyToken(encoded)
		if err != nil || verified["sub"] != claims["sub"] {
			t.Fatalf("session policy entered native verification: %v", err)
		}
	}
}
