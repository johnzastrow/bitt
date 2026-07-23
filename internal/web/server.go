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

	"github.com/a-h/templ"
	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/config"
	"github.com/johnzastrow/bitt/internal/ledger"
	"github.com/johnzastrow/bitt/internal/store"
	"github.com/johnzastrow/bitt/internal/web/views"
)

// Server holds the application's HTTP dependencies.
type Server struct {
	cfg      config.Config
	store    store.Store
	ledger   *ledger.Service
	sessions *auth.Manager
	log      *slog.Logger
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
		_, _ = w.Write([]byte("ok"))
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
		Title:     title,
		CSRFToken: auth.EnsureCSRFToken(w, r, s.cfg.SecureCookies),
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
