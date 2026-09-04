package auth

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"the8020/kernel/database"
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
	db := newTestAuthDatabase(t, root)
	manager, err := New(Config{
		Database:        db,
		SessionDuration: time.Hour,
		CleanupInterval: cleanup,
		Cookie:          CookieConfig{Name: "the8020_auth", Secure: false, SameSite: "lax"},
		Argon2:          testArgon2Parameters(),
		Now:             clock.Time,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func addTestUser(t *testing.T, manager *Manager, username, password string) {
	t.Helper()
	passwordHash, err := manager.users.hasher.Hash(password)
	if err != nil {
		t.Fatal(err)
	}
	now := database.EncodeTime(manager.users.database, manager.now().UTC())
	if _, err := manager.users.database.ExecContext(context.Background(), `INSERT INTO `+usersTable+` ("username", "passwordHash", "enabled", "authVersion", "createdAt", "updatedAt") VALUES ($1, $2, $3, 1, $4, $4)`, username, passwordHash, true, now); err != nil {
		t.Fatal(err)
	}
}

func newTestAuthDatabase(t *testing.T, root string) *database.Manager {
	t.Helper()
	db := database.New(database.Config{
		Backend: database.BackendSQLite, Location: filepath.Join(root, "system.db"),
		MaximumOpenConnections: 8, MaximumIdleConnections: 2,
	})
	if _, err := db.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS "the8020__users__users" ("username" TEXT PRIMARY KEY, "passwordHash" TEXT NOT NULL, "enabled" INTEGER NOT NULL, "authVersion" INTEGER NOT NULL, "createdAt" TEXT NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`,
		`CREATE TABLE IF NOT EXISTS "the8020__users__sessions" ("sessionId" TEXT PRIMARY KEY, "username" TEXT NOT NULL, "secretHash" TEXT NOT NULL, "authVersion" INTEGER NOT NULL, "createdAt" TEXT NOT NULL, "expiresAt" TEXT NOT NULL) STRICT`,
	} {
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestAuthenticationConstructionRejectsMissingPackageTables(t *testing.T) {
	db := database.New(database.Config{
		Backend: database.BackendSQLite, Location: filepath.Join(t.TempDir(), "system.db"),
		MaximumOpenConnections: 2, MaximumIdleConnections: 1,
	})
	if _, err := db.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := New(Config{Database: db, Argon2: testArgon2Parameters()}); err == nil || !strings.Contains(err.Error(), "check users table") {
		t.Fatalf("construct authentication without users package tables: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE `+usersTable+` ("username" TEXT PRIMARY KEY, "passwordHash" TEXT NOT NULL, "enabled" INTEGER NOT NULL, "authVersion" INTEGER NOT NULL, "createdAt" TEXT NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Database: db, Argon2: testArgon2Parameters()}); err == nil || !strings.Contains(err.Error(), "check authentication sessions table") {
		t.Fatalf("construct authentication without sessions table: %v", err)
	}
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
	addTestUser(t, nodeA, "admin", "password")
	first, err := nodeA.Login(context.Background(), "admin", "password", true)
	if err != nil || !first.Authenticated || first.User == nil || first.User.ID != "user:admin" || first.Error != "" {
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
	stored, err := nodeA.sessions.Read(sessionID)
	if err != nil || stored.SecretHash == secret || !strings.HasPrefix(stored.SecretHash, "sha256:") {
		t.Fatalf("stored session disclosed secret or omitted hash: %#v, err=%v", stored, err)
	}

	nodeB := newTestManager(t, root, clock, time.Minute)
	contextValue, err := nodeB.ValidateCookie(cookie.Value)
	if err != nil || !contextValue.Authenticated || contextValue.Username != "admin" || contextValue.UserID != "user:admin" || contextValue.SessionID != sessionID {
		t.Fatalf("context=%#v err=%v", contextValue, err)
	}
	second, err := nodeA.Login(context.Background(), "admin", "password", false)
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
	if err := nodeA.sessions.Delete(sessionID); err != nil {
		t.Fatal(err)
	}
	if err := nodeA.sessions.Delete(sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := nodeB.ValidateCookie(cookie.Value); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked cookie err=%v", err)
	}
}

func TestAuthenticationObservesPackageOwnedUserState(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
	manager := newTestManager(t, t.TempDir(), clock, time.Minute)
	addTestUser(t, manager, "admin", "password")
	login, err := manager.Login(context.Background(), "admin", "password", false)
	if err != nil || !login.Authenticated {
		t.Fatalf("login=%#v err=%v", login, err)
	}
	cookie := cookieFromHeader(t, login.SetCookie).Value
	if _, err := manager.users.database.ExecContext(context.Background(), `UPDATE `+usersTable+` SET "authVersion" = "authVersion" + 1 WHERE "username" = $1`, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ValidateCookie(cookie); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("version-mismatched cookie err=%v", err)
	}
	if _, err := manager.users.database.ExecContext(context.Background(), `UPDATE `+usersTable+` SET "enabled" = $1 WHERE "username" = $2`, false, "admin"); err != nil {
		t.Fatal(err)
	}
	if result, err := manager.Login(context.Background(), "admin", "password", false); err != nil || result.Error != "disabled" || result.Authenticated {
		t.Fatalf("disabled login=%#v err=%v", result, err)
	}
	if _, err := manager.users.database.ExecContext(context.Background(), `DELETE FROM `+usersTable+` WHERE "username" = $1`, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.users.Authenticate("admin", "password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("removed user authentication err=%v", err)
	}
}

func TestAuthenticationSessionExpirationLazyAndPeriodicCleanup(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
	manager := newTestManager(t, root, clock, 5*time.Millisecond)
	addTestUser(t, manager, "admin", "password")
	login, err := manager.Login(context.Background(), "admin", "password", false)
	if err != nil {
		t.Fatal(err)
	}
	first := cookieFromHeader(t, login.SetCookie).Value
	firstID, _, _ := parseSessionToken(first)
	clock.Advance(2 * time.Hour)
	if _, err := manager.ValidateCookie(first); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired cookie err=%v", err)
	}
	if _, err := manager.sessions.Read(firstID); !errors.Is(err, sql.ErrNoRows) {
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
			_, err := manager.sessions.CleanupExpired(clock.Time())
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
	if _, err := manager.sessions.Read(second.SessionID); !errors.Is(err, sql.ErrNoRows) {
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
		if _, err := manager.sessions.Read(third.SessionID); errors.Is(err, sql.ErrNoRows) {
			cancel()
			<-done
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("periodic cleanup did not remove expired authentication session")
}

func TestLogoutClearsCookieAndRevokesCurrentSession(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
	manager := newTestManager(t, root, clock, time.Minute)
	addTestUser(t, manager, "admin", "password")
	login, err := manager.Login(context.Background(), "admin", "password", false)
	if err != nil {
		t.Fatal(err)
	}
	cookie := cookieFromHeader(t, login.SetCookie)
	authContext, err := manager.ValidateCookie(cookie.Value)
	if err != nil {
		t.Fatal(err)
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
