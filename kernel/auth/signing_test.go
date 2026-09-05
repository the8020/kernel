package auth

import (
	"bytes"
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
	signature := first.Sign(value)
	if !reloaded.Verify(value, signature) || reloaded.Verify([]byte("different data"), signature) {
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
	if first.Verify(value, signature) {
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
	other, err := OpenSigner(filepath.Join(t.TempDir(), "signing.key"), base64.StdEncoding.EncodeToString(signer.key.Seed()))
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
		"missing session":     func(token *jwt.Token) { delete(token.Claims.(TokenClaims), "sid") },
		"missing version":     func(token *jwt.Token) { delete(token.Claims.(TokenClaims), "ver") },
		"fractional version":  func(token *jwt.Token) { token.Claims.(TokenClaims)["ver"] = 1.5 },
		"invalid principal":   func(token *jwt.Token) { token.Claims.(TokenClaims)["sub"] = "../../alice" },
	} {
		t.Run(name, func(t *testing.T) {
			token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, testTokenClaims(now))
			token.Header["typ"], token.Header["kid"] = TokenType, signer.Fingerprint()
			mutate(token)
			encoded, err := token.SignedString(signer.key)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := signer.verifyTokenAt(encoded, now); !errors.Is(err, ErrInvalidToken) {
				t.Fatal("invalid token profile accepted")
			}
		})
	}
	for _, encoded := range []string{"", "malformed", signer.Sign([]byte("data")), strings.Repeat("x", MaximumTokenBytes+1)} {
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
