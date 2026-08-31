// Package auth owns bootstrap administrators and opaque shared authentication
// sessions. Authentication data remains kernel-only and outside sandboxes.
package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const BootstrapRealm = "bootstrap-admin"

type CookieConfig struct {
	Name     string `json:"name"`
	Secure   bool   `json:"secure"`
	SameSite string `json:"same_site"`
}

func DefaultCookieConfig() CookieConfig {
	return CookieConfig{Name: "the8020_auth", SameSite: "lax"}
}

func (c CookieConfig) Validate() error {
	if c.Name == "" {
		return errors.New("authentication cookie name is required")
	}
	if err := (&http.Cookie{Name: c.Name, Value: "value"}).Valid(); err != nil {
		return fmt.Errorf("authentication cookie: %w", err)
	}
	switch strings.ToLower(c.SameSite) {
	case "lax", "strict", "none":
		return nil
	default:
		return errors.New("authentication cookie SameSite must be lax, strict, or none")
	}
}

type Config struct {
	UsersFile       string
	SessionsRoot    string
	SessionDuration time.Duration
	CleanupInterval time.Duration
	Cookie          CookieConfig
	Argon2          Argon2Parameters
	LockTimeout     time.Duration
	Random          io.Reader
	Now             func() time.Time
}

type Manager struct {
	users           *UserStore
	sessions        *SessionStore
	sessionDuration time.Duration
	cleanupInterval time.Duration
	cookie          CookieConfig
	now             func() time.Time
	runtimeMu       sync.Mutex
	runtimeRequests map[string]*RuntimeRequest
}

type RuntimeRequest struct {
	RequestID       string
	ServiceID       string
	RuntimeGroupID  string
	SandboxID       string
	Auth            AuthContext
	SecureTransport bool
}

type AuthContext struct {
	Authenticated bool   `json:"authenticated"`
	Realm         string `json:"realm,omitempty"`
	UserID        string `json:"userId,omitempty"`
	Username      string `json:"username,omitempty"`
	AuthVersion   uint64 `json:"authVersion,omitempty"`
	SessionID     string `json:"-"`
}

type BootstrapUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Realm    string `json:"realm"`
}

type BootstrapLoginResult struct {
	Authenticated bool           `json:"authenticated"`
	User          *BootstrapUser `json:"user,omitempty"`
	SetCookie     string         `json:"setCookie,omitempty"`
	Error         string         `json:"error,omitempty"`
}

type LogoutResult struct {
	SetCookie string `json:"setCookie"`
}

type UserSummary struct {
	Username       string    `json:"username"`
	Enabled        bool      `json:"enabled"`
	AuthVersion    uint64    `json:"auth_version"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	ActiveSessions int       `json:"active_authentication_session_count"`
}

type SessionSummary struct {
	SessionID   string    `json:"session_id"`
	Username    string    `json:"username"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Valid       bool      `json:"valid"`
	AuthVersion uint64    `json:"auth_version"`
}

func New(config Config) (*Manager, error) {
	if config.SessionDuration <= 0 {
		config.SessionDuration = 12 * time.Hour
	}
	if config.CleanupInterval <= 0 {
		config.CleanupInterval = 15 * time.Minute
	}
	if config.Cookie == (CookieConfig{}) {
		config.Cookie = DefaultCookieConfig()
	}
	config.Cookie.SameSite = strings.ToLower(config.Cookie.SameSite)
	if err := config.Cookie.Validate(); err != nil {
		return nil, err
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	hasher, err := NewPasswordHasher(config.Argon2, config.Random)
	if err != nil {
		return nil, err
	}
	users, err := NewUserStore(UserStoreConfig{Path: config.UsersFile, Hasher: hasher, LockTimeout: config.LockTimeout, Now: config.Now})
	if err != nil {
		return nil, err
	}
	sessions, err := NewSessionStore(SessionStoreConfig{Root: config.SessionsRoot, Random: config.Random, Now: config.Now})
	if err != nil {
		return nil, err
	}
	return &Manager{users: users, sessions: sessions, sessionDuration: config.SessionDuration, cleanupInterval: config.CleanupInterval, cookie: config.Cookie, now: config.Now, runtimeRequests: make(map[string]*RuntimeRequest)}, nil
}

func (m *Manager) UsersFile() string    { return m.users.Path() }
func (m *Manager) SessionsRoot() string { return m.sessions.Root() }
func (m *Manager) CookieName() string   { return m.cookie.Name }

func (m *Manager) BeginRuntimeRequest(request RuntimeRequest) (func(), error) {
	if request.RequestID == "" || request.ServiceID == "" || request.RuntimeGroupID == "" || request.SandboxID == "" {
		return nil, errors.New("runtime authentication request identity is incomplete")
	}
	registered := request
	m.runtimeMu.Lock()
	if _, exists := m.runtimeRequests[request.RequestID]; exists {
		m.runtimeMu.Unlock()
		return nil, errors.New("runtime authentication request ID is already active")
	}
	m.runtimeRequests[request.RequestID] = &registered
	m.runtimeMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.runtimeMu.Lock()
			if current := m.runtimeRequests[request.RequestID]; current == &registered {
				delete(m.runtimeRequests, request.RequestID)
			}
			m.runtimeMu.Unlock()
		})
	}, nil
}

func (m *Manager) RuntimeRequest(requestID string) (RuntimeRequest, bool) {
	m.runtimeMu.Lock()
	defer m.runtimeMu.Unlock()
	request, exists := m.runtimeRequests[requestID]
	if !exists {
		return RuntimeRequest{}, false
	}
	return *request, true
}

func (m *Manager) BootstrapLogin(ctx context.Context, username, password string, secureTransport bool) (BootstrapLoginResult, error) {
	if err := ctx.Err(); err != nil {
		return BootstrapLoginResult{Error: "internal_error"}, err
	}
	user, err := m.users.Authenticate(username, password)
	if errors.Is(err, ErrInvalidCredentials) {
		return BootstrapLoginResult{Error: "invalid_credentials"}, nil
	}
	if errors.Is(err, ErrUserDisabled) {
		return BootstrapLoginResult{Error: "disabled"}, nil
	}
	if err != nil {
		return BootstrapLoginResult{Error: "internal_error"}, err
	}
	session, token, err := m.sessions.Create(user.Username, user.AuthVersion, m.sessionDuration)
	if err != nil {
		return BootstrapLoginResult{Error: "internal_error"}, err
	}
	return BootstrapLoginResult{
		Authenticated: true,
		User:          &BootstrapUser{ID: user.ID(), Username: user.Username, Realm: BootstrapRealm},
		SetCookie:     m.setCookie(token, session.ExpiresAt, secureTransport),
	}, nil
}

// AuthenticatePassword validates a real 80|20 user without creating a browser
// authentication session. The presented password is neither retained nor
// persisted; callers that own a mutable input buffer remain responsible for
// clearing it after this call returns.
func (m *Manager) AuthenticatePassword(username string, password []byte) (AuthContext, error) {
	user, err := m.users.AuthenticateBytes(username, password)
	if err != nil {
		return AuthContext{}, err
	}
	return AuthContext{
		Authenticated: true,
		Realm:         BootstrapRealm,
		UserID:        user.ID(),
		Username:      user.Username,
		AuthVersion:   user.AuthVersion,
	}, nil
}

func (m *Manager) ValidateCookie(cookieValue string) (AuthContext, error) {
	session, err := m.sessions.ValidateToken(cookieValue)
	if err != nil {
		return AuthContext{}, ErrUnauthenticated
	}
	user, exists, err := m.users.Get(session.Username)
	if err != nil {
		return AuthContext{}, err
	}
	if !exists || !user.Enabled || user.AuthVersion != session.AuthVersion {
		return AuthContext{}, ErrUnauthenticated
	}
	return AuthContext{Authenticated: true, Realm: BootstrapRealm, UserID: user.ID(), Username: user.Username, AuthVersion: user.AuthVersion, SessionID: session.SessionID}, nil
}

func (m *Manager) LogoutCurrent(context AuthContext, secureTransport bool) (LogoutResult, error) {
	if context.SessionID != "" {
		if err := m.sessions.Delete(context.SessionID); err != nil {
			return LogoutResult{}, err
		}
	}
	return LogoutResult{SetCookie: m.clearCookie(secureTransport)}, nil
}

func (m *Manager) AddUser(ctx context.Context, username, password string) (UserSummary, error) {
	user, err := m.users.Add(ctx, username, password)
	return summaryForUser(user, 0), err
}

func (m *Manager) RemoveUser(ctx context.Context, username string) error {
	if err := m.users.Remove(ctx, username); err != nil {
		return err
	}
	_, err := m.sessions.RevokeUser(username)
	return err
}

func (m *Manager) EnableUser(ctx context.Context, username string) (UserSummary, error) {
	user, err := m.users.Enable(ctx, username)
	return summaryForUser(user, 0), err
}

func (m *Manager) DisableUser(ctx context.Context, username string) (UserSummary, error) {
	user, err := m.users.Disable(ctx, username)
	if err != nil {
		return UserSummary{}, err
	}
	_, revokeErr := m.sessions.RevokeUser(username)
	return summaryForUser(user, 0), revokeErr
}

func (m *Manager) SetPassword(ctx context.Context, username, password string) (UserSummary, error) {
	user, err := m.users.SetPassword(ctx, username, password)
	if err != nil {
		return UserSummary{}, err
	}
	_, revokeErr := m.sessions.RevokeUser(username)
	return summaryForUser(user, 0), revokeErr
}

func (m *Manager) InvalidateUserSessions(ctx context.Context, username string) (UserSummary, error) {
	user, err := m.users.InvalidateSessions(ctx, username)
	if err != nil {
		return UserSummary{}, err
	}
	_, revokeErr := m.sessions.RevokeUser(username)
	return summaryForUser(user, 0), revokeErr
}

func (m *Manager) ListUsers() ([]UserSummary, error) {
	users, err := m.users.List()
	if err != nil {
		return nil, err
	}
	sessions, err := m.ListSessions()
	if err != nil {
		return nil, err
	}
	active := make(map[string]int)
	for _, session := range sessions {
		if session.Valid {
			active[session.Username]++
		}
	}
	result := make([]UserSummary, 0, len(users))
	for _, user := range users {
		result = append(result, summaryForUser(user, active[user.Username]))
	}
	return result, nil
}

func (m *Manager) ListSessions() ([]SessionSummary, error) {
	records, err := m.sessions.List()
	if err != nil {
		return nil, err
	}
	users, err := m.users.List()
	if err != nil {
		return nil, err
	}
	byUsername := make(map[string]UserRecord, len(users))
	for _, user := range users {
		byUsername[user.Username] = user
	}
	now := m.now().UTC()
	result := make([]SessionSummary, 0, len(records))
	for _, record := range records {
		user, exists := byUsername[record.Username]
		valid := now.Before(record.ExpiresAt) && exists && user.Enabled && user.AuthVersion == record.AuthVersion
		result = append(result, SessionSummary{SessionID: record.SessionID, Username: record.Username, CreatedAt: record.CreatedAt, ExpiresAt: record.ExpiresAt, Valid: valid, AuthVersion: record.AuthVersion})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SessionID < result[j].SessionID })
	return result, nil
}

func (m *Manager) RevokeSession(sessionID string) error { return m.sessions.Delete(sessionID) }

func (m *Manager) RevokeUserSessions(username string) (int, error) {
	return m.sessions.RevokeUser(username)
}

func (m *Manager) CleanupExpired() (int, error) {
	return m.sessions.CleanupExpired(m.now().UTC())
}

func (m *Manager) RunCleanup(ctx context.Context, report func(error)) {
	timer := time.NewTimer(m.cleanupInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if _, err := m.CleanupExpired(); err != nil && report != nil {
				report(err)
			}
			timer.Reset(m.cleanupInterval)
		}
	}
}

func summaryForUser(user UserRecord, active int) UserSummary {
	return UserSummary{Username: user.Username, Enabled: user.Enabled, AuthVersion: user.AuthVersion, CreatedAt: user.CreatedAt, UpdatedAt: user.UpdatedAt, ActiveSessions: active}
}

func (m *Manager) setCookie(value string, expires time.Time, secureTransport bool) string {
	maxAge := int(expires.Sub(m.now().UTC()).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	return (&http.Cookie{Name: m.cookie.Name, Value: value, Path: "/", HttpOnly: true, Secure: m.cookie.Secure || secureTransport, SameSite: sameSiteMode(m.cookie.SameSite), Expires: expires, MaxAge: maxAge}).String()
}

func (m *Manager) clearCookie(secureTransport bool) string {
	return (&http.Cookie{Name: m.cookie.Name, Value: "", Path: "/", HttpOnly: true, Secure: m.cookie.Secure || secureTransport, SameSite: sameSiteMode(m.cookie.SameSite), Expires: time.Unix(1, 0).UTC(), MaxAge: -1}).String()
}

func sameSiteMode(value string) http.SameSite {
	switch strings.ToLower(value) {
	case "strict":
		return http.SameSiteStrictMode
	case "none":
		return http.SameSiteNoneMode
	default:
		return http.SameSiteLaxMode
	}
}
