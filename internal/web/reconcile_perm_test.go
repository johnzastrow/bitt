package web

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/johnzastrow/bitt/internal/store"
)

// RECON-01: the "Can reconcile" permission, the instance switch, and the guard
// on every reconciliation route.

// lockedBuffer is a log sink safe for the server's goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLog points the server's logger at a buffer for the test.
func (h *harness) captureLog() *lockedBuffer {
	buf := &lockedBuffer{}
	h.srv().log = slog.New(slog.NewTextHandler(buf, nil))
	return buf
}

func (h *harness) setReconcile(id int64, on bool) (*http.Response, string) {
	h.t.Helper()
	v := "false"
	if on {
		v = "true"
	}
	return h.post("/admin/users/"+itoa(id)+"/reconcile", url.Values{
		"csrf_token":    {h.csrfToken("/admin/users")},
		"can_reconcile": {v},
	})
}

func (h *harness) setSwitch(on bool) (*http.Response, string) {
	h.t.Helper()
	v := "false"
	if on {
		v = "true"
	}
	return h.post("/admin/reconcile-switch", url.Values{
		"csrf_token": {h.csrfToken("/admin/users")},
		"enabled":    {v},
	})
}

func (h *harness) holds(id int64) bool {
	h.t.Helper()
	u, err := h.db.GetUser(h.t.Context(), id)
	if err != nil {
		h.t.Fatalf("get user: %v", err)
	}
	return u.CanReconcile
}

func (h *harness) switchOn() bool {
	h.t.Helper()
	inst, err := h.db.GetInstance(h.t.Context())
	if err != nil {
		h.t.Fatalf("get instance: %v", err)
	}
	return inst.ReconcileEnabled
}

func TestReconcileSwitch(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	logs := h.captureLog()

	_, body := h.get("/admin/users")
	if !strings.Contains(body, "Bank reconciliation") || !strings.Contains(body, "Turn on") {
		t.Fatalf("People screen lacks the switch: %s", truncate(body))
	}

	if _, body := h.setSwitch(true); !strings.Contains(body, "Bank reconciliation is on.") {
		t.Errorf("turning on: %s", truncate(body))
	}
	if !h.switchOn() {
		t.Fatal("switch did not turn on")
	}
	// Again: a no-op save must still succeed (the MariaDB no-op class).
	if resp, _ := h.setSwitch(true); resp.StatusCode != http.StatusOK || !h.switchOn() {
		t.Errorf("repeating on: %d", resp.StatusCode)
	}
	if _, body := h.get("/admin/users"); !strings.Contains(body, "Turn off") {
		t.Error("switch does not show as on")
	}
	if _, body := h.setSwitch(false); !strings.Contains(body, "Bank reconciliation is off.") || h.switchOn() {
		t.Errorf("turning off: %s", truncate(body))
	}
	if !strings.Contains(logs.String(), "bank reconciliation switched") ||
		!strings.Contains(logs.String(), "by_user_id=1") {
		t.Errorf("switch change not logged with the actor: %s", logs.String())
	}
}

func TestReconcileSwitchRefusals(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	// No or forged CSRF token, or a value that is neither true nor false.
	for _, form := range []url.Values{
		{"enabled": {"true"}},
		{"enabled": {"true"}, "csrf_token": {"forged"}},
		{"enabled": {"1"}, "csrf_token": {h.csrfToken("/admin/users")}},
		{"csrf_token": {h.csrfToken("/admin/users")}},
	} {
		h.post("/admin/reconcile-switch", form)
		if h.switchOn() {
			t.Fatalf("switch turned on by %v", form)
		}
	}

	// A non-administrator gets 404 and changes nothing.
	h.addUser("sam@example.com", "Sam", false)
	h.loginAs("sam@example.com", "a-long-enough-password")
	resp, _ := h.post("/admin/reconcile-switch", url.Values{
		"csrf_token": {h.csrfToken("/")}, "enabled": {"true"},
	})
	if resp.StatusCode != http.StatusNotFound || h.switchOn() {
		t.Errorf("non-admin: status %d, switch %v", resp.StatusCode, h.switchOn())
	}
}

func TestGrantAndRemoveCanReconcile(t *testing.T) {
	h := newHarness(t)
	h.completeSetup() // Jane, id 1
	logs := h.captureLog()
	ann := h.addUser("ann@example.com", "Ann Admin", true)
	sam := h.addUser("sam@example.com", "Sam", false)

	_, body := h.get("/admin/users")
	// Offered on each administrator's row, not on Sam's.
	if n := strings.Count(body, "Allow reconciling"); n != 2 {
		t.Errorf("%d grant controls, want 2 (one per administrator)", n)
	}
	if strings.Contains(body, "/admin/users/"+itoa(sam.ID)+"/reconcile") {
		t.Error("a non-administrator's row offers the permission")
	}

	// Grant to another administrator, then to oneself.
	if _, body := h.setReconcile(ann.ID, true); !strings.Contains(body, "can now reconcile") {
		t.Errorf("grant: %s", truncate(body))
	}
	if !h.holds(ann.ID) {
		t.Fatal("Ann was not granted")
	}
	h.setReconcile(1, true)
	if !h.holds(1) {
		t.Fatal("self-grant failed")
	}
	if _, body := h.get("/admin/users"); strings.Count(body, "can reconcile</span>") != 2 {
		t.Error("holders are not marked on the People screen")
	}

	// Remove.
	if _, body := h.setReconcile(ann.ID, false); !strings.Contains(body, "no longer reconcile") || h.holds(ann.ID) {
		t.Errorf("remove: %s", truncate(body))
	}

	// A non-administrator cannot be given it, even by a direct post.
	if _, body := h.setReconcile(sam.ID, true); !strings.Contains(body, "Only an administrator") {
		t.Errorf("grant to non-admin was not refused: %s", truncate(body))
	}
	if h.holds(sam.ID) {
		t.Fatal("non-administrator holds the permission")
	}

	// Unknown account.
	if resp, _ := h.setReconcile(99999, true); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown account: %d, want 404", resp.StatusCode)
	}

	log := logs.String()
	for _, want := range []string{
		"reconcile permission changed",
		"target_user_id=" + itoa(ann.ID),
		"can_reconcile=true",
		"can_reconcile=false",
		"by_user_id=1",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q: %s", want, log)
		}
	}
}

func TestGrantCanReconcileRefusals(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	ann := h.addUser("ann@example.com", "Ann Admin", true)

	path := "/admin/users/" + itoa(ann.ID) + "/reconcile"
	for _, form := range []url.Values{
		{"can_reconcile": {"true"}},
		{"can_reconcile": {"true"}, "csrf_token": {"forged"}},
		{"can_reconcile": {"yes"}, "csrf_token": {h.csrfToken("/admin/users")}},
		{"csrf_token": {h.csrfToken("/admin/users")}},
	} {
		h.post(path, form)
		if h.holds(ann.ID) {
			t.Fatalf("granted by %v", form)
		}
	}

	// Only an administrator may grant: a non-administrator gets 404.
	h.addUser("sam@example.com", "Sam", false)
	h.loginAs("sam@example.com", "a-long-enough-password")
	resp, _ := h.post(path, url.Values{"csrf_token": {h.csrfToken("/")}, "can_reconcile": {"true"}})
	if resp.StatusCode != http.StatusNotFound || h.holds(ann.ID) {
		t.Errorf("non-admin grant: status %d, held %v", resp.StatusCode, h.holds(ann.ID))
	}
}

// The guard on /admin/reconcile, across every combination that matters, and
// re-evaluated on each request: removing the permission or the switch takes
// effect on the very next page.
func TestReconcileRouteGuard(t *testing.T) {
	h := newHarness(t)
	h.completeSetup() // Jane, id 1, administrator
	logs := h.captureLog()
	h.addUser("sam@example.com", "Sam", false)

	status := func() int {
		resp, _ := h.get("/admin/reconcile")
		return resp.StatusCode
	}

	// Switch off, no permission: hidden.
	if got := status(); got != http.StatusNotFound {
		t.Errorf("switch off, no permission: %d, want 404", got)
	}
	// Permission but switch off: still hidden.
	h.setReconcile(1, true)
	if got := status(); got != http.StatusNotFound {
		t.Errorf("switch off, holder: %d, want 404", got)
	}
	// Both: allowed.
	h.setSwitch(true)
	if got := status(); got != http.StatusOK {
		t.Errorf("switch on, holder: %d, want 200", got)
	}
	// Permission removed: 403 on the next request.
	h.setReconcile(1, false)
	if got := status(); got != http.StatusForbidden {
		t.Errorf("switch on, permission removed: %d, want 403", got)
	}
	if !strings.Contains(logs.String(), "reconciliation route denied") {
		t.Error("a denied reconciliation request was not logged")
	}
	// Restored, then the switch turned off: 404 again.
	h.setReconcile(1, true)
	h.setSwitch(false)
	if got := status(); got != http.StatusNotFound {
		t.Errorf("switch turned off: %d, want 404", got)
	}

	// A non-administrator: 404 whatever the switch says.
	h.setSwitch(true)
	h.loginAs("sam@example.com", "a-long-enough-password")
	if got := status(); got != http.StatusNotFound {
		t.Errorf("non-admin, switch on: %d, want 404", got)
	}

	// Signed out: sent to sign in, never the page.
	h.post("/logout", url.Values{"csrf_token": {h.csrfToken("/")}})
	if _, body := h.get("/admin/reconcile"); strings.Contains(body, "<h1>Reconciliation</h1>") {
		t.Error("signed-out visitor reached the reconciliation page")
	}
}

// The menu shows Reconciliation only to a holder with the switch on.
func TestReconcileMenuItem(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	hasItem := func() bool {
		_, body := h.get("/")
		for _, it := range menuItems(t, body) {
			if it == "Reconciliation" {
				return true
			}
		}
		return false
	}

	if hasItem() {
		t.Error("shown with neither permission nor switch")
	}
	h.setSwitch(true)
	if hasItem() {
		t.Error("shown to an administrator without the permission")
	}
	h.setReconcile(1, true)
	if !hasItem() {
		t.Fatal("not shown to a holder with the switch on")
	}
	_, body := h.get("/")
	if got, want := strings.Join(menuItems(t, body), ","),
		"Profile,People,Notifications,Reconciliation,Log out"; got != want {
		t.Errorf("menu = %q, want %q", got, want)
	}
	// Marked current on its own page.
	if _, body := h.get("/admin/reconcile"); !strings.Contains(menu(t, body),
		`href="/admin/reconcile" aria-current="page"`) {
		t.Error("Reconciliation not marked current on its page")
	}
	h.setSwitch(false)
	if hasItem() {
		t.Error("still shown after the switch was turned off")
	}
}

// Can reconcile does not change who may post a payment (AUTH-05): a holder who
// is not on a tab is still refused by the ordinary payment form.
func TestCanReconcileDoesNotGrantTransact(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	h.setSwitch(true)
	h.setReconcile(1, true)

	h.addUser("sam@example.com", "Sam Provider", false)
	h.loginAs("sam@example.com", "a-long-enough-password")
	tab := h.makeTabAs("Sam's tab", 2, "Line", "10.00")
	tabID := tabIDFrom(t, mustBody(t, h, tab))

	h.loginAs("jane@example.com", "correct-horse-battery")
	if !h.holds(1) {
		t.Fatal("precondition: Jane holds Can reconcile")
	}
	_, body := h.get(tab)
	if strings.Contains(body, "Record a payment</h2>") {
		t.Error("a holder not on the tab was offered the payment form")
	}
	h.post(tab+"/payments", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"amount":     {"10.00"},
		"method":     {string(store.MethodTransfer)},
	})
	h.post(tab+"/adjustments", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"amount":     {"-5.00"},
		"reason":     {"bank said so"},
	})
	balance, err := h.db.SumEntries(t.Context(), tabID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != 0 {
		t.Errorf("a holder not on the tab moved money: balance %s, want 0", balance)
	}
}

// The accounts table is wider than a phone. On a phone it stacks into one card
// per account (table.accounts); on wider screens it scrolls inside its own
// container rather than widening the page (it measured 614 px at 360 px).
func TestAccountsTableScrollsInItsCard(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	_, body := h.get("/admin/users")
	if !regexp.MustCompile(`<div class="tablescroll">\s*<table class="entries accounts">`).MatchString(body) {
		t.Error("the accounts table is not inside a .tablescroll container")
	}
}
