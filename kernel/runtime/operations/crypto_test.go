package operations

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"the8020/kernel/auth"
	"the8020/kernel/execution"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/services"
)

func TestPasswordDigestUsesOnlyTrustedNamespace(t *testing.T) {
	signer, err := auth.OpenSigner(filepath.Join(t.TempDir(), "key"), "")
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := New(&services.Services{Signing: signer})
	if err != nil {
		t.Fatal(err)
	}
	caller := execution.Caller{ContextID: "ctx-aaaaaaaaaa", Workload: model.WorkloadService, User: execution.SystemUser(), CredentialNamespace: "service:example/a/login"}
	ctx := execution.WithCaller(t.Context(), caller)
	input := map[string]any{"user_id": "alice", "password": base64.StdEncoding.EncodeToString([]byte("fixture-password")), "nonce": base64.StdEncoding.EncodeToString(make([]byte, 16))}
	first, err := dispatcher.Execute(ctx, "crypto.password.digest", input)
	if err != nil || len(first.(map[string]any)) != 1 {
		t.Fatal("derivation failed", err)
	}
	caller.CredentialNamespace = "service:example/b/login"
	other, err := dispatcher.Execute(execution.WithCaller(t.Context(), caller), "crypto.password.digest", input)
	if err != nil || first.(map[string]any)["digest"] == other.(map[string]any)["digest"] {
		t.Fatal("namespaces share digests", err)
	}
	input["namespace"] = caller.CredentialNamespace
	if _, err := dispatcher.Execute(ctx, "crypto.password.digest", input); err == nil {
		t.Fatal("caller selected another namespace")
	}
	delete(input, "namespace")
	if _, err := dispatcher.Execute(t.Context(), "crypto.password.digest", input); err == nil {
		t.Fatal("unidentified caller accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := dispatcher.Execute(cancelled, "crypto.password.digest", input); err != context.Canceled {
		t.Fatal("cancelled derivation accepted", err)
	}
	for _, change := range []struct {
		key   string
		value any
	}{
		{"user_id", nil}, {"user_id", ""}, {"password", nil}, {"password", ""},
		{"password", "credential-marker%%%"}, {"password", strings.Repeat("A", base64.StdEncoding.EncodedLen(1<<20)+1)},
		{"password", base64.StdEncoding.EncodeToString([]byte{255})},
		{"nonce", nil}, {"nonce", ""}, {"nonce", strings.Repeat("!", 24)},
		{"nonce", base64.StdEncoding.EncodeToString(make([]byte, 17))},
	} {
		previous := input[change.key]
		input[change.key] = change.value
		_, err := dispatcher.Execute(ctx, "crypto.password.digest", input)
		input[change.key] = previous
		if err == nil || strings.Contains(err.Error(), "credential-marker") {
			t.Fatal("invalid input accepted or exposed")
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(first.(map[string]any)["digest"].(string))
	if err != nil || len(decoded) != 32 {
		t.Fatal("expected only a final SHA-256 digest", err)
	}

}

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
	input["associated_data"] = "bmFtZQ=="
	encrypted, err := dispatcher.Execute(t.Context(), "crypto.encrypt", input)
	if err != nil {
		t.Fatal(err)
	}
	input["encrypted"] = encrypted.(map[string]any)["encrypted"]
	decrypted, err := dispatcher.Execute(t.Context(), "crypto.decrypt", input)
	if err != nil || decrypted.(map[string]any)["data"] != "AQID" {
		t.Fatalf("encryption bridge roundtrip failed: %v", err)
	}
	input["associated_data"] = "b3RoZXI="
	if _, err := dispatcher.Execute(t.Context(), "crypto.decrypt", input); err == nil {
		t.Fatal("decryption ignored associated data")
	}
	input["associated_data"] = "bmFtZQ=="
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
		for _, op := range []string{"crypto.sign", "crypto.verify", "crypto.encrypt", "crypto.decrypt"} {
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
