package operations

import (
	"context"
	"encoding/base64"
	"errors"

	"the8020/kernel/auth"
	"the8020/kernel/execution"
)

// Bytes use standard base64 on the JSON bridge. Invalid signatures are data,
// while malformed operation inputs are errors; neither returns key material.
func (d *Dispatcher) crypto(ctx context.Context, operation string, input map[string]any) (any, error) {
	signer := d.services.Signing
	switch operation {
	case "crypto.password.digest":
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		caller, ok := execution.CallerFromContext(ctx)
		if !ok || !execution.ValidCredentialNamespace(caller.CredentialNamespace) {
			return nil, errors.New("trusted credential namespace is required")
		}
		userID, userOK := input["user_id"].(string)
		encoded, passwordOK := input["password"].(string)
		nonceEncoded, nonceOK := input["nonce"].(string)
		if !userOK || !passwordOK || !nonceOK || len(input) != 3 {
			return nil, errors.New("only user_id, password and nonce are accepted")
		}
		if len(encoded) > base64.StdEncoding.EncodedLen(1<<20) || len(nonceEncoded) != base64.StdEncoding.EncodedLen(16) {
			return nil, errors.New("password or nonce exceeds its bounds")
		}
		password, err := base64.StdEncoding.Strict().DecodeString(encoded)
		defer clear(password)
		if err != nil {
			return nil, errors.New("password must be base64")
		}
		nonce, err := base64.StdEncoding.Strict().DecodeString(nonceEncoded)
		if err != nil {
			return nil, errors.New("nonce must be base64")
		}
		digest, err := signer.PasswordDigest(caller.CredentialNamespace, userID, password, nonce)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return map[string]any{"digest": base64.StdEncoding.EncodeToString(digest)}, nil
	case "crypto.encrypt", "crypto.decrypt":
		purpose, _ := input["purpose"].(string)
		associated, ok := input["associated_data"].(string)
		if !ok {
			return nil, errors.New("associated_data must be base64")
		}
		aad, err := base64.StdEncoding.Strict().DecodeString(associated)
		if err != nil {
			return nil, errors.New("associated_data must be base64")
		}
		if operation == "crypto.decrypt" {
			encrypted, _ := input["encrypted"].(string)
			data, err := signer.Decrypt(purpose, encrypted, aad)
			return map[string]any{"data": base64.StdEncoding.EncodeToString(data)}, err
		}
		encoded, ok := input["data"].(string)
		if !ok {
			return nil, errors.New("data must be base64")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil {
			return nil, errors.New("data must be base64")
		}
		encrypted, err := signer.Encrypt(purpose, data, aad)
		return map[string]any{"encrypted": encrypted}, err
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
