package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	platformauth "the8020/kernel/auth"
	bootstrapadd "the8020/kernel/cbus/commands/auth/bootstrapadmin/add"
	bootstrapdisable "the8020/kernel/cbus/commands/auth/bootstrapadmin/disable"
	bootstrapenable "the8020/kernel/cbus/commands/auth/bootstrapadmin/enable"
	bootstrapinvalidate "the8020/kernel/cbus/commands/auth/bootstrapadmin/invalidatesessions"
	bootstraplist "the8020/kernel/cbus/commands/auth/bootstrapadmin/list"
	bootstrapremove "the8020/kernel/cbus/commands/auth/bootstrapadmin/remove"
	bootstrapsetpassword "the8020/kernel/cbus/commands/auth/bootstrapadmin/setpassword"
	sessioncleanup "the8020/kernel/cbus/commands/auth/session/cleanup"
	sessionlist "the8020/kernel/cbus/commands/auth/session/list"
	sessionrevoke "the8020/kernel/cbus/commands/auth/session/revoke"
	sessionrevokeuser "the8020/kernel/cbus/commands/auth/session/revokeuser"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func request(arguments map[string]any) core.Request { return core.Request{Arguments: arguments} }

func invoke(t *testing.T, handler core.Handler, arguments map[string]any) core.Result {
	t.Helper()
	result, err := handler(context.Background(), request(arguments))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestAuthenticationCommandHandlersAndNonDisclosure(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	manager, err := platformauth.New(platformauth.Config{
		UsersFile: filepath.Join(root, "config", "auth", "bootstrap-users.toml"), SessionsRoot: filepath.Join(root, "state", "auth", "bootstrap-sessions"),
		SessionDuration: time.Minute, CleanupInterval: time.Hour, Now: func() time.Time { return now },
		Argon2: platformauth.Argon2Parameters{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, OutputLength: 16},
	})
	if err != nil {
		t.Fatal(err)
	}
	serviceSet := &services.Services{Auth: manager}

	added := invoke(t, bootstrapadd.New(serviceSet), map[string]any{"username": "admin", "password": "initial-password"})
	listed := invoke(t, bootstraplist.New(serviceSet), nil)
	encoded, err := json.Marshal([]any{added, listed})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"initial-password", "password_hash", "secret_hash"} {
		if string(encoded) != "" && contains(string(encoded), forbidden) {
			t.Fatalf("command result disclosed %q: %s", forbidden, encoded)
		}
	}

	login, err := manager.BootstrapLogin(context.Background(), "admin", "initial-password", false)
	if err != nil || !login.Authenticated {
		t.Fatalf("login = %#v, error = %v", login, err)
	}
	sessions := invoke(t, sessionlist.New(serviceSet), nil)
	sessionData, _ := json.Marshal(sessions)
	if contains(string(sessionData), "secret") || contains(string(sessionData), login.SetCookie) {
		t.Fatalf("authentication-session list disclosed credentials: %s", sessionData)
	}
	summaries := sessions["authentication_sessions"].([]platformauth.SessionSummary)
	if len(summaries) != 1 {
		t.Fatalf("authentication sessions = %#v", summaries)
	}
	invoke(t, sessionrevoke.New(serviceSet), map[string]any{"session_id": summaries[0].SessionID})

	for index := 0; index < 2; index++ {
		if result, loginErr := manager.BootstrapLogin(context.Background(), "admin", "initial-password", false); loginErr != nil || !result.Authenticated {
			t.Fatalf("login %d = %#v, error = %v", index, result, loginErr)
		}
	}
	revoked := invoke(t, sessionrevokeuser.New(serviceSet), map[string]any{"username": "admin"})
	if revoked["revoked_count"] != 2 {
		t.Fatalf("revoked user sessions = %#v", revoked)
	}

	login, err = manager.BootstrapLogin(context.Background(), "admin", "initial-password", false)
	if err != nil || !login.Authenticated {
		t.Fatalf("cleanup login = %#v, error = %v", login, err)
	}
	now = now.Add(2 * time.Minute)
	cleaned := invoke(t, sessioncleanup.New(serviceSet), nil)
	if cleaned["removed_count"] != 1 {
		t.Fatalf("cleanup result = %#v", cleaned)
	}

	invoke(t, bootstrapdisable.New(serviceSet), map[string]any{"username": "admin"})
	invoke(t, bootstrapenable.New(serviceSet), map[string]any{"username": "admin"})
	invoke(t, bootstrapsetpassword.New(serviceSet), map[string]any{"username": "admin", "password": "replacement-password"})
	invoke(t, bootstrapinvalidate.New(serviceSet), map[string]any{"username": "admin"})
	invoke(t, bootstrapremove.New(serviceSet), map[string]any{"username": "admin"})

	_, err = bootstrapadd.New(serviceSet)(context.Background(), request(map[string]any{"username": "bad\nname", "password": "password"}))
	var commandError *core.Error
	if !errors.As(err, &commandError) || commandError.Code != core.CodeInvalidArguments {
		t.Fatalf("invalid username error = %#v", err)
	}
	_, err = sessionrevoke.New(serviceSet)(context.Background(), request(map[string]any{"session_id": "not-an-id"}))
	if !errors.As(err, &commandError) || commandError.Code != core.CodeInvalidArguments {
		t.Fatalf("invalid session ID error = %#v", err)
	}
}

func contains(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
