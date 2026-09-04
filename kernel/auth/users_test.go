package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestUserStoreAuthenticatesPackageOwnedRows(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
	manager := newTestManager(t, t.TempDir(), clock, time.Minute)
	addTestUser(t, manager, "admin", "password")

	user, err := manager.users.Authenticate("admin", "password")
	if err != nil || user.Username != "admin" || !user.Enabled || user.AuthVersion != 1 || user.PasswordHash == "password" {
		t.Fatalf("user=%#v err=%v", user, err)
	}
	if _, err := manager.users.Authenticate("admin", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password err=%v", err)
	}
	if _, err := manager.users.Authenticate("missing", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user err=%v", err)
	}
}

func TestUsernameValidationMatchesLinuxIdentityContract(t *testing.T) {
	for _, username := range []string{"abc", "alice", strings.Repeat("a", 32), "123"} {
		if err := ValidateUsername(username); err != nil {
			t.Errorf("valid username %q: %v", username, err)
		}
	}
	for _, username := range []string{"", "ab", strings.Repeat("a", 33), "Alice", "alice1!", "alice-smith", "管理者"} {
		if err := ValidateUsername(username); !errors.Is(err, ErrInvalidUsername) {
			t.Errorf("invalid username %q: %v", username, err)
		}
	}
}
