// Package secrets validates credential references at native Git boundaries.
package secrets

import (
	"errors"
	"strings"
)

func normalizeName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return "", errors.New("secret name must contain 1-128 characters")
	}
	for index, character := range value {
		valid := character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			index > 0 && strings.ContainsRune("._-", character)
		if !valid {
			return "", errors.New("secret name must start with a letter or digit and contain only letters, digits, dots, underscores, or hyphens")
		}
	}
	return value, nil
}

// ValidateName applies the canonical syntax used by secret references outside
// this package.
func ValidateName(value string) error {
	normalized, err := normalizeName(value)
	if err != nil {
		return err
	}
	if normalized != value {
		return errors.New("secret name must not contain surrounding whitespace")
	}
	return nil
}
