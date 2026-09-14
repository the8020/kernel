package auth

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
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

// Signer holds one master seed and purpose-separated signing keys. The master
// is used only for derivation; its file is never sandbox-mounted.
type Signer struct {
	mu   sync.RWMutex
	path string
	signingKeys
}

type signingKeys struct {
	master     []byte
	session    ed25519.PrivateKey
	routing    ed25519.PrivateKey
	forwarding *tls.Certificate
}

func deriveSigningKey(master []byte, purpose string) (ed25519.PrivateKey, error) {
	seed, err := hkdf.Key(sha256.New, master, nil, "the8020/"+purpose+"/ed25519/v1", ed25519.SeedSize)
	if err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func deriveSigningKeys(master []byte) (signingKeys, error) {
	keys := signingKeys{master: master}
	var err error
	if keys.session, err = deriveSigningKey(master, "app-session-cookie"); err != nil {
		return keys, err
	}
	if keys.routing, err = deriveSigningKey(master, "service-routing"); err != nil {
		return keys, err
	}
	peer, err := deriveSigningKey(master, "node-forwarding")
	if err != nil {
		return keys, err
	}
	keys.forwarding, err = forwardingCertificate(peer)
	return keys, err
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
	master, err := decodeMasterSeed(string(value))
	if err == nil {
		signer.signingKeys, err = deriveSigningKeys(master)
	}
	return signer, err
}

func decodeMasterSeed(value string) ([]byte, error) {
	seed, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(value))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("signing key must be a base64-encoded 32-byte master seed")
	}
	return seed, nil
}

// Replace publishes a key only after its atomic file replacement succeeds.
// Replacing the key invalidates signatures made by the previous key.
func (s *Signer) Replace(value string) error {
	master, err := decodeMasterSeed(value)
	if err != nil {
		return err
	}
	keys, err := deriveSigningKeys(master)
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
	if _, err := file.WriteString(base64.StdEncoding.EncodeToString(master) + "\n"); err != nil {
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
	s.signingKeys = keys
	return nil
}

// Fingerprint identifies the master through its derived authentication public key.
func (s *Signer) Fingerprint() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return keyFingerprint(s.session)
}

func keyFingerprint(key ed25519.PrivateKey) string {
	digest := sha256.Sum256(key.Public().(ed25519.PublicKey))
	return hex.EncodeToString(digest[:])
}

func (s *Signer) String() string { return "Signer(" + s.Fingerprint() + ")" }

// appSigningKey requires a bounded application purpose, never a native one.
// The caller holds s.mu. Application keys are derived on demand, without a cache.
func (s *Signer) appSigningKey(purpose string) (ed25519.PrivateKey, error) {
	if !strings.HasPrefix(purpose, "app-") || len(purpose) <= 4 || len(purpose) > 128 {
		return nil, errors.New("signing purpose must start with app- and contain 5 to 128 lowercase ASCII letters, digits, or hyphens")
	}
	for _, ch := range purpose {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
			return nil, errors.New("signing purpose must contain only lowercase ASCII letters, digits, or hyphens")
		}
	}
	return deriveSigningKey(s.master, purpose)
}

// Sign and Verify use the caller's expected application purpose, independently
// of the JWT profile. A signature never chooses its own verification purpose.
func (s *Signer) Sign(purpose string, data []byte) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, err := s.appSigningKey(purpose)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, data)), nil
}

func (s *Signer) Verify(purpose string, data []byte, signature string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, err := s.appSigningKey(purpose)
	if err != nil {
		return false, err
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || len(decoded) != ed25519.SignatureSize {
		return false, nil
	}
	return ed25519.Verify(key.Public().(ed25519.PublicKey), data, decoded), nil
}
