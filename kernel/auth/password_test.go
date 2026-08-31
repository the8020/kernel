package auth

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

func testArgon2Parameters() Argon2Parameters {
	return Argon2Parameters{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, OutputLength: 32}
}

func TestArgon2idPHCCreationParsingAndVerification(t *testing.T) {
	defaults := DefaultArgon2Parameters()
	if defaults.Memory != 64*1024 || defaults.Iterations != 3 || defaults.Parallelism != 1 || defaults.SaltLength != 16 || defaults.OutputLength != 32 {
		t.Fatalf("defaults=%#v", defaults)
	}
	hasher, err := NewPasswordHasher(testArgon2Parameters(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	first, err := hasher.Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	second, err := hasher.Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.HasPrefix(first, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("first=%q second=%q", first, second)
	}
	parameters, salt, output, err := parsePHC(first)
	if err != nil || parameters != testArgon2Parameters() || len(salt) != 16 || len(output) != 32 {
		t.Fatalf("parameters=%#v salt=%d output=%d err=%v", parameters, len(salt), len(output), err)
	}
	if valid, err := hasher.Verify(first, "correct horse battery staple"); err != nil || !valid {
		t.Fatalf("correct password valid=%t err=%v", valid, err)
	}
	if valid, err := hasher.Verify(first, "wrong"); err != nil || valid {
		t.Fatalf("wrong password valid=%t err=%v", valid, err)
	}
}

func TestArgon2idPHCRejectsMalformedOrDangerousParameters(t *testing.T) {
	for _, encoded := range []string{
		"",
		"$argon2i$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=18$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=64,t=1,p=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=999999999,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=64,t=0,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=64,t=1,p=0$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=64,t=1,p=1$not base64$aGFzaGhhc2hoYXNoaGFzaA",
	} {
		if _, _, _, err := parsePHC(encoded); !errors.Is(err, ErrMalformedPasswordHash) {
			t.Fatalf("hash %q err=%v", encoded, err)
		}
	}
}
