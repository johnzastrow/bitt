package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/store"
)

// fakeSessions is an in-memory SessionStore, so the manager can be tested
// without a database. The real sqlite behaviour is exercised through the web
// package; here the concern is the manager's own logic -- cookie handling,
// fail-closed resolution, and which sessions Revoke and RevokeOthers end.
type fakeSessions struct {
	rows map[string]store.Session
	user store.User
}

func newFake(u store.User) *fakeSessions {
	return &fakeSessions{rows: map[string]store.Session{}, user: u}
}

func (f *fakeSessions) CreateSession(_ context.Context, s store.Session) error {
	f.rows[s.TokenHash] = s
	return nil
}

func (f *fakeSessions) GetSession(_ context.Context, hash string) (store.Session, store.User, error) {
	s, ok := f.rows[hash]
	if !ok || s.ExpiresAt.Before(time.Now()) {
		return store.Session{}, store.User{}, store.ErrNotFound
	}
	return s, f.user, nil
}

func (f *fakeSessions) TouchSession(_ context.Context, hash string, at time.Time) error {
	if s, ok := f.rows[hash]; ok {
		s.LastSeenAt = at
		f.rows[hash] = s
	}
	return nil
}

func (f *fakeSessions) DeleteSession(_ context.Context, hash string) error {
	delete(f.rows, hash)
	return nil
}

func (f *fakeSessions) DeleteExpiredSessions(_ context.Context, now time.Time) (int64, error) {
	var n int64
	for h, s := range f.rows {
		if s.ExpiresAt.Before(now) {
			delete(f.rows, h)
			n++
		}
	}
	return n, nil
}

func (f *fakeSessions) DeleteSessionsForUserExcept(_ context.Context, userID int64, keep string) (int, error) {
	var n int
	for h, s := range f.rows {
		if s.UserID == userID && h != keep {
			delete(f.rows, h)
			n++
		}
	}
	return n, nil
}

// issueAndCookie issues a session and returns the cookie it set, so a follow-up
// request can present it.
func issueAndCookie(t *testing.T, m *Manager, userID int64) *http.Cookie {
	t.Helper()
	w := httptest.NewRecorder()
	if err := m.Issue(context.Background(), w, userID); err != nil {
		t.Fatalf("issue: %v", err)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName {
			return c
		}
	}
	t.Fatal("Issue set no session cookie")
	return nil
}

func TestSessionRoundTrip(t *testing.T) {
	f := newFake(store.User{ID: 7, DisplayName: "Sam"})
	m := NewManager(f, false)

	cookie := issueAndCookie(t, m, 7)
	if cookie.HttpOnly != true {
		t.Error("session cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Error("session cookie is not SameSite=Lax")
	}

	// The raw token is never what is stored: the store holds only its digest.
	if _, ok := f.rows[cookie.Value]; ok {
		t.Error("the raw token is stored -- a database read could be replayed as a login")
	}
	if _, ok := f.rows[hashToken(cookie.Value)]; !ok {
		t.Error("the token digest was not stored")
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(cookie)
	user, err := m.Resolve(r)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if user.ID != 7 {
		t.Errorf("resolved user %d, want 7", user.ID)
	}
}

// Resolution fails closed: a missing, malformed, or unknown cookie denies rather
// than falling through to an anonymous-but-permitted state.
func TestResolveFailsClosed(t *testing.T) {
	m := NewManager(newFake(store.User{ID: 1}), false)

	for _, tc := range []struct {
		name   string
		cookie *http.Cookie
	}{
		{"no cookie", nil},
		{"empty value", &http.Cookie{Name: SessionCookieName, Value: ""}},
		{"unknown token", &http.Cookie{Name: SessionCookieName, Value: "not-a-real-token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.cookie != nil {
				r.AddCookie(tc.cookie)
			}
			if _, err := m.Resolve(r); err == nil {
				t.Error("resolution succeeded where it should have denied")
			}
		})
	}
}

// An expired session does not resolve, even though its row still exists.
func TestExpiredSessionDenied(t *testing.T) {
	f := newFake(store.User{ID: 3})
	m := NewManager(f, false)
	cookie := issueAndCookie(t, m, 3)

	// Force the stored row to have expired.
	s := f.rows[hashToken(cookie.Value)]
	s.ExpiresAt = time.Now().Add(-time.Hour)
	f.rows[hashToken(cookie.Value)] = s

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(cookie)
	if _, err := m.Resolve(r); err == nil {
		t.Error("an expired session resolved")
	}
}

func TestRevokeEndsOnlyThisSession(t *testing.T) {
	f := newFake(store.User{ID: 5})
	m := NewManager(f, false)

	a := issueAndCookie(t, m, 5)
	b := issueAndCookie(t, m, 5)
	if len(f.rows) != 2 {
		t.Fatalf("expected 2 sessions, have %d", len(f.rows))
	}

	// Revoke the request carrying cookie a.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/logout", nil)
	r.AddCookie(a)
	if err := m.Revoke(context.Background(), w, r); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok := f.rows[hashToken(a.Value)]; ok {
		t.Error("the revoked session survived")
	}
	if _, ok := f.rows[hashToken(b.Value)]; !ok {
		t.Error("Revoke ended a session other than the request's own")
	}
	// The cookie is cleared on the way out.
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("Revoke did not clear the session cookie")
	}
}

// RevokeOthers is what a password change runs: end every other session, keep
// the one making the request. This is the security-relevant half -- a device no
// longer trusted must lose access, and the actor must not log themselves out.
func TestRevokeOthersKeepsTheCurrentSession(t *testing.T) {
	f := newFake(store.User{ID: 9})
	m := NewManager(f, false)

	current := issueAndCookie(t, m, 9)
	issueAndCookie(t, m, 9)
	issueAndCookie(t, m, 9)
	// A different user's session must be untouched.
	other := newFake(store.User{ID: 9})
	_ = other
	f.rows["someone-else"] = store.Session{TokenHash: "someone-else", UserID: 99, ExpiresAt: time.Now().Add(time.Hour)}

	r := httptest.NewRequest(http.MethodPost, "/profile/password", nil)
	r.AddCookie(current)
	ended, err := m.RevokeOthers(context.Background(), r, 9)
	if err != nil {
		t.Fatalf("revoke others: %v", err)
	}
	if ended != 2 {
		t.Errorf("ended %d sessions, want 2 (the two other devices)", ended)
	}
	if _, ok := f.rows[hashToken(current.Value)]; !ok {
		t.Error("RevokeOthers signed out the session that made the change")
	}
	if _, ok := f.rows["someone-else"]; !ok {
		t.Error("RevokeOthers ended another user's session")
	}
}

// With no cookie to preserve, RevokeOthers ends everything for the user: the
// caller has authenticated the change some other way.
func TestRevokeOthersWithoutACookieEndsAll(t *testing.T) {
	f := newFake(store.User{ID: 4})
	m := NewManager(f, false)
	issueAndCookie(t, m, 4)
	issueAndCookie(t, m, 4)

	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	ended, err := m.RevokeOthers(context.Background(), r, 4)
	if err != nil {
		t.Fatalf("revoke others: %v", err)
	}
	if ended != 2 {
		t.Errorf("ended %d, want 2", ended)
	}
}

// ---------------------------------------------------------------------------
// CSRF (double-submit)
// ---------------------------------------------------------------------------

func TestCSRFRoundTrip(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	token := EnsureCSRFToken(w, r, false)
	if len(token) < 20 {
		t.Fatalf("token too short: %q", token)
	}

	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == CSRFCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no CSRF cookie set")
	}
	if !cookie.HttpOnly {
		t.Error("CSRF cookie is not HttpOnly")
	}

	// A POST carrying the matching token and cookie passes.
	post := httptest.NewRequest(http.MethodPost, "/x",
		strings.NewReader(url.Values{CSRFFieldName: {token}}.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.AddCookie(cookie)
	if !CheckCSRF(post) {
		t.Error("a matching token was rejected")
	}
}

func TestCSRFReusesAnExistingToken(t *testing.T) {
	existing := &http.Cookie{Name: CSRFCookieName, Value: "an-existing-token-value-1234"}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(existing)
	if got := EnsureCSRFToken(w, r, false); got != existing.Value {
		t.Errorf("minted a new token %q instead of reusing %q", got, existing.Value)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("set a new cookie when one already existed")
	}
}

func TestCSRFRejects(t *testing.T) {
	token := "the-cookie-token-value-123456"
	cookie := &http.Cookie{Name: CSRFCookieName, Value: token}

	t.Run("no cookie", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/x",
			strings.NewReader(url.Values{CSRFFieldName: {token}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if CheckCSRF(r) {
			t.Error("passed with no cookie")
		}
	})
	t.Run("mismatched token", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/x",
			strings.NewReader(url.Values{CSRFFieldName: {"wrong"}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(cookie)
		if CheckCSRF(r) {
			t.Error("passed with a mismatched token")
		}
	})
	t.Run("no submitted value", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		r.AddCookie(cookie)
		if CheckCSRF(r) {
			t.Error("passed with no submitted token")
		}
	})
	t.Run("header instead of field", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		r.AddCookie(cookie)
		r.Header.Set("X-CSRF-Token", token)
		if !CheckCSRF(r) {
			t.Error("the X-CSRF-Token header path was not honoured")
		}
	})
}

// DummyVerify must do comparable work to a real verify and never panic; login
// calls it for a missing account so timing does not reveal which emails exist.
func TestDummyVerifyDoesNotPanic(t *testing.T) {
	DummyVerify("any-password")
	DummyVerify("")
}
