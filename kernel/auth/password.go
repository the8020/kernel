package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argon2Version       = 19
	maximumArgonMemory  = 1 << 20
	maximumArgonRounds  = 1_000
	maximumPHCComponent = 1_024
)

var (
	ErrMalformedPasswordHash = errors.New("malformed Argon2id password hash")
	ErrInvalidPassword       = errors.New("invalid password")
)

type Argon2Parameters struct {
	Memory       uint32 `json:"memory"`
	Iterations   uint32 `json:"iterations"`
	Parallelism  uint8  `json:"parallelism"`
	SaltLength   uint32 `json:"salt_length"`
	OutputLength uint32 `json:"output_length"`
}

func DefaultArgon2Parameters() Argon2Parameters {
	return Argon2Parameters{Memory: 64 * 1024, Iterations: 3, Parallelism: 1, SaltLength: 16, OutputLength: 32}
}

func (p Argon2Parameters) Validate() error {
	if p.Parallelism == 0 {
		return errors.New("Argon2id parallelism must be positive")
	}
	if p.Memory < 8*uint32(p.Parallelism) || p.Memory > maximumArgonMemory {
		return fmt.Errorf("Argon2id memory must be between %d and %d KiB", 8*uint32(p.Parallelism), maximumArgonMemory)
	}
	if p.Iterations == 0 || p.Iterations > maximumArgonRounds {
		return fmt.Errorf("Argon2id iterations must be between 1 and %d", maximumArgonRounds)
	}
	if p.SaltLength < 8 || p.SaltLength > maximumPHCComponent {
		return fmt.Errorf("Argon2id salt length must be between 8 and %d bytes", maximumPHCComponent)
	}
	if p.OutputLength < 16 || p.OutputLength > maximumPHCComponent {
		return fmt.Errorf("Argon2id output length must be between 16 and %d bytes", maximumPHCComponent)
	}
	return nil
}

type PasswordHasher struct {
	parameters Argon2Parameters
	random     io.Reader
}

func NewPasswordHasher(parameters Argon2Parameters, random io.Reader) (*PasswordHasher, error) {
	if parameters == (Argon2Parameters{}) {
		parameters = DefaultArgon2Parameters()
	}
	if err := parameters.Validate(); err != nil {
		return nil, err
	}
	if random == nil {
		random = rand.Reader
	}
	return &PasswordHasher{parameters: parameters, random: random}, nil
}

func (h *PasswordHasher) Parameters() Argon2Parameters { return h.parameters }

func (h *PasswordHasher) Hash(password string) (string, error) {
	if err := validatePassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, h.parameters.SaltLength)
	if _, err := io.ReadFull(h.random, salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, h.parameters.Iterations, h.parameters.Memory, h.parameters.Parallelism, h.parameters.OutputLength)
	return encodePHC(h.parameters, salt, hash), nil
}

func (h *PasswordHasher) Verify(encoded, password string) (bool, error) {
	return h.VerifyBytes(encoded, []byte(password))
}

// VerifyBytes verifies a presented password without requiring transports such
// as SSH to create an immutable string copy of secret input.
func (h *PasswordHasher) VerifyBytes(encoded string, password []byte) (bool, error) {
	parameters, salt, expected, err := parsePHC(encoded)
	if err != nil {
		return false, err
	}
	actual := argon2.IDKey(password, salt, parameters.Iterations, parameters.Memory, parameters.Parallelism, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}

// VerifyUnknown performs the same expensive Argon2id operation used for an
// existing user so an unknown username does not have a cheap rejection path.
func (h *PasswordHasher) VerifyUnknown(password string) {
	h.VerifyUnknownBytes([]byte(password))
}

// VerifyUnknownBytes preserves the expensive unknown-user path for transports
// that receive passwords as mutable byte slices.
func (h *PasswordHasher) VerifyUnknownBytes(password []byte) {
	salt := make([]byte, h.parameters.SaltLength)
	copy(salt, []byte("the8020-auth-unknown-user"))
	expected := argon2.IDKey([]byte("not-a-real-password"), salt, h.parameters.Iterations, h.parameters.Memory, h.parameters.Parallelism, h.parameters.OutputLength)
	actual := argon2.IDKey(password, salt, h.parameters.Iterations, h.parameters.Memory, h.parameters.Parallelism, h.parameters.OutputLength)
	_ = subtle.ConstantTimeCompare(actual, expected)
}

func encodePHC(parameters Argon2Parameters, salt, hash []byte) string {
	encoding := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2Version, parameters.Memory, parameters.Iterations, parameters.Parallelism, encoding.EncodeToString(salt), encoding.EncodeToString(hash))
}

func parsePHC(encoded string) (Argon2Parameters, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return Argon2Parameters{}, nil, nil, ErrMalformedPasswordHash
	}
	parameterParts := strings.Split(parts[3], ",")
	if len(parameterParts) != 3 {
		return Argon2Parameters{}, nil, nil, ErrMalformedPasswordHash
	}
	values := map[string]uint64{}
	for _, part := range parameterParts {
		name, value, ok := strings.Cut(part, "=")
		if !ok || (name != "m" && name != "t" && name != "p") {
			return Argon2Parameters{}, nil, nil, ErrMalformedPasswordHash
		}
		if _, duplicate := values[name]; duplicate {
			return Argon2Parameters{}, nil, nil, ErrMalformedPasswordHash
		}
		parsed, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return Argon2Parameters{}, nil, nil, ErrMalformedPasswordHash
		}
		values[name] = parsed
	}
	if values["p"] > 255 {
		return Argon2Parameters{}, nil, nil, ErrMalformedPasswordHash
	}
	encoding := base64.RawStdEncoding
	salt, err := encoding.DecodeString(parts[4])
	if err != nil {
		return Argon2Parameters{}, nil, nil, ErrMalformedPasswordHash
	}
	hash, err := encoding.DecodeString(parts[5])
	if err != nil {
		return Argon2Parameters{}, nil, nil, ErrMalformedPasswordHash
	}
	parameters := Argon2Parameters{Memory: uint32(values["m"]), Iterations: uint32(values["t"]), Parallelism: uint8(values["p"]), SaltLength: uint32(len(salt)), OutputLength: uint32(len(hash))}
	if err := parameters.Validate(); err != nil {
		return Argon2Parameters{}, nil, nil, fmt.Errorf("%w: %v", ErrMalformedPasswordHash, err)
	}
	return parameters, salt, hash, nil
}

func validatePassword(password string) error {
	if password == "" {
		return fmt.Errorf("%w: must not be empty", ErrInvalidPassword)
	}
	if len(password) > 1<<20 {
		return fmt.Errorf("%w: exceeds 1 MiB", ErrInvalidPassword)
	}
	return nil
}
