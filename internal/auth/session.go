package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/johnzastrow/bitt/internal/store"
)

// SessionCookieName is the cookie carrying the session token.
const SessionCookieName = "bitt_session"

// SessionTTL is how long a session remains valid without re-login.
const SessionTTL = 30 * 24 * time.Hour

// touchInterval throttles last-seen writes so a page load does not cause a
// database write on every request.
const touchInterval = 15 * time.Minute

// ErrNoSession is returned when a request carries no usable session.
var ErrNoSession = errors.New("auth: no session")

// Manager issues and validates sessions (AUTH-02).
type Manager struct {
	store store.SessionStore
	// Secure sets the cookie's Secure flag. True in production; may be false
	// for local plain-HTTP development only.
	Secure bool
}

// NewManager builds a session manager.
func NewManager(s store.SessionStore, secure bool) *Manager {
	return &Manager{store: s, Secure: secure}
}

// hashToken derives the storage key for a token. Only the digest is persisted,
// so a database read cannot be replayed as a login.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Issue creates a session and writes the cookie.
func (m *Manager) Issue(ctx context.Context, w http.ResponseWriter, userID int64) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("auth: generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	now := time.Now().UTC()
	if err := m.store.CreateSession(ctx, store.Session{
		TokenHash:  hashToken(token),
		UserID:     userID,
		CreatedAt:  now,
		ExpiresAt:  now.Add(SessionTTL),
		LastSeenAt: now,
	}); err != nil {
		return err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   m.Secure,
		// Lax rather than Strict: Strict would drop the session when following
		// a link into the app from elsewhere, and every state-changing route is
		// a POST carrying a CSRF token.
		SameSite: http.SameSiteLaxMode,
		Expires:  now.Add(SessionTTL),
		MaxAge:   int(SessionTTL / time.Second),
	})
	return nil
}

// Resolve returns the user for the request's session, or ErrNoSession.
//
// It fails closed: any error resolving the session denies access rather than
// falling through to an anonymous-but-permitted state.
func (m *Manager) Resolve(r *http.Request) (store.User, error) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return store.User{}, ErrNoSession
	}

	tokenHash := hashToken(cookie.Value)
	session, user, err := m.store.GetSession(r.Context(), tokenHash)
	if err != nil {
		return store.User{}, ErrNoSession
	}

	if time.Since(session.LastSeenAt) > touchInterval {
		_ = m.store.TouchSession(r.Context(), tokenHash, time.Now().UTC())
	}
	return user, nil
}

// Revoke deletes the session and clears the cookie (AUTH-03 logout).
func (m *Manager) Revoke(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	if cookie, err := r.Cookie(SessionCookieName); err == nil && cookie.Value != "" {
		if err := m.store.DeleteSession(ctx, hashToken(cookie.Value)); err != nil {
			return err
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   m.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	return nil
}
