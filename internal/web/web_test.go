package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/config"
	"github.com/johnzastrow/bitt/internal/ledger"
	"github.com/johnzastrow/bitt/internal/notify"
	"github.com/johnzastrow/bitt/internal/store"
	"github.com/johnzastrow/bitt/internal/store/sqlite"
	"github.com/johnzastrow/bitt/internal/tz"
	"github.com/johnzastrow/bitt/internal/version"
)

// harness is a running server plus a cookie-carrying client, so tests exercise
// the same path a browser takes.
type harness struct {
	server0 *Server
	t       *testing.T
	server  *httptest.Server
	client  *http.Client
	db      *sqlite.DB
}

func newHarness(t *testing.T) *harness { return newHarnessCfg(t, config.NotifyConfig{}) }

// newHarnessWithTick builds a harness whose tick endpoint has a secret.
func newHarnessWithTick(t *testing.T, secret string) *harness {
	return newHarnessCfg(t, config.NotifyConfig{TickSecret: secret})
}

func newHarnessCfg(t *testing.T, nc config.NotifyConfig) *harness {
	t.Helper()

	db, err := sqlite.Open(sqlite.Options{
		Path:               filepath.Join(t.TempDir(), "web.db"),
		AppendOnlyTriggers: true,
	})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := config.Config{
		Addr:            ":0",
		DefaultTimezone: "UTC",
		// The test server speaks plain HTTP, so Secure cookies would never be
		// sent back. Production defaults to true.
		SecureCookies:      false,
		AppendOnlyTriggers: true,
		Notify:             nc,
	}

	srv := New(cfg, db, ledger.New(db),
		auth.NewManager(db, cfg.SecureCookies),
		notify.New(cfg.Notify),
		slog.New(slog.DiscardHandler))

	ts := httptest.NewServer(srv.Handler())
	_ = srv
	t.Cleanup(ts.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}

	return &harness{t: t, server0: srv, server: ts, db: db, client: &http.Client{Jar: jar}}
}

// newClient returns a second harness against the same server with its own
// cookie jar, which is how a test represents another signed-in device.
func (h *harness) newClient() *harness {
	h.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		h.t.Fatalf("cookie jar: %v", err)
	}
	return &harness{t: h.t, server: h.server, db: h.db, client: &http.Client{Jar: jar}}
}

func (h *harness) get(path string) (*http.Response, string) {
	h.t.Helper()
	resp, err := h.client.Get(h.server.URL + path)
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// csrfToken scrapes the token the server just set into the form.
func (h *harness) csrfToken(path string) string {
	h.t.Helper()
	_, body := h.get(path)
	m := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(body)
	if len(m) < 2 {
		h.t.Fatalf("no CSRF token found on %s", path)
	}
	return m[1]
}

func (h *harness) post(path string, form url.Values) (*http.Response, string) {
	h.t.Helper()
	resp, err := h.client.PostForm(h.server.URL+path, form)
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// postNoRedirect posts without following the redirect, so the response's own
// Set-Cookie header can be inspected. Go's cookiejar discards cookie
// attributes, so it cannot be used to verify flags like HttpOnly.
func (h *harness) postNoRedirect(path string, form url.Values) *http.Response {
	h.t.Helper()
	client := &http.Client{
		Jar: h.client.Jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.PostForm(h.server.URL+path, form)
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// completeSetup runs the first-run screen and leaves the client signed in.
func (h *harness) completeSetup() {
	h.t.Helper()
	form := url.Values{
		"csrf_token":   {h.csrfToken("/setup")},
		"display_name": {"Jane Provider"},
		"email":        {"jane@example.com"},
		"password":     {"correct-horse-battery"},
		"timezone":     {"America/New_York"},
	}
	resp, body := h.post("/setup", form)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("setup returned %d: %s", resp.StatusCode, truncate(body))
	}
}

func truncate(s string) string {
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

// The Phase 1 exit criteria, walked end to end: setup, log in, create a tab
// with items, post a charge, see a correct derived balance.
func TestPhase1WalkingSkeleton(t *testing.T) {
	h := newHarness(t)

	// An unauthenticated visitor is sent to first-run setup, not to login.
	resp, body := h.get("/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / returned %d", resp.StatusCode)
	}
	if !strings.Contains(body, "Welcome to BitTabby") {
		t.Fatalf("fresh instance did not land on setup; got: %s", truncate(body))
	}

	h.completeSetup()

	// Setup signs the new admin in and lands on an empty dashboard.
	resp, body = h.get("/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard returned %d", resp.StatusCode)
	}
	if !strings.Contains(body, "Your tabs") {
		t.Fatalf("not signed in after setup: %s", truncate(body))
	}
	if !strings.Contains(body, "No tabs yet") {
		t.Errorf("expected the empty state on a fresh dashboard")
	}

	// Create a tab with a two-line breakdown (TAB-01, TAB-04).
	form := url.Values{
		"csrf_token":  {h.csrfToken("/tabs/new")},
		"name":        {"Family phone plan"},
		"description": {"Shared carrier bill"},
		"item_name":   {"Jane line", "Sam line", ""},
		"item_amount": {"45.00", "30.00", ""},
	}
	resp, body = h.post("/tabs", form)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create tab returned %d: %s", resp.StatusCode, truncate(body))
	}
	if !strings.Contains(body, "Family phone plan") {
		t.Fatalf("tab page did not render: %s", truncate(body))
	}
	// The items render with their amounts, and the per-period total is their sum.
	for _, want := range []string{"Jane line", "Sam line", "$45.00", "$30.00", "$75.00"} {
		if !strings.Contains(body, want) {
			t.Errorf("tab page missing %q", want)
		}
	}

	// A fresh tab has a zero balance and says so.
	if !strings.Contains(body, "Settled up") {
		t.Errorf("new tab should read as settled; got: %s", truncate(body))
	}

	// Post a charge (CHG-03).
	form = url.Values{
		"csrf_token": {h.csrfToken("/tabs/1")},
		"amount":     {"75.00"},
		"memo":       {"October"},
	}
	resp, body = h.post("/tabs/1/charges", form)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post charge returned %d: %s", resp.StatusCode, truncate(body))
	}

	// The balance is derived from the entry and shown as owed.
	if !strings.Contains(body, "$75.00 owed") {
		t.Errorf("balance did not read as $75.00 owed: %s", truncate(body))
	}
	if !strings.Contains(body, "October") {
		t.Errorf("charge memo missing from history")
	}

	// And it shows on the dashboard card too.
	_, body = h.get("/")
	if !strings.Contains(body, "Family phone plan") || !strings.Contains(body, "-$75.00") {
		t.Errorf("dashboard card missing tab or balance: %s", truncate(body))
	}
}

// AUTH-03: the setup screen locks permanently once used.
func TestSetupLocksAfterUse(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	// The screen now redirects to login instead of rendering.
	_, body := h.get("/setup")
	if strings.Contains(body, "Create administrator") {
		t.Error("setup screen still reachable after completion")
	}

	// And a direct POST cannot mint a second admin.
	form := url.Values{
		"csrf_token":   {h.csrfToken("/")},
		"display_name": {"Intruder"},
		"email":        {"intruder@example.com"},
		"password":     {"another-long-password"},
		"timezone":     {"UTC"},
	}
	h.post("/setup", form)

	users, err := h.db.ListUsers(t.Context())
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 {
		t.Errorf("%d accounts exist, want 1 -- setup was not locked", len(users))
	}
}

// AUTH-02: sign out, sign in, and reject bad credentials identically.
func TestLoginLogout(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	form := url.Values{"csrf_token": {h.csrfToken("/")}}
	if _, body := h.post("/logout", form); !strings.Contains(body, "Sign in") {
		t.Fatalf("logout did not land on the login page: %s", truncate(body))
	}

	// Protected pages now redirect away.
	_, body := h.get("/")
	if strings.Contains(body, "Your tabs") {
		t.Error("dashboard still reachable after logout")
	}

	// Wrong password and unknown account must produce the same message, so the
	// response cannot be used to discover which addresses are registered.
	const generic = "not correct"

	_, body = h.post("/login", url.Values{
		"csrf_token": {h.csrfToken("/login")},
		"email":      {"jane@example.com"},
		"password":   {"wrong-password-here"},
	})
	if !strings.Contains(body, generic) {
		t.Errorf("wrong password message = %s", truncate(body))
	}

	_, body = h.post("/login", url.Values{
		"csrf_token": {h.csrfToken("/login")},
		"email":      {"nobody@example.com"},
		"password":   {"wrong-password-here"},
	})
	if !strings.Contains(body, generic) {
		t.Errorf("unknown account message = %s", truncate(body))
	}

	// Correct credentials work, and email case does not matter.
	_, body = h.post("/login", url.Values{
		"csrf_token": {h.csrfToken("/login")},
		"email":      {"JANE@EXAMPLE.COM"},
		"password":   {"correct-horse-battery"},
	})
	if !strings.Contains(body, "Your tabs") {
		t.Errorf("valid login failed: %s", truncate(body))
	}
}

// A POST without a valid CSRF token must not take effect.
func TestCSRFRequired(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	// Correct session, absent token.
	h.post("/tabs", url.Values{"name": {"Sneaky tab"}})

	tabs, err := h.db.ListTabsForUser(t.Context(), 1)
	if err != nil {
		t.Fatalf("list tabs: %v", err)
	}
	if len(tabs) != 0 {
		t.Errorf("%d tabs created without a CSRF token, want 0", len(tabs))
	}

	// A wrong token is equally rejected.
	h.post("/tabs", url.Values{
		"csrf_token": {"not-the-right-token"},
		"name":       {"Sneaky tab"},
	})
	tabs, _ = h.db.ListTabsForUser(t.Context(), 1)
	if len(tabs) != 0 {
		t.Errorf("%d tabs created with a forged CSRF token, want 0", len(tabs))
	}
}

// AUTH-05: a tab belonging to someone else answers 404 rather than revealing
// that it exists.
func TestForeignTabIsNotFound(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	// Jane creates a tab.
	h.post("/tabs", url.Values{
		"csrf_token": {h.csrfToken("/tabs/new")},
		"name":       {"Jane's tab"},
	})

	// Sam signs up separately and signs in.
	hash, err := auth.HashPassword("sams-long-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := h.db.CreateUser(t.Context(), storeUser("sam@example.com", "Sam", hash)); err != nil {
		t.Fatalf("create sam: %v", err)
	}

	h.post("/logout", url.Values{"csrf_token": {h.csrfToken("/")}})
	h.post("/login", url.Values{
		"csrf_token": {h.csrfToken("/login")},
		"email":      {"sam@example.com"},
		"password":   {"sams-long-password"},
	})

	// Sam's dashboard is empty, and Jane's tab is a 404 for him.
	_, body := h.get("/")
	if strings.Contains(body, "Jane's tab") {
		t.Error("another user's tab appeared on the dashboard")
	}

	resp, _ := h.get("/tabs/1")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("foreign tab returned %d, want 404", resp.StatusCode)
	}

	// And he cannot post a charge to it either.
	h.post("/tabs/1/charges", url.Values{
		"csrf_token": {h.csrfToken("/")},
		"amount":     {"999.00"},
	})
	balance, err := h.db.SumEntries(t.Context(), 1)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if balance != 0 {
		t.Errorf("foreign charge landed: balance = %s", balance)
	}
}

// A double-submitted charge carrying the same idempotency key posts once
// (LEDGER-07), which is what makes a double-tapped button safe.
func TestDuplicateChargeSubmitPostsOnce(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	h.post("/tabs", url.Values{
		"csrf_token": {h.csrfToken("/tabs/new")},
		"name":       {"Phone plan"},
	})

	token := h.csrfToken("/tabs/1")
	form := url.Values{
		"csrf_token":      {token},
		"amount":          {"45.00"},
		"memo":            {"October"},
		"idempotency_key": {"fixed-key-from-the-form"},
	}
	h.post("/tabs/1/charges", form)
	h.post("/tabs/1/charges", form)

	entries, err := h.db.ListEntries(t.Context(), 1)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries posted from a duplicate submit, want 1", len(entries))
	}
	balance, _ := h.db.SumEntries(t.Context(), 1)
	if balance != -4500 {
		t.Errorf("balance = %s after duplicate submit, want -45.00", balance)
	}
}

// Malformed money must be rejected rather than coerced.
func TestBadAmountRejected(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	h.post("/tabs", url.Values{
		"csrf_token": {h.csrfToken("/tabs/new")},
		"name":       {"Phone plan"},
	})

	for _, bad := range []string{"", "abc", "12.345", "-5.00", "0", "1e3"} {
		h.post("/tabs/1/charges", url.Values{
			"csrf_token": {h.csrfToken("/tabs/1")},
			"amount":     {bad},
		})
	}

	entries, _ := h.db.ListEntries(t.Context(), 1)
	if len(entries) != 0 {
		t.Errorf("%d entries posted from malformed amounts, want 0", len(entries))
	}
}

// Security headers must be present on every response.
func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.get("/login")

	checks := map[string]string{
		"Content-Security-Policy": "default-src 'self'",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "same-origin",
	}
	for header, want := range checks {
		if got := resp.Header.Get(header); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want it to contain %q", header, got, want)
		}
	}
	if csp := resp.Header.Get("Content-Security-Policy"); strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP permits unsafe-inline: %q", csp)
	}
}

// The session cookie must be HttpOnly and SameSite so it cannot be read by
// script or sent on a cross-site POST.
func TestSessionCookieFlags(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	h.post("/logout", url.Values{"csrf_token": {h.csrfToken("/")}})

	resp := h.postNoRedirect("/login", url.Values{
		"csrf_token": {h.csrfToken("/login")},
		"email":      {"jane@example.com"},
		"password":   {"correct-horse-battery"},
	})

	for _, c := range resp.Cookies() {
		if c.Name != auth.SessionCookieName {
			continue
		}
		if !c.HttpOnly {
			t.Error("session cookie is not HttpOnly")
		}
		if c.SameSite != http.SameSiteLaxMode && c.SameSite != http.SameSiteStrictMode {
			t.Errorf("session cookie SameSite = %v, want Lax or Strict", c.SameSite)
		}
		if c.Value == "" {
			t.Error("session cookie has an empty value")
		}
		return
	}
	t.Fatal("login did not set a session cookie")
}

// The Secure flag must follow configuration, so a production deployment cannot
// silently ship cookies over plain HTTP.
func TestSecureCookieFlagFollowsConfig(t *testing.T) {
	h := newHarness(t)
	// The harness runs with SecureCookies=false for plain HTTP.
	resp := h.postNoRedirect("/setup", url.Values{
		"csrf_token":   {h.csrfToken("/setup")},
		"display_name": {"Jane"},
		"email":        {"jane@example.com"},
		"password":     {"correct-horse-battery"},
		"timezone":     {"UTC"},
	})
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName && c.Secure {
			t.Error("Secure flag set despite SecureCookies=false")
		}
	}
}

func TestHealthz(t *testing.T) {
	h := newHarness(t)
	resp, body := h.get("/healthz")
	// The body carries the version so a probe can confirm which build answered.
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "ok ") {
		t.Errorf("healthz = %d %q", resp.StatusCode, body)
	}
	if !strings.Contains(body, version.Short()) {
		t.Errorf("healthz body %q does not carry the version", body)
	}
}

// storeUser is a small constructor keeping the test bodies readable.
func storeUser(email, name, hash string) store.User {
	return store.User{Email: email, DisplayName: name, PasswordHash: hash}
}

// clientFor is a second, independent browser session against the same server,
// so tests can hold two people signed in at once.
type clientFor struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
}

func newClientFor(t *testing.T, h *harness) *clientFor {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return &clientFor{t: t, server: h.server, client: &http.Client{Jar: jar}}
}

func (c *clientFor) get(path string) (*http.Response, string) {
	c.t.Helper()
	resp, err := c.client.Get(c.server.URL + path)
	if err != nil {
		c.t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func (c *clientFor) login(email, password string) {
	c.t.Helper()
	_, body := c.get("/login")
	m := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(body)
	if len(m) < 2 {
		// Already signed in, or the login page did not render a token.
		return
	}
	resp, err := c.client.PostForm(c.server.URL+"/login", url.Values{
		"csrf_token": {m[1]},
		"email":      {email},
		"password":   {password},
	})
	if err != nil {
		c.t.Fatalf("login %s: %v", email, err)
	}
	_ = resp.Body.Close()
}

// ---------------------------------------------------------------------------
// Setup: the timezone picker
// ---------------------------------------------------------------------------

// The setup screen offers the zone list rather than asking someone to recall an
// IANA name exactly.
func TestSetupOffersTimezoneOptions(t *testing.T) {
	h := newHarness(t)
	_, body := h.get("/setup")

	if !strings.Contains(body, `list="timezone-options"`) {
		t.Errorf("the timezone field is not wired to a datalist: %s", truncate(body))
	}
	if !strings.Contains(body, `<datalist id="timezone-options">`) {
		t.Error("no datalist is rendered")
	}
	for _, want := range []string{"UTC", "America/New_York", "Asia/Kolkata"} {
		if !strings.Contains(body, `<option value="`+want+`">`) {
			t.Errorf("%q is not offered", want)
		}
	}
	// Every offered zone must be one the handler would accept, or the picker
	// hands people a value the form then rejects.
	for _, zone := range tz.Zones() {
		if !tz.Valid(zone) {
			t.Errorf("offered zone %q would be rejected on submit", zone)
		}
	}
}

// The datalist is a convenience, not the authority: the server still validates.
func TestSetupRejectsAnUnknownTimezone(t *testing.T) {
	h := newHarness(t)
	_, body := h.post("/setup", url.Values{
		"csrf_token":   {h.csrfToken("/setup")},
		"display_name": {"Admin"},
		"email":        {"admin@example.com"},
		"password":     {"correct-horse-battery-staple"},
		"timezone":     {"Mars/Olympus"},
	})
	if !strings.Contains(body, "not a recognized timezone") {
		t.Errorf("an unknown zone was not rejected: %s", truncate(body))
	}

	// A valid zone absent from the offered list must still be accepted, since
	// the list can lag the runtime's tzdata.
	_, body = h.post("/setup", url.Values{
		"csrf_token":   {h.csrfToken("/setup")},
		"display_name": {"Admin"},
		"email":        {"admin@example.com"},
		"password":     {"correct-horse-battery-staple"},
		"timezone":     {"Etc/GMT+5"},
	})
	if strings.Contains(body, "not a recognized timezone") {
		t.Errorf("a loadable zone outside the offered list was rejected: %s", truncate(body))
	}
}

// ---------------------------------------------------------------------------
// New tab: fields scoped to the chosen kind
// ---------------------------------------------------------------------------

// The create form carries the markup the stylesheet keys off, so the two halves
// can be shown and hidden without JavaScript.
func TestNewTabScopesFieldsByKind(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	_, body := h.get("/tabs/new")

	for _, want := range []string{
		`class="stack kindform"`,
		`id="kind-services"`,
		`id="kind-payoff"`,
		`class="stack payoff-only"`,
		`class="stack services-only"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in the create form: %s", want, truncate(body))
		}
	}

	// Services is the default selection, matching parseTabKind's default.
	if !strings.Contains(body, `id="kind-services" value="services" checked`) {
		t.Errorf("Services is not preselected: %s", truncate(body))
	}

	// Nothing that gets hidden may be `required` -- the browser refuses to
	// submit a form with an invalid hidden field and cannot show why.
	for _, field := range []string{"loan_amount", "interest_apr", "loan_term", "loan_payment", "item_name", "item_amount"} {
		for _, tag := range findInputs(body, field) {
			if strings.Contains(tag, "required") {
				t.Errorf("%s is required but lives in a kind-scoped block: %s", field, tag)
			}
		}
	}
}

// Both kinds still create correctly through the reworked form.
func TestNewTabCreatesEitherKind(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	h.post("/tabs", url.Values{
		"csrf_token":  {h.csrfToken("/tabs/new")},
		"name":        {"Phone plan"},
		"kind":        {"services"},
		"item_name":   {"Line"},
		"item_amount": {"40.00"},
	})
	h.post("/tabs", url.Values{
		"csrf_token":   {h.csrfToken("/tabs/new")},
		"name":         {"Car loan"},
		"kind":         {"payoff"},
		"loan_amount":  {"21852.48"},
		"interest_apr": {"5.24"},
		"loan_term":    {"48"},
		"loan_payment": {"505.65"},
	})

	tabs, err := h.db.ListTabsForUser(t.Context(), 1)
	if err != nil {
		t.Fatalf("list tabs: %v", err)
	}
	byName := make(map[string]store.Tab, len(tabs))
	for _, tab := range tabs {
		byName[tab.Name] = tab
	}

	services, ok := byName["Phone plan"]
	if !ok {
		t.Fatal("the Services tab was not created")
	}
	if services.Kind != store.TabServices || services.LoanTermPeriods != 0 || services.LoanPayment != 0 {
		t.Errorf("Services tab picked up loan fields: %+v", services)
	}

	payoff, ok := byName["Car loan"]
	if !ok {
		t.Fatal("the Payoff tab was not created")
	}
	if payoff.LoanTermPeriods != 48 || payoff.LoanPayment != 50_565 {
		t.Errorf("Payoff tab terms are %d periods at %s",
			payoff.LoanTermPeriods, payoff.LoanPayment.Display())
	}
	// A Payoff tab takes no line items, whatever the form posted.
	items, err := h.db.ListItems(t.Context(), payoff.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("the Payoff tab carries %d line items, want none", len(items))
	}
}

// findInputs returns every <input> tag in body carrying the given name.
func findInputs(body, name string) []string {
	var out []string
	needle := `name="` + name + `"`
	for i := 0; ; {
		j := strings.Index(body[i:], needle)
		if j < 0 {
			return out
		}
		j += i
		start := strings.LastIndex(body[:j], "<input")
		end := strings.Index(body[j:], ">")
		if start >= 0 && end >= 0 {
			out = append(out, body[start:j+end+1])
		}
		i = j + len(needle)
	}
}

// ---------------------------------------------------------------------------
// Static assets: cache busting
// ---------------------------------------------------------------------------

// A long cache lifetime on a stable URL is how a shipped stylesheet change goes
// unnoticed. The URL has to change when the content does.
func TestAssetURLsCarryTheContentDigest(t *testing.T) {
	v := AssetVersion()
	if len(v) != 12 {
		t.Fatalf("AssetVersion() = %q, want 12 hex characters", v)
	}
	if v != AssetVersion() {
		t.Error("AssetVersion is not stable across calls")
	}
	if got := AssetURL("app.css"); got != "/static/app.css?v="+v {
		t.Errorf("AssetURL = %q", got)
	}

	h := newHarness(t)
	_, body := h.get("/login")
	for _, asset := range []string{"app.css", "htmx.min.js"} {
		want := "/static/" + asset + "?v=" + v
		if !strings.Contains(body, want) {
			t.Errorf("the page does not reference %s with its digest: %s", asset, truncate(body))
		}
		// A bare reference would be cached for a minute and is a bug waiting to
		// happen, so none should remain.
		if strings.Contains(body, `"/static/`+asset+`"`) {
			t.Errorf("%s is referenced without a digest", asset)
		}
	}
}

// Only a request carrying the current digest may be cached indefinitely.
func TestStaticCacheLifetimeDependsOnTheDigest(t *testing.T) {
	h := newHarness(t)

	resp, _ := h.get("/static/app.css?v=" + AssetVersion())
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("digested asset Cache-Control = %q, want immutable", got)
	}

	for _, path := range []string{"/static/app.css", "/static/app.css?v=stale"} {
		resp, _ = h.get(path)
		got := resp.Header.Get("Cache-Control")
		if strings.Contains(got, "immutable") {
			t.Errorf("%s was served as immutable (%q) without the current digest", path, got)
		}
		if !strings.Contains(got, "max-age=60") {
			t.Errorf("%s Cache-Control = %q, want a short lifetime", path, got)
		}
	}
}

// Every input type the forms actually use must be styled, or fields render at
// browser defaults beside styled ones and stop lining up. This is a stylesheet
// assertion rather than a rendering one; the visual check is a Playwright run.
func TestStylesheetCoversEveryInputTypeInUse(t *testing.T) {
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read stylesheet: %v", err)
	}
	sheet := string(css)

	h := newHarness(t)
	h.completeSetup()

	seen := make(map[string]bool)
	for _, path := range []string{"/tabs/new", "/"} {
		_, body := h.get(path)
		for _, chunk := range strings.Split(body, `<input type="`)[1:] {
			if end := strings.Index(chunk, `"`); end > 0 {
				seen[chunk[:end]] = true
			}
		}
	}

	for typ := range seen {
		switch typ {
		case "hidden", "radio", "checkbox", "submit":
			continue // deliberately not given the text-field treatment
		}
		if !strings.Contains(sheet, `input[type="`+typ+`"]`) {
			t.Errorf("forms use input[type=%q] but the stylesheet never mentions it, "+
				"so it renders at browser defaults beside styled fields", typ)
		}
	}
}

// postRaw posts to a path with a given Authorization header and no session,
// used to exercise the cron tick endpoint.
func (h *harness) postRaw(t *testing.T, path, authHeader string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// srv returns the underlying Server for white-box tests.
func (h *harness) srv() *Server { return h.server0 }
