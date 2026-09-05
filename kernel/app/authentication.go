package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"the8020/kernel/auth"
	"the8020/kernel/execution"
	"the8020/kernel/execution/programs"
)

const authenticationModule = "/p/the8020/users/mod.ts"
const authenticationProgram = "the8020/users/authenticate"

// Native transports delegate application eligibility to an ordinary package
// program. HTTP service hooks instead run inside their existing target Worker.
type packageAuthentication struct {
	context  context.Context
	programs *programs.Runner
	signing  *auth.Signer
}

func (a *packageAuthentication) run(ctx context.Context, mode, username string, secrets map[string]string) (execution.User, error) {
	user, err := execution.UserForUsername(username)
	if err != nil {
		return execution.User{}, errors.New("authentication failed")
	}
	result, err := a.programs.RunWithOptions(ctx, authenticationProgram, "", []any{mode, username}, secrets,
		programs.Options{User: execution.SystemUser(), Timeout: 30 * time.Second})
	if err != nil {
		return execution.User{}, errors.New("authentication unavailable")
	}
	if approved, ok := result.Value.(bool); !ok || !approved {
		return execution.User{}, errors.New("authentication failed")
	}
	return user, nil
}

func (a *packageAuthentication) AuthenticatePassword(username string, password []byte) (execution.User, error) {
	return a.run(a.context, "password", username, map[string]string{"password": string(password)})
}

func (a *packageAuthentication) AuthenticateUser(username string) (execution.User, error) {
	return a.run(a.context, "user", username, nil)
}

func (a *packageAuthentication) AuthenticateToken(ctx context.Context, token string) (execution.User, error) {
	claims, err := a.signing.VerifyToken(token)
	if err != nil {
		return execution.User{}, err
	}
	encoded, err := json.Marshal(claims)
	if err != nil {
		return execution.User{}, auth.ErrInvalidToken
	}
	user, err := a.run(ctx, "session", strings.TrimPrefix(claims["sub"].(string), "user:"), map[string]string{"claims": string(encoded)})
	if err == nil && user.ID != claims["sub"] {
		return execution.User{}, auth.ErrInvalidToken
	}
	return user, err
}
