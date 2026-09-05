package auth

import (
	"crypto/ed25519"
	"errors"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

const RouteTokenType = "the8020-route+jwt"

var ErrInvalidRoute = errors.New("invalid persistent routing token")

// RouteTarget addresses an existing runtime execution. Its owning supervisor,
// not the token, determines service/principal ownership and execution lifetime.
type RouteTarget struct {
	NodeID      string `json:"node_id"`
	SandboxID   string `json:"sandbox_id"`
	WorkerID    string `json:"worker_id"`
	ExecutionID string `json:"execution_id"`
}

type routeClaims struct {
	RouteTarget
	jwt.RegisteredClaims
}

func (r RouteTarget) validate() error {
	for _, id := range []string{r.NodeID, r.SandboxID, r.WorkerID, r.ExecutionID} {
		if id == "" || len(id) > 256 || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\x00\r\n") {
			return ErrInvalidRoute
		}
	}
	return nil
}

func (s *Signer) SignRoute(target RouteTarget) (string, error) {
	if err := target.validate(); err != nil {
		return "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, routeClaims{
		RouteTarget:      target,
		RegisteredClaims: jwt.RegisteredClaims{Issuer: TokenIssuer, Audience: jwt.ClaimStrings{TokenAudience}},
	})
	token.Header["typ"], token.Header["kid"] = RouteTokenType, keyFingerprint(s.key)
	return token.SignedString(s.key)
}

func (s *Signer) VerifyRoute(encoded string) (RouteTarget, error) {
	if encoded == "" || len(encoded) > MaximumTokenBytes {
		return RouteTarget{}, ErrInvalidRoute
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	claims := &routeClaims{}
	token, err := jwt.ParseWithClaims(encoded, claims, func(token *jwt.Token) (any, error) {
		if token.Header["typ"] != RouteTokenType || token.Header["kid"] != keyFingerprint(s.key) {
			return nil, ErrInvalidRoute
		}
		return s.key.Public().(ed25519.PublicKey), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithIssuer(TokenIssuer), jwt.WithAudience(TokenAudience), jwt.WithStrictDecoding())
	if err != nil || !token.Valid || claims.RouteTarget.validate() != nil {
		return RouteTarget{}, ErrInvalidRoute
	}
	return claims.RouteTarget, nil
}
