// Package identity defines the shared format of opaque operational identifiers.
package identity

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

const SuffixLength = 10

// NewToken creates a separate 256-bit credential, never a short operational ID.
func NewToken() (string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", fmt.Errorf("generate credential: %w", err)
	}
	return hex.EncodeToString(secret[:]), nil
}

// New creates an identifier. The registering owner must still reject collisions.
// These identifiers are not credentials and must never authorize an operation.
func New(prefix string) (string, error) {
	if !validPrefix(prefix) {
		return "", errors.New("identity prefix must contain three lowercase letters")
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var result [4 + SuffixLength]byte
	copy(result[:], prefix)
	result[3] = '-'
	var random [32]byte
	for offset := 4; offset < len(result); {
		if _, err := rand.Read(random[:]); err != nil {
			return "", fmt.Errorf("generate %s identity: %w", prefix, err)
		}
		for _, value := range random {
			// Rejection sampling avoids bias from reducing arbitrary bytes modulo 36.
			if value >= 252 {
				continue
			}
			result[offset] = alphabet[int(value)%len(alphabet)]
			offset++
			if offset == len(result) {
				break
			}
		}
	}
	return string(result[:]), nil
}

// Is checks the entire canonical encoding and the expected resource type.
func Is(value, prefix string) bool {
	if !validPrefix(prefix) || len(value) != 4+SuffixLength || value[:3] != prefix || value[3] != '-' {
		return false
	}
	for _, character := range value[4:] {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func validPrefix(value string) bool {
	if len(value) != 3 {
		return false
	}
	for _, character := range value {
		if character < 'a' || character > 'z' {
			return false
		}
	}
	return true
}
