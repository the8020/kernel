package auth

import (
	"bytes"
	"encoding/base64"
	"path/filepath"
	"testing"
)

func TestEncryptionKeepsKeysPrivateAndAuthenticatesPurposeAndName(t *testing.T) {
	owner := testSigner(t)
	data, aad := []byte("a private credential: páss:word"), []byte("github")
	encrypted, err := owner.Encrypt("app-secret-store", data, aad)
	if err != nil {
		t.Fatal(err)
	}
	again, err := owner.Encrypt("app-secret-store", data, aad)
	if err != nil || encrypted == again {
		t.Fatal("encryption did not use independent nonces")
	}
	peer, err := OpenSigner(filepath.Join(t.TempDir(), "key"), base64.StdEncoding.EncodeToString(owner.master))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := peer.Decrypt("app-secret-store", encrypted, aad)
	if err != nil || !bytes.Equal(plain, data) {
		t.Fatal("same-key decryption failed")
	}
	for _, input := range []struct {
		signer         *Signer
		purpose, value string
		aad            []byte
	}{
		{testSigner(t), "app-secret-store", encrypted, aad},
		{owner, "app-other", encrypted, aad},
		{owner, "app-secret-store", encrypted, []byte("another-row")},
		{owner, "app-secret-store", encrypted[:len(encrypted)-4] + "AAAA", aad},
		{owner, "app-secret-store", "plaintext", aad},
	} {
		if _, err := input.signer.Decrypt(input.purpose, input.value, input.aad); err == nil {
			t.Fatal("invalid encrypted data accepted")
		}
	}
	for _, purpose := range []string{"", "app-", "node-forwarding", "app-../secret-store"} {
		if _, err := owner.Encrypt(purpose, data, aad); err == nil {
			t.Fatal("reserved or malformed purpose accepted")
		}
	}
	if _, err := owner.Encrypt("app-secret-store", make([]byte, maximumEncryptionSize+1), aad); err == nil {
		t.Fatal("unbounded input accepted")
	}
	if err := owner.Replace(base64.StdEncoding.EncodeToString(make([]byte, 32))); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Decrypt("app-secret-store", encrypted, aad); err == nil {
		t.Fatal("master replacement retained old encryption authority")
	}
}
