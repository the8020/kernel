package auth

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestUserStore(t *testing.T, path string, now func() time.Time) *UserStore {
	t.Helper()
	hasher, err := NewPasswordHasher(testArgon2Parameters(), nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewUserStore(UserStoreConfig{Path: path, Hasher: hasher, LockTimeout: 2 * time.Second, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestUserStoreCreatesRestrictiveFileAndManagesVersions(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "config", "auth", "bootstrap-users.toml")
	store := newTestUserStore(t, path, func() time.Time { return now })
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
	}
	users, err := store.List()
	if err != nil || len(users) != 0 {
		t.Fatalf("users=%#v err=%v", users, err)
	}
	admin, err := store.Add(context.Background(), "admin", "first-password")
	if err != nil || !admin.Enabled || admin.AuthVersion != 1 || admin.PasswordHash == "first-password" {
		t.Fatalf("admin=%#v err=%v", admin, err)
	}
	if _, err := store.Add(context.Background(), "admin", "other"); !errors.Is(err, ErrDuplicateUser) {
		t.Fatalf("duplicate err=%v", err)
	}
	if _, err := store.Add(context.Background(), "Admin", "uppercase"); !errors.Is(err, ErrInvalidUsername) {
		t.Fatalf("uppercase username err=%v", err)
	}
	if _, err := store.Authenticate("admin", "first-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate("admin", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password err=%v", err)
	}
	if _, err := store.Authenticate("missing", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user err=%v", err)
	}

	now = now.Add(time.Minute)
	disabled, err := store.Disable(context.Background(), "admin")
	if err != nil || disabled.Enabled || disabled.AuthVersion != 2 {
		t.Fatalf("disabled=%#v err=%v", disabled, err)
	}
	if _, err := store.Authenticate("admin", "first-password"); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("disabled authentication err=%v", err)
	}
	enabled, err := store.Enable(context.Background(), "admin")
	if err != nil || !enabled.Enabled || enabled.AuthVersion != 2 {
		t.Fatalf("enabled=%#v err=%v", enabled, err)
	}
	now = now.Add(time.Minute)
	changed, err := store.SetPassword(context.Background(), "admin", "second-password")
	if err != nil || changed.AuthVersion != 3 || changed.PasswordHash == admin.PasswordHash {
		t.Fatalf("changed=%#v err=%v", changed, err)
	}
	if _, err := store.Authenticate("admin", "first-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password err=%v", err)
	}
	if _, err := store.Authenticate("admin", "second-password"); err != nil {
		t.Fatal(err)
	}
	invalidated, err := store.InvalidateSessions(context.Background(), "admin")
	if err != nil || invalidated.AuthVersion != 4 {
		t.Fatalf("invalidated=%#v err=%v", invalidated, err)
	}
	if err := store.Remove(context.Background(), "admin"); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.Get("admin"); err != nil || exists {
		t.Fatalf("exists=%t err=%v", exists, err)
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

func TestUserStoreConcurrentReadersAndWritersSeeCompleteDocuments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bootstrap-users.toml")
	store := newTestUserStore(t, path, time.Now)
	var wait sync.WaitGroup
	errorsChannel := make(chan error, 64)
	for index := 0; index < 16; index++ {
		wait.Add(2)
		go func(index int) {
			defer wait.Done()
			_, err := store.Add(context.Background(), "user"+string(rune('a'+index)), "password")
			errorsChannel <- err
		}(index)
		go func() {
			defer wait.Done()
			for read := 0; read < 20; read++ {
				if _, err := store.List(); err != nil {
					errorsChannel <- err
					return
				}
			}
			errorsChannel <- nil
		}()
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	users, err := store.List()
	if err != nil || len(users) != 16 {
		t.Fatalf("users=%d err=%v", len(users), err)
	}
}

func TestUserStoreInvalidTOMLIsNeverOverwrittenByMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bootstrap-users.toml")
	store := newTestUserStore(t, path, time.Now)
	corrupt := []byte("schema = [\n")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add(context.Background(), "admin", "password"); err == nil {
		t.Fatal("mutation accepted corrupt bootstrap user file")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(corrupt) {
		t.Fatalf("data=%q err=%v", data, err)
	}
}

func TestUserStoreCrossProcessWritesUseAdvisoryLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "auth", "bootstrap-users.toml")
	newTestUserStore(t, path, time.Now)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	commands := []*exec.Cmd{
		exec.Command(executable, "-test.run=TestUserStoreProcessHelper"),
		exec.Command(executable, "-test.run=TestUserStoreProcessHelper"),
	}
	outputs := make([]bytes.Buffer, len(commands))
	for index, command := range commands {
		command.Env = append(os.Environ(), "THE8020_AUTH_HELPER=1", "THE8020_AUTH_USERS_FILE="+path, "THE8020_AUTH_USERNAME=process"+string(rune('a'+index)))
		command.Stdout = &outputs[index]
		command.Stderr = &outputs[index]
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for index, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("helper failed: %v\n%s", err, outputs[index].String())
		}
	}
	store := newTestUserStore(t, path, time.Now)
	users, err := store.List()
	if err != nil || len(users) != 2 {
		t.Fatalf("users=%#v err=%v", users, err)
	}
}

func TestUserStoreProcessHelper(t *testing.T) {
	if os.Getenv("THE8020_AUTH_HELPER") != "1" {
		t.Skip("helper process")
	}
	path, username := os.Getenv("THE8020_AUTH_USERS_FILE"), os.Getenv("THE8020_AUTH_USERNAME")
	if path == "" || !strings.HasPrefix(username, "process") {
		t.Fatal("helper environment missing")
	}
	store := newTestUserStore(t, path, time.Now)
	if _, err := store.Add(context.Background(), username, "password"); err != nil {
		t.Fatal(err)
	}
}
