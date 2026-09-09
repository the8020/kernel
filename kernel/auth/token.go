package auth

import (
	"context"
	"crypto/ed25519"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"the8020/kernel/execution"
)

const (
	TokenHeader       = "the8020-authorization"
	TokenCookie       = "the8020_auth"
	TokenType         = "the8020-auth+jwt"
	TokenIssuer       = "the8020"
	TokenAudience     = "the8020"
	MaximumTokenBytes = 8192
)

var ErrInvalidToken = errors.New("invalid platform authentication token")

type TokenClaims = jwt.MapClaims

type localTransportKey struct{}

// WithLocalTransport marks native ingress or an authenticated kernel peer.
// Client-supplied HTTP headers must never call this function.
func WithLocalTransport(ctx context.Context) context.Context {
	return context.WithValue(ctx, localTransportKey{}, true)
}

func LocalTransport(ctx context.Context) bool {
	local, _ := ctx.Value(localTransportKey{}).(bool)
	return local
}

func AllowsTransport(claims TokenClaims, ctx context.Context) bool {
	return claims["transport"] != "local" || LocalTransport(ctx)
}

// TokenUser reads only the signed execution principal, never account state.
func TokenUser(claims TokenClaims) (execution.User, error) {
	subject, err := claims.GetSubject()
	if err != nil || !strings.HasPrefix(subject, "user:") {
		return execution.User{}, ErrInvalidToken
	}
	user, err := execution.UserForUsername(strings.TrimPrefix(subject, "user:"))
	if err != nil {
		return execution.User{}, ErrInvalidToken
	}
	return user, nil
}

// SignToken signs Deno-authored claims without imposing account policy.
func (s *Signer) SignToken(claims TokenClaims) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	token.Header["typ"] = TokenType
	token.Header["kid"] = keyFingerprint(s.key)
	encoded, err := token.SignedString(s.key)
	if err != nil || len(encoded) > MaximumTokenBytes {
		return "", errors.New("cannot encode platform token claims")
	}
	return encoded, nil
}

func (s *Signer) VerifyToken(encoded string) (TokenClaims, error) {
	return s.verifyTokenAt(encoded, time.Now())
}

func (s *Signer) verifyTokenAt(encoded string, now time.Time) (TokenClaims, error) {
	if encoded == "" || len(encoded) > MaximumTokenBytes {
		return nil, ErrInvalidToken
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	token, err := jwt.Parse(encoded, func(token *jwt.Token) (any, error) {
		if token.Header["typ"] != TokenType || token.Header["kid"] != keyFingerprint(s.key) {
			return nil, ErrInvalidToken
		}
		return s.key.Public().(ed25519.PublicKey), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithIssuer(TokenIssuer), jwt.WithAudience(TokenAudience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(func() time.Time { return now }),
		jwt.WithStrictDecoding())
	if err != nil || !token.Valid {
		return nil, ErrInvalidToken
	}
	claims := token.Claims.(jwt.MapClaims)
	if transport, present := claims["transport"]; present && transport != "local" && transport != "remote" {
		return nil, ErrInvalidToken
	}
	issued, err := claims.GetIssuedAt()
	expires, expiryErr := claims.GetExpirationTime()
	if err != nil || expiryErr != nil || issued == nil || expires == nil || !expires.After(issued.Time) {
		return nil, ErrInvalidToken
	}
	session, sessionOK := claims["sid"].(string)
	version, versionOK := claims["ver"].(float64)
	if !sessionOK || session == "" || len(session) > 128 || !versionOK || version < 1 || version > 9007199254740991 || math.Trunc(version) != version {
		return nil, ErrInvalidToken
	}
	if _, err := TokenUser(claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// RequestToken selects exactly one credential. Presence of a header forbids
// cookie fallback, including when that header is empty or malformed.
func RequestToken(request *http.Request) (token string, cookie bool) {
	if values, present := request.Header[http.CanonicalHeaderKey(TokenHeader)]; present {
		if len(values) != 1 {
			return "", false
		}
		parts := strings.Fields(values[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return "", false
		}
		return parts[1], false
	}
	values := request.CookiesNamed(TokenCookie)
	if len(values) != 1 {
		return "", len(values) > 0
	}
	return values[0].Value, true
}

func ClearTokenCookie(writer http.ResponseWriter, secure bool) {
	http.SetCookie(writer, &http.Cookie{Name: TokenCookie, Value: "", Path: "/", MaxAge: -1,
		Expires: time.Unix(1, 0).UTC(), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
}

// SecureTransport uses the same deployment proxy scheme contract as service URLs.
func SecureTransport(request *http.Request) bool {
	return request.TLS != nil || strings.EqualFold(strings.TrimSpace(strings.Split(request.Header.Get("X-Forwarded-Proto"), ",")[0]), "https")
}
