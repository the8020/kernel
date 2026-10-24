package auth

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"unicode/utf8"
)

// PasswordDigest returns only the final verifier, never key material.
// The runtime supplies the trusted credential namespace; identifiers are exact.
func (s *Signer) PasswordDigest(namespace, userID string, password, nonce []byte) ([]byte, error) {
	if len(namespace) == 0 || len(namespace) > 256 || len(userID) == 0 || len(userID) > 256 || !utf8.ValidString(namespace) || !utf8.ValidString(userID) {
		return nil, errors.New("credential namespace and user ID must contain 1 to 256 UTF-8 bytes")
	}
	if len(password) == 0 || len(password) > 1<<20 || !utf8.Valid(password) {
		return nil, errors.New("password must contain 1 to 1048576 UTF-8 bytes")
	}
	if len(nonce) != 16 {
		return nil, errors.New("password nonce must contain 16 bytes")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, err := hkdf.Key(sha256.New, s.master, nil, "the8020/salt/hmac-sha256/v1", 32)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	mac := hmac.New(sha256.New, key)
	for _, part := range []string{namespace, userID} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(part)))
		mac.Write(length[:])
		mac.Write([]byte(part))
	}
	secret := mac.Sum(nil)
	defer clear(secret)
	digest := hmac.New(sha256.New, secret)
	digest.Write([]byte("the8020/password/hmac-sha256/v1\x00"))
	digest.Write(nonce)
	digest.Write(password)
	return digest.Sum(nil), nil
}
