package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Time() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func newTestManager(t *testing.T, root string, clock *testClock, cleanup time.Duration) *Manager {
	t.Helper()
	manager, err := New(Config{
		UsersFile:       filepath.Join(root, "config", "auth", "bootstrap-users.toml"),
		SessionsRoot:    filepath.Join(root, "state", "auth", "bootstrap-sessions"),
		SessionDuration: time.Hour,
		CleanupInterval: cleanup,
		Cookie:          CookieConfig{Name: "the8020_auth", Secure: false, SameSite: "lax"},
		Argon2:          testArgon2Parameters(),
		LockTimeout:     time.Second,
		Now:             clock.Time,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func cookieFromHeader(t *testing.T, header string) *http.Cookie {
	t.Helper()
	response := &http.Response{Header: http.Header{"Set-Cookie": []string{header}}}
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("Set-Cookie %q parsed as %#v", header, cookies)
	}
	return cookies[0]
}

func TestOpaqueAuthenticationSessionIsSharedAndNeverStoresSecret(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
	nodeA := newTestManager(t, root, clock, time.Minute)
	if _, err := nodeA.AddUser(context.Background(), "admin", "password"); err != nil {
		t.Fatal(err)
	}
	first, err := nodeA.BootstrapLogin(context.Background(), "admin", "password", true)
	if err != nil || !first.Authenticated || first.User == nil || first.User.ID != "bootstrap-admin:admin" || first.Error != "" {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	cookie := cookieFromHeader(t, first.SetCookie)
	if cookie.Name != "the8020_auth" || !cookie.HttpOnly || !cookie.Secure || cookie.Path != "/" || cookie.SameSite != http.SameSiteLaxMode || !strings.HasPrefix(cookie.Value, "v1.") {
		t.Fatalf("cookie=%#v header=%q", cookie, first.SetCookie)
	}
	sessionID, secret, err := parseSessionToken(cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "state", "auth", "bootstrap-sessions", sessionID[:2], sessionID+".toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) || !strings.Contains(string(data), "secret_hash = ") || !strings.Contains(string(data), "sha256:") {
		t.Fatalf("session file disclosed secret or omitted hash: %s", data)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
	}

	nodeB := newTestManager(t, root, clock, time.Minute)
	contextValue, err := nodeB.ValidateCookie(cookie.Value)
	if err != nil || !contextValue.Authenticated || contextValue.Username != "admin" || contextValue.UserID != "bootstrap-admin:admin" || contextValue.SessionID != sessionID {
		t.Fatalf("context=%#v err=%v", contextValue, err)
	}
	second, err := nodeA.BootstrapLogin(context.Background(), "admin", "password", false)
	if err != nil || cookieFromHeader(t, second.SetCookie).Value == cookie.Value {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	badSecret := cookie.Value[:len(cookie.Value)-1] + map[bool]string{true: "1", false: "0"}[strings.HasSuffix(cookie.Value, "0")]
	if _, err := nodeB.ValidateCookie(badSecret); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("invalid secret err=%v", err)
	}
	if _, err := nodeB.ValidateCookie("v1.malformed"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("malformed cookie err=%v", err)
	}
	if err := nodeA.RevokeSession(sessionID); err != nil {
		t.Fatal(err)
	}
	if err := nodeA.RevokeSession(sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := nodeB.ValidateCookie(cookie.Value); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked cookie err=%v", err)
	}
}

func TestAuthenticationVersionDisablePasswordChangeAndRemovalInvalidateSessions(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
	manager := newTestManager(t, root, clock, time.Minute)
	if _, err := manager.AddUser(context.Background(), "admin", "first"); err != nil {
		t.Fatal(err)
	}
	login := func(password string) string {
		result, err := manager.BootstrapLogin(context.Background(), "admin", password, false)
		if err != nil || !result.Authenticated {
			t.Fatalf("login=%#v err=%v", result, err)
		}
		return cookieFromHeader(t, result.SetCookie).Value
	}

	versionCookie := login("first")
	if _, err := manager.users.InvalidateSessions(context.Background(), "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ValidateCookie(versionCookie); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("version-mismatched cookie err=%v", err)
	}
	passwordCookie := login("first")
	changed, err := manager.SetPassword(context.Background(), "admin", "second")
	if err != nil || changed.AuthVersion != 3 {
		t.Fatalf("changed=%#v err=%v", changed, err)
	}
	if _, err := manager.ValidateCookie(passwordCookie); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("password-change cookie err=%v", err)
	}
	if result, err := manager.BootstrapLogin(context.Background(), "admin", "first", false); err != nil || result.Error != "invalid_credentials" || result.Authenticated {
		t.Fatalf("old password result=%#v err=%v", result, err)
	}
	disabledCookie := login("second")
	disabled, err := manager.DisableUser(context.Background(), "admin")
	if err != nil || disabled.Enabled || disabled.AuthVersion != 4 {
		t.Fatalf("disabled=%#v err=%v", disabled, err)
	}
	if _, err := manager.ValidateCookie(disabledCookie); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("disabled cookie err=%v", err)
	}
	if result, err := manager.BootstrapLogin(context.Background(), "admin", "second", false); err != nil || result.Error != "disabled" || result.Authenticated {
		t.Fatalf("disabled login=%#v err=%v", result, err)
	}
	if _, err := manager.EnableUser(context.Background(), "admin"); err != nil {
		t.Fatal(err)
	}
	removedCookie := login("second")
	if err := manager.RemoveUser(context.Background(), "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ValidateCookie(removedCookie); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("removed-user cookie err=%v", err)
	}
	sessions, err := manager.ListSessions()
	if err != nil || len(sessions) != 0 {
		t.Fatalf("sessions=%#v err=%v", sessions, err)
	}
}

func TestAuthenticationSessionExpirationLazyAndPeriodicCleanup(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
	manager := newTestManager(t, root, clock, 5*time.Millisecond)
	if _, err := manager.AddUser(context.Background(), "admin", "password"); err != nil {
		t.Fatal(err)
	}
	login, err := manager.BootstrapLogin(context.Background(), "admin", "password", false)
	if err != nil {
		t.Fatal(err)
	}
	first := cookieFromHeader(t, login.SetCookie).Value
	firstID, _, _ := parseSessionToken(first)
	clock.Advance(2 * time.Hour)
	if _, err := manager.ValidateCookie(first); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired cookie err=%v", err)
	}
	if _, err := manager.sessions.Read(firstID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired session still exists: %v", err)
	}

	second, _, err := manager.sessions.Create("admin", 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute)
	var wait sync.WaitGroup
	errorsChannel := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := manager.CleanupExpired()
			errorsChannel <- err
		}()
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.sessions.Read(second.SessionID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("concurrent cleanup left session: %v", err)
	}

	third, _, err := manager.sessions.Create("admin", 1, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		manager.RunCleanup(ctx, func(err error) { t.Errorf("cleanup: %v", err) })
		close(done)
	}()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := manager.sessions.Read(third.SessionID); errors.Is(err, os.ErrNotExist) {
			cancel()
			<-done
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("periodic cleanup did not remove expired authentication session")
}

func TestLogoutCookieUserAndSessionSummariesDoNotExposeSecrets(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
	manager := newTestManager(t, root, clock, time.Minute)
	if _, err := manager.AddUser(context.Background(), "admin", "password"); err != nil {
		t.Fatal(err)
	}
	login, err := manager.BootstrapLogin(context.Background(), "admin", "password", false)
	if err != nil {
		t.Fatal(err)
	}
	cookie := cookieFromHeader(t, login.SetCookie)
	authContext, err := manager.ValidateCookie(cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	users, err := manager.ListUsers()
	if err != nil || len(users) != 1 || users[0].ActiveSessions != 1 {
		t.Fatalf("users=%#v err=%v", users, err)
	}
	sessions, err := manager.ListSessions()
	if err != nil || len(sessions) != 1 || !sessions[0].Valid {
		t.Fatalf("sessions=%#v err=%v", sessions, err)
	}
	encoded, err := json.Marshal(struct {
		Users    []UserSummary    `json:"users"`
		Sessions []SessionSummary `json:"sessions"`
	}{users, sessions})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "password") || strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), cookie.Value) {
		t.Fatalf("summary disclosed authentication secret: %s", encoded)
	}
	logout, err := manager.LogoutCurrent(authContext, true)
	if err != nil {
		t.Fatal(err)
	}
	clearing := cookieFromHeader(t, logout.SetCookie)
	if clearing.Value != "" || clearing.MaxAge >= 0 || !clearing.HttpOnly || !clearing.Secure || clearing.Path != "/" {
		t.Fatalf("clearing cookie=%#v header=%q", clearing, logout.SetCookie)
	}
	if _, err := manager.ValidateCookie(cookie.Value); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("logged-out cookie err=%v", err)
	}
}

func TestRuntimeRequestRegistrationIsScopedAndCollisionSafe(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
	manager := newTestManager(t, root, clock, time.Minute)
	record := RuntimeRequest{RequestID: "request-1", ServiceID: "core/example/login", RuntimeGroupID: "group-1", SandboxID: "sandbox-1", Auth: AuthContext{Authenticated: true, SessionID: "private-session"}, SecureTransport: true}
	release, err := manager.BeginRuntimeRequest(record)
	if err != nil {
		t.Fatal(err)
	}
	resolved, exists := manager.RuntimeRequest("request-1")
	if !exists || resolved.ServiceID != record.ServiceID || resolved.Auth.SessionID != "private-session" || !resolved.SecureTransport {
		t.Fatalf("resolved runtime request = %#v, exists=%t", resolved, exists)
	}
	if _, err := manager.BeginRuntimeRequest(record); err == nil {
		t.Fatal("accepted duplicate active runtime request ID")
	}
	release()
	release()
	if _, exists := manager.RuntimeRequest("request-1"); exists {
		t.Fatal("runtime request remained after release")
	}
}
