package app

import (
	"context"
	"errors"
	"time"

	"the8020/kernel/execution"
	"the8020/kernel/execution/programs"
)

// Native Git resolves named credentials through the owning application program.
type packageSecrets struct {
	context  context.Context
	programs *programs.Runner
}

func (s *packageSecrets) SecretValue(name string) (string, error) {
	result, err := s.programs.RunWithOptions(s.context, "the8020/secrets/get", "", []any{name}, nil,
		programs.Options{User: execution.SystemUser(), Timeout: 30 * time.Second})
	if err != nil {
		return "", errors.New("secret retrieval failed")
	}
	response, ok := result.Value.(map[string]any)
	if !ok {
		return "", errors.New("invalid secret response")
	}
	secret, ok := response["secret"].(map[string]any)
	if !ok {
		return "", errors.New("invalid secret response")
	}
	value, ok := secret["value"].(string)
	if !ok || value == "" {
		return "", errors.New("invalid secret response")
	}
	return value, nil
}
