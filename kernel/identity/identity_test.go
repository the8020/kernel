package identity

import (
	"encoding/hex"
	"testing"
)

func TestCredentialsRetainIndependentEntropy(t *testing.T) {
	first, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewToken()
	decoded, decodeErr := hex.DecodeString(first)
	if err != nil || decodeErr != nil || len(decoded) != 32 || first == second {
		t.Fatal("credential generation did not preserve the 256-bit format")
	}
}

func TestCanonicalIdentityAndTypeSeparation(t *testing.T) {
	seen := map[string]bool{}
	for _, prefix := range []string{"nod", "sbx", "wrk", "ctx", "job", "srv", "uis"} {
		for range 128 {
			id, err := New(prefix)
			if err != nil || !Is(id, prefix) || seen[id] {
				t.Fatalf("generated identity %q, error %v", id, err)
			}
			seen[id] = true
			if Is(id, "bad") {
				t.Fatalf("accepted %s as another resource type", id)
			}
		}
	}
	for _, prefix := range []string{"", "a", "abcd", "SBX", "s1x", "s-x", "éx"} {
		if _, err := New(prefix); err == nil {
			t.Fatalf("accepted invalid prefix %q", prefix)
		}
	}
	for _, value := range []string{"", "sbx-abcdefgh", "sbx-ABCDEFGHIJ", "sbx-abcdefghi_", "sbx-abcdefghijk", "sbx-abcdefghié", "sbx-abcdefghij/", "wrk-abcdefghij"} {
		if Is(value, "sbx") {
			t.Fatalf("accepted invalid sandbox identity %q", value)
		}
	}
}
