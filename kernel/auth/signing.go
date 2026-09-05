package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const SigningKeyEnvironment = "THE8020_SIGNING_KEY"

// Signer holds the deployment's private key. Its file is never sandbox-mounted.
// Provisioned values are standard base64-encoded 32-byte Ed25519 seeds.
type Signer struct {
	mu   sync.RWMutex
	path string
	key  ed25519.PrivateKey
}

// OpenSigner applies a startup environment override, otherwise loads the file,
// generating it only when absent. An override is persisted like a command change.
func OpenSigner(path, override string) (*Signer, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("signing key path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create signing key directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	signer := &Signer{path: path}
	if override != "" {
		return signer, signer.Replace(override)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		return signer, signer.Replace(base64.StdEncoding.EncodeToString(seed))
	}
	if err != nil {
		return nil, fmt.Errorf("inspect signing key: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 1024 {
		return nil, errors.New("signing key must be a regular private seed file")
	}
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	signer.key, err = decodeSigningKey(string(value))
	return signer, err
}

func decodeSigningKey(value string) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(value))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("signing key must be a base64-encoded 32-byte Ed25519 seed")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// Replace publishes a key only after its atomic file replacement succeeds.
// Replacing the key invalidates signatures made by the previous key.
func (s *Signer) Replace(value string) error {
	key, err := decodeSigningKey(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := os.CreateTemp(filepath.Dir(s.path), ".signing-key-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.WriteString(base64.StdEncoding.EncodeToString(key.Seed()) + "\n"); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), s.path); err != nil {
		return err
	}
	s.key = key
	return nil
}

func (s *Signer) Fingerprint() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return keyFingerprint(s.key)
}

func keyFingerprint(key ed25519.PrivateKey) string {
	digest := sha256.Sum256(key.Public().(ed25519.PublicKey))
	return hex.EncodeToString(digest[:])
}

func (s *Signer) String() string { return "Signer(" + s.Fingerprint() + ")" }

// Sign and Verify operate on arbitrary bytes, independently of the JWT profile.
func (s *Signer) Sign(data []byte) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.key, data))
}

func (s *Signer) Verify(data []byte, signature string) bool {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || len(decoded) != ed25519.SignatureSize {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return ed25519.Verify(s.key.Public().(ed25519.PublicKey), data, decoded)
}
