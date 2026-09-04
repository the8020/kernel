// Package auth owns users and opaque shared authentication
// sessions. Authentication data remains kernel-only and outside sandboxes.
package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"the8020/kernel/database"
)

const UserRealm = "user"

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
	Database        database.Store
	SessionDuration time.Duration
	CleanupInterval time.Duration
	Cookie          CookieConfig
	Argon2          Argon2Parameters
	Hasher          *PasswordHasher
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

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Realm    string `json:"realm"`
}

type LoginResult struct {
	Authenticated bool   `json:"authenticated"`
	User          *User  `json:"user,omitempty"`
	SetCookie     string `json:"setCookie,omitempty"`
	Error         string `json:"error,omitempty"`
}

type LogoutResult struct {
	SetCookie string `json:"setCookie"`
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
	hasher := config.Hasher
	if hasher == nil {
		var err error
		hasher, err = NewPasswordHasher(config.Argon2, config.Random)
		if err != nil {
			return nil, err
		}
	}
	users, err := NewUserStore(UserStoreConfig{Database: config.Database, Hasher: hasher})
	if err != nil {
		return nil, err
	}
	if err := users.Check(); err != nil {
		return nil, fmt.Errorf("check users table: %w", err)
	}
	sessions, err := NewSessionStore(SessionStoreConfig{Database: config.Database, Random: config.Random, Now: config.Now})
	if err != nil {
		return nil, err
	}
	if err := sessions.Check(); err != nil {
		return nil, fmt.Errorf("check authentication sessions table: %w", err)
	}
	return &Manager{users: users, sessions: sessions, sessionDuration: config.SessionDuration, cleanupInterval: config.CleanupInterval, cookie: config.Cookie, now: config.Now, runtimeRequests: make(map[string]*RuntimeRequest)}, nil
}

func (m *Manager) CookieName() string { return m.cookie.Name }

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

func (m *Manager) Login(ctx context.Context, username, password string, secureTransport bool) (LoginResult, error) {
	if err := ctx.Err(); err != nil {
		return LoginResult{Error: "internal_error"}, err
	}
	user, err := m.users.AuthenticateContext(ctx, username, password)
	if errors.Is(err, ErrInvalidCredentials) {
		return LoginResult{Error: "invalid_credentials"}, nil
	}
	if errors.Is(err, ErrUserDisabled) {
		return LoginResult{Error: "disabled"}, nil
	}
	if err != nil {
		return LoginResult{Error: "internal_error"}, err
	}
	session, token, err := m.sessions.CreateContext(ctx, user.Username, user.AuthVersion, m.sessionDuration)
	if err != nil {
		return LoginResult{Error: "internal_error"}, err
	}
	return LoginResult{
		Authenticated: true,
		User:          &User{ID: user.ID(), Username: user.Username, Realm: UserRealm},
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
	return authContextForUser(user), nil
}

// AuthenticateUser resolves a real enabled 80|20 user without verifying a
// password. Protocol adapters use it only as the identity half of an
// independently verified authentication factor.
func (m *Manager) AuthenticateUser(username string) (AuthContext, error) {
	user, exists, err := m.users.Get(username)
	if err != nil {
		return AuthContext{}, err
	}
	if !exists {
		return AuthContext{}, ErrInvalidCredentials
	}
	if !user.Enabled {
		return AuthContext{}, ErrUserDisabled
	}
	return authContextForUser(user), nil
}

func authContextForUser(user UserRecord) AuthContext {
	return AuthContext{
		Authenticated: true,
		Realm:         UserRealm,
		UserID:        user.ID(),
		Username:      user.Username,
		AuthVersion:   user.AuthVersion,
	}
}

func (m *Manager) ValidateCookie(cookieValue string) (AuthContext, error) {
	return m.ValidateCookieContext(context.Background(), cookieValue)
}

func (m *Manager) ValidateCookieContext(ctx context.Context, cookieValue string) (AuthContext, error) {
	session, err := m.sessions.ValidateTokenContext(ctx, cookieValue)
	if err != nil {
		return AuthContext{}, ErrUnauthenticated
	}
	user, exists, err := m.users.GetContext(ctx, session.Username)
	if err != nil {
		return AuthContext{}, err
	}
	if !exists || !user.Enabled || user.AuthVersion != session.AuthVersion {
		return AuthContext{}, ErrUnauthenticated
	}
	return AuthContext{Authenticated: true, Realm: UserRealm, UserID: user.ID(), Username: user.Username, AuthVersion: user.AuthVersion, SessionID: session.SessionID}, nil
}

func (m *Manager) LogoutCurrent(authContext AuthContext, secureTransport bool) (LogoutResult, error) {
	return m.LogoutCurrentContext(context.Background(), authContext, secureTransport)
}

func (m *Manager) LogoutCurrentContext(ctx context.Context, authContext AuthContext, secureTransport bool) (LogoutResult, error) {
	if authContext.SessionID != "" {
		if err := m.sessions.DeleteContext(ctx, authContext.SessionID); err != nil {
			return LogoutResult{}, err
		}
	}
	return LogoutResult{SetCookie: m.clearCookie(secureTransport)}, nil
}

func (m *Manager) RunCleanup(ctx context.Context, report func(error)) {
	timer := time.NewTimer(m.cleanupInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if _, err := m.sessions.CleanupExpiredContext(ctx, m.now().UTC()); err != nil && report != nil {
				report(err)
			}
			timer.Reset(m.cleanupInterval)
		}
	}
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
