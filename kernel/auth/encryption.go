package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

const maximumEncryptionSize = 1 << 20

// Application encryption uses a separate derived key; no key leaves the signer.
func (s *Signer) encryptionCipher(purpose string) (cipher.AEAD, error) {
	if err := validateAppPurpose(purpose); err != nil {
		return nil, err
	}
	key, err := hkdf.Key(sha256.New, s.master, nil, "the8020/"+purpose+"/aes-256-gcm/v1", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func (s *Signer) Encrypt(purpose string, data, associatedData []byte) (string, error) {
	if len(data) > maximumEncryptionSize || len(associatedData) > maximumEncryptionSize {
		return "", errors.New("encryption input exceeds 1 MiB")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	aead, err := s.encryptionCipher(purpose)
	if err != nil {
		return "", err
	}
	return "v1:" + base64.StdEncoding.EncodeToString(aead.Seal(nil, nil, data, associatedData)), nil
}

func (s *Signer) Decrypt(purpose, encrypted string, associatedData []byte) ([]byte, error) {
	if len(encrypted) > base64.StdEncoding.EncodedLen(maximumEncryptionSize+28)+3 || len(associatedData) > maximumEncryptionSize {
		return nil, errors.New("decryption input exceeds 1 MiB")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	aead, err := s.encryptionCipher(purpose)
	if err != nil {
		return nil, err
	}
	encoded, version := strings.CutPrefix(encrypted, "v1:")
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if !version || err != nil {
		return nil, errors.New("invalid encrypted data")
	}
	data, err = aead.Open(nil, nil, data, associatedData)
	if err != nil {
		return nil, errors.New("encrypted data cannot be decrypted")
	}
	return data, nil
}
