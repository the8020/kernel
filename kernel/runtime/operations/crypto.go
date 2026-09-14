package operations

import (
	"encoding/base64"
	"errors"

	"the8020/kernel/auth"
)

// Bytes use standard base64 on the JSON bridge. Invalid signatures are data,
// while malformed operation inputs are errors; neither returns key material.
func (d *Dispatcher) crypto(operation string, input map[string]any) (any, error) {
	signer := d.services.Signing
	switch operation {
	case "crypto.token.sign":
		claims, ok := input["claims"].(map[string]any)
		if !ok {
			return nil, errors.New("token claims must be an object")
		}
		token, err := signer.SignToken(auth.TokenClaims(claims))
		return map[string]any{"token": token}, err
	case "crypto.token.verify":
		token, _ := input["token"].(string)
		claims, err := signer.VerifyToken(token)
		if err != nil {
			return nil, nil
		}
		return claims, nil
	case "crypto.sign", "crypto.verify":
		purpose, _ := input["purpose"].(string)
		encoded, ok := input["data"].(string)
		if !ok {
			return nil, errors.New("data must be base64")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil {
			return nil, errors.New("data must be base64")
		}
		if operation == "crypto.sign" {
			signature, err := signer.Sign(purpose, data)
			return map[string]any{"signature": signature}, err
		}
		signature, _ := input["signature"].(string)
		valid, err := signer.Verify(purpose, data, signature)
		return map[string]any{"valid": valid}, err
	default:
		return nil, errors.New("unknown crypto operation")
	}
}
