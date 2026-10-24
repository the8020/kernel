package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
)

func TestPasswordDigestSeparationAndRestart(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	path := filepath.Join(t.TempDir(), "key")
	signer, err := OpenSigner(path, base64.StdEncoding.EncodeToString(seed))
	if err != nil {
		t.Fatal(err)
	}
	password := []byte("unicode-🔐\x00exact")
	nonce := make([]byte, 16)
	for i := range nonce {
		nonce[i] = byte(i)
	}
	digest, err := signer.PasswordDigest("package:the8020/users", "alice", password, nonce)
	if err != nil || hex.EncodeToString(digest) != "4a4d1de8440b6de1a0471b8cb75f529e4426c75d9f696725b42d0ae020e2f50c" {
		t.Fatal("password derivation differs from independent HMAC/HKDF vector", err)
	}
	restarted, err := OpenSigner(path, "")
	if err != nil {
		t.Fatal(err)
	}
	again, err := restarted.PasswordDigest("package:the8020/users", "alice", password, nonce)
	if err != nil || !bytes.Equal(digest, again) {
		t.Fatal("restart changed digest", err)
	}
	for _, pair := range [][2]string{{"package:the8020/users", "other"}, {"service:example/custom/login", "alice"}, {"package:the8020/usersa", "lice"}, {"package:the8020/users", "Alice"}} {
		other, err := signer.PasswordDigest(pair[0], pair[1], password, nonce)
		if err != nil || bytes.Equal(digest, other) {
			t.Fatal("identity separation failed", err)
		}
	}
	if bytes.Equal(digest, seed) || bytes.Equal(digest, signer.session.Seed()) {
		t.Fatal("private key exported")
	}
	for _, value := range []string{"", strings.Repeat("x", 257), string([]byte{255})} {
		if _, err := signer.PasswordDigest("valid", value, password, nonce); err == nil {
			t.Fatal("invalid user ID accepted")
		}
		if _, err := signer.PasswordDigest(value, "valid", password, nonce); err == nil {
			t.Fatal("invalid namespace accepted")
		}
	}
	for _, invalid := range [][]byte{nil, bytes.Repeat([]byte("x"), (1<<20)+1), {255}} {
		if _, err := signer.PasswordDigest("valid", "valid", invalid, nonce); err == nil {
			t.Fatal("invalid password accepted")
		}
	}
	for _, size := range []int{0, 15, 17} {
		if _, err := signer.PasswordDigest("valid", "valid", password, make([]byte, size)); err == nil {
			t.Fatal("invalid nonce accepted")
		}
	}
	if _, err := signer.PasswordDigest(strings.Repeat("n", 256), strings.Repeat("u", 256), bytes.Repeat([]byte("x"), 1<<20), nonce); err != nil {
		t.Fatal("valid bounds rejected", err)
	}
	other, err := signer.PasswordDigest("package:the8020/users", "alice", []byte("wrong"), nonce)
	if err != nil || bytes.Equal(digest, other) {
		t.Fatal("password not bound", err)
	}
	other, err = signer.PasswordDigest("package:the8020/users", "alice", password, bytes.Repeat([]byte{1}, 16))
	if err != nil || bytes.Equal(digest, other) {
		t.Fatal("nonce not bound", err)
	}
	if err := signer.Replace(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))); err != nil {
		t.Fatal(err)
	}
	changed, err := signer.PasswordDigest("package:the8020/users", "alice", password, nonce)
	if err != nil || bytes.Equal(digest, changed) {
		t.Fatal("replacement retained old digest", err)
	}
}
