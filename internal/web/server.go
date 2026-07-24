// Package web wires HTTP routes to the application services.
package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/config"
	"github.com/johnzastrow/bitt/internal/ledger"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
	"github.com/johnzastrow/bitt/internal/version"
	"github.com/johnzastrow/bitt/internal/web/views"
)

// Server holds the application's HTTP dependencies.
type Server struct {
	cfg      config.Config
	store    store.Store
	ledger   *ledger.Service
	sessions *auth.Manager
	log      *slog.Logger
	// zones caches resolved timezones. time.LoadLocation parses the zoneinfo
	// database on every call, and the instance timezone is read on every tab
	// render (SCHED-02).
	zones sync.Map
	// avatarRate limits the one route that does real CPU work per request.
	// Decoding an image is far more expensive than anything else here and an
	// authenticated user can ask for it in a loop.
	avatarRate   sync.Map
	avatarRateMu sync.Mutex
}

// New builds a server.
func New(cfg config.Config, st store.Store, led *ledger.Service, sessions *auth.Manager, log *slog.Logger) *Server {
	return &Server{cfg: cfg, store: st, ledger: led, sessions: sessions, log: log}
}

// contextKey is unexported so no other package can collide with it.
type contextKey struct{ name string }

var userKey = &contextKey{"user"}

// userFrom returns the authenticated user attached by requireAuth.
func userFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(userKey).(*store.User)
	return u
}

// Handler returns the fully wired router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler()))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok " + version.Short()))
	})

	// First-run setup (AUTH-03)
	mux.HandleFunc("GET /setup", s.getSetup)
	mux.HandleFunc("POST /setup", s.postSetup)

	// Authentication (AUTH-01, AUTH-02)
	mux.HandleFunc("GET /login", s.getLogin)
	mux.HandleFunc("POST /login", s.postLogin)
	mux.HandleFunc("POST /logout", s.postLogout)

	// Application
	mux.Handle("GET /{$}", s.requireAuth(http.HandlerFunc(s.getDashboard)))
	mux.Handle("GET /tabs/new", s.requireAuth(http.HandlerFunc(s.getNewTab)))
	mux.Handle("POST /tabs", s.requireAuth(http.HandlerFunc(s.postTab)))
	mux.Handle("GET /tabs/{id}", s.requireAuth(http.HandlerFunc(s.getTab)))
	mux.Handle("POST /tabs/{id}/charges", s.requireAuth(http.HandlerFunc(s.postCharge)))

	// The settle loop (PAY-01, UI-03)
	mux.Handle("GET /tabs/{id}/card", s.requireAuth(http.HandlerFunc(s.getCard)))
	mux.Handle("GET /tabs/{id}/settle", s.requireAuth(http.HandlerFunc(s.getSettleConfirm)))
	mux.Handle("POST /tabs/{id}/settle", s.requireAuth(http.HandlerFunc(s.postSettle)))
	mux.Handle("POST /tabs/{id}/payments", s.requireAuth(http.HandlerFunc(s.postPayment)))
	mux.Handle("POST /tabs/{id}/adjustments", s.requireAuth(http.HandlerFunc(s.postAdjustment)))
	mux.Handle("POST /tabs/{id}/entries/{seq}/undo", s.requireAuth(http.HandlerFunc(s.postUndo)))
	mux.Handle("POST /tabs/{id}/participants", s.requireAuth(http.HandlerFunc(s.postParticipant)))
	mux.Handle("POST /tabs/{id}/participants/{userID}/remove", s.requireAuth(http.HandlerFunc(s.postParticipantRemove)))

	// Tab settings: name, description, kind, and archival (TAB-01, TAB-02)
	mux.Handle("POST /tabs/{id}/details", s.requireAuth(http.HandlerFunc(s.postTabDetails)))
	mux.Handle("POST /tabs/{id}/archive", s.requireAuth(http.HandlerFunc(s.postTabArchive)))

	// Recurrence, late fees, and what a period covers (SCHED-01, FEE-01, CHG-02)
	mux.Handle("POST /tabs/{id}/schedule", s.requireAuth(http.HandlerFunc(s.postSchedule)))
	mux.Handle("POST /tabs/{id}/fee", s.requireAuth(http.HandlerFunc(s.postFeePolicy)))
	mux.Handle("POST /tabs/{id}/interest", s.requireAuth(http.HandlerFunc(s.postInterestRate)))
	mux.Handle("POST /tabs/{id}/loan", s.requireAuth(http.HandlerFunc(s.postLoanTerms)))
	mux.Handle("POST /tabs/{id}/items", s.requireAuth(http.HandlerFunc(s.postItem)))
	mux.Handle("POST /tabs/{id}/items/{itemID}", s.requireAuth(http.HandlerFunc(s.postItemUpdate)))
	mux.Handle("POST /tabs/{id}/items/{itemID}/remove", s.requireAuth(http.HandlerFunc(s.postItemRemove)))

	// Account self-service
	mux.Handle("GET /profile", s.requireAuth(http.HandlerFunc(s.getProfile)))
	mux.Handle("POST /profile/details", s.requireAuth(http.HandlerFunc(s.postProfileDetails)))
	mux.Handle("POST /profile/password", s.requireAuth(http.HandlerFunc(s.postProfilePassword)))
	mux.Handle("POST /profile/avatar", s.requireAuth(http.HandlerFunc(s.postProfileAvatar)))
	mux.Handle("POST /profile/avatar/remove", s.requireAuth(http.HandlerFunc(s.postProfileAvatarRemove)))
	mux.Handle("GET /users/{id}/avatar", s.requireAuth(http.HandlerFunc(s.getAvatar)))

	// Administration (AUTH-04)
	mux.Handle("GET /admin/users", s.requireAdmin(http.HandlerFunc(s.getAdminUsers)))
	mux.Handle("POST /admin/users", s.requireAdmin(http.HandlerFunc(s.postAdminUser)))
	mux.Handle("POST /admin/users/{id}/active", s.requireAdmin(http.HandlerFunc(s.postAdminUserActive)))

	return s.securityHeaders(s.recoverPanic(mux))
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// securityHeaders applies defense-in-depth headers to every response.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// No inline script or eval. htmx is served from our own origin, and all
		// styling lives in a stylesheet, so nothing needs 'unsafe-inline'.
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
				"form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), payment=()")
		if s.cfg.SecureCookies {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// recoverPanic keeps a handler panic from taking down the process, and shows
// the user a generic message while the detail goes to the log.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in handler", "path", r.URL.Path, "recovered", rec)
				s.serverError(w, r, errors.New("panic recovered"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requireAuth admits only signed-in users, and sends everyone else to setup or
// login. It fails closed: any error resolving the session is a redirect, never
// a fall-through.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := s.sessions.Resolve(r)
		if err != nil {
			if s.setupPending(r.Context()) {
				http.Redirect(w, r, "/setup", http.StatusSeeOther)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), userKey, &user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// location resolves the instance timezone, which is the zone period boundaries
// are computed in (SCHED-02).
//
// It falls back to UTC and logs rather than failing the render. A tab that
// displays with a boundary an hour off is recoverable; a 500 on every page
// because someone typoed a zone name is not.
func (s *Server) location(ctx context.Context) *time.Location {
	inst, err := s.store.GetInstance(ctx)
	if err != nil {
		s.log.Warn("could not read the instance timezone", "error", err)
		return time.UTC
	}
	if inst.Timezone == "" {
		return time.UTC
	}
	if cached, ok := s.zones.Load(inst.Timezone); ok {
		return cached.(*time.Location)
	}
	loc, err := time.LoadLocation(inst.Timezone)
	if err != nil {
		s.log.Warn("instance timezone is not a known zone, using UTC",
			"timezone", inst.Timezone, "error", err)
		return time.UTC
	}
	s.zones.Store(inst.Timezone, loc)
	return loc
}

// today is the current calendar date in the instance timezone, which is what
// decides whether a period has come due or run late.
func (s *Server) today(ctx context.Context) schedule.Date {
	return schedule.DateOf(time.Now().In(s.location(ctx)))
}

// accrueTab posts every billing cycle the tab owes, as part of reading it
// (SCHED-03). This is the whole scheduler: there is no background process, so
// if this is not called on a read path, that tab simply never bills.
func (s *Server) accrueTab(r *http.Request, tab store.Tab) (ledger.Accrual, error) {
	if !tab.Schedule.Set() {
		return ledger.Accrual{}, nil
	}
	// An archived tab stops billing. Without this, archiving would only hide a
	// tab from the dashboard while it quietly kept charging.
	if tab.ArchivedAt != nil {
		return ledger.Accrual{}, nil
	}
	// The full history, superseded rows included: each cycle is billed for the
	// items the tab carried when that cycle came due (CHG-02).
	history, err := s.store.ListItemHistory(r.Context(), tab.ID)
	if err != nil {
		return ledger.Accrual{}, err
	}
	acc, err := s.ledger.Accrue(r.Context(), tab, history, s.location(r.Context()))
	if err != nil {
		return ledger.Accrual{}, err
	}
	if len(acc.Posted) > 0 {
		s.log.Info("scheduled charges posted",
			"tab_id", tab.ID, "periods", len(acc.Posted))
	}
	return acc, nil
}

// setupPending reports whether the first-run screen is still available.
func (s *Server) setupPending(ctx context.Context) bool {
	inst, err := s.store.GetInstance(ctx)
	if err != nil {
		return false
	}
	return !inst.SetupComplete()
}

// ---------------------------------------------------------------------------
// Rendering and errors
// ---------------------------------------------------------------------------

// page builds the chrome data for a render, minting a CSRF token as it goes.
func (s *Server) page(w http.ResponseWriter, r *http.Request, title string) views.Page {
	p := views.Page{
		Title:        title,
		CSRFToken:    auth.EnsureCSRFToken(w, r, s.cfg.SecureCookies),
		AssetVersion: AssetVersion(),
	}
	if u := userFrom(r.Context()); u != nil {
		p.User = u
	}
	if msg := r.URL.Query().Get("err"); msg != "" {
		p.Error = safeFlash(msg)
	}
	if msg := r.URL.Query().Get("ok"); msg != "" {
		p.Flash = safeFlash(msg)
	}
	return p
}

// safeFlash bounds a querystring-supplied message. The value is rendered as
// text by templ, which escapes it, so this only guards against absurd length.
func safeFlash(s string) string {
	const max = 200
	if len(s) > max {
		return s[:max]
	}
	return s
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := c.Render(r.Context(), w); err != nil {
		s.log.Error("render failed", "path", r.URL.Path, "error", err)
	}
}

// serverError logs the detail and shows the user a generic message. No stack
// trace, database error, or file path ever reaches the response.
func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	http.Error(w, "Something went wrong. Please try again.", http.StatusInternalServerError)
}

// redirectWith sends the user onward carrying a short message.
func redirectWith(w http.ResponseWriter, r *http.Request, path, param, msg string) {
	if msg != "" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + param + "=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// pathID reads a positive integer path parameter.
func pathID(r *http.Request, name string) (int64, bool) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
