package operations

import (
	"path/filepath"
	"testing"
	"time"

	"the8020/kernel/auth"
	"the8020/kernel/services"
)

func TestCryptoBridgeRequiresApplicationPurpose(t *testing.T) {
	signer, err := auth.OpenSigner(filepath.Join(t.TempDir(), "key"), "")
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := New(&services.Services{Signing: signer})
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"purpose": "app-example", "data": "AQID"}
	result, err := dispatcher.Execute(t.Context(), "crypto.sign", input)
	if err != nil {
		t.Fatal(err)
	}
	input["signature"] = result.(map[string]any)["signature"]
	for _, purpose := range []string{"app-example", "app-other"} {
		input["purpose"] = purpose
		result, err := dispatcher.Execute(t.Context(), "crypto.verify", input)
		if err != nil || result.(map[string]any)["valid"] != (purpose == "app-example") {
			t.Fatalf("verification ignored expected purpose: %v", err)
		}
	}
	for _, purpose := range []any{nil, 3, "", "app-", "node-forwarding", "service-routing", "app-../service-routing"} {
		input["purpose"] = purpose
		for _, op := range []string{"crypto.sign", "crypto.verify"} {
			if _, err := dispatcher.Execute(t.Context(), op, input); err == nil {
				t.Fatalf("%s accepted invalid purpose", op)
			}
		}
	}
	// Dedicated JWT issuance remains available to trusted packages; session
	// representation is package-owned and needs no database for cryptography.
	result, err = dispatcher.Execute(t.Context(), "crypto.token.sign", map[string]any{"claims": map[string]any{
		"iss": auth.TokenIssuer, "aud": auth.TokenAudience, "sub": "user:alice",
		"iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Minute).Unix(),
		"session": map[string]any{"handle": "next-format"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := dispatcher.Execute(t.Context(), "crypto.token.verify", result.(map[string]any))
	if err != nil || claims.(auth.TokenClaims)["session"].(map[string]any)["handle"] != "next-format" {
		t.Fatalf("token bridge lost opaque session claims: %v", err)
	}
}
