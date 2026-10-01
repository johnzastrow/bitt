package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/reconcile"
	"github.com/johnzastrow/bitt/internal/store"
)

// RECON-03 on the web: suggestions on the Reconciliation screen, the setup
// controls, and ignore rules.

// scenario imports the reference file and records payments around it:
//
//	Phone plan (Jane, Sam Payee): $150 transfer 9/1 -> exact match, row 2
//	                              $50 transfer 9/5  -> three $50 lines: a tie
//	                              $30 cash 9/2, reversed -> never suggested
//	Insurance (Jane, Alex Other): $1,190 transfer 9/3 -> $1,200 line, $10 over
//	Garden (Jane):                $20 cash 9/20 -> no line: unmatched payment
//
// Row 9's $75.25 check has no payment; the two dividends are under $1.
func scenario(t *testing.T) *harness {
	t.Helper()
	h := reconcileReady(t)
	ctx := t.Context()
	sam := h.addUser("sam@example.com", "Sam Payee", false)
	alex := h.addUser("alex@example.com", "Alex Other", false)

	tab := func(name string, people ...store.User) store.Tab {
		tb, err := h.db.CreateTab(ctx, store.Tab{Name: name, Kind: store.TabServices, CreatedBy: 1}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range people {
			if err := h.db.AddParticipant(ctx, store.Participant{TabID: tb.ID, UserID: u.ID, Role: store.RolePayee}); err != nil {
				t.Fatal(err)
			}
		}
		return tb
	}
	pay := func(tb store.Tab, key string, cents money.Cents, d int, m store.PaymentMethod, actor int64) store.Entry {
		e, _, err := h.db.PostEntry(ctx, store.NewEntry{TabID: tb.ID, Kind: store.KindPayment, Amount: cents,
			EffectiveAt: time.Date(2026, 9, d, 16, 0, 0, 0, time.UTC), ActorUserID: actor,
			IdempotencyKey: key, Method: m})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	phone := tab("Phone plan", sam)
	ins := tab("Insurance", alex)
	garden := tab("Garden")
	pay(phone, "p150", 15000, 1, store.MethodTransfer, sam.ID)
	pay(phone, "p50", 5000, 5, store.MethodTransfer, sam.ID)
	undone := pay(phone, "p30", 3000, 2, store.MethodCash, sam.ID)
	if _, _, err := h.db.PostEntry(ctx, store.NewEntry{TabID: phone.ID, Kind: store.KindReversal, Amount: -3000,
		EffectiveAt: time.Date(2026, 9, 2, 17, 0, 0, 0, time.UTC), ActorUserID: sam.ID,
		IdempotencyKey: "undo30", ReversesSeq: &undone.Seq}); err != nil {
		t.Fatal(err)
	}
	pay(ins, "p1190", 119000, 3, store.MethodTransfer, alex.ID)
	pay(garden, "p20", 2000, 20, store.MethodCash, 1)
	// 02:00 UTC on 9/22 is the evening of 9/21 in New York, the instance zone.
	if _, _, err := h.db.PostEntry(ctx, store.NewEntry{TabID: garden.ID, Kind: store.KindPayment, Amount: 2200,
		EffectiveAt: time.Date(2026, 9, 22, 2, 0, 0, 0, time.UTC), ActorUserID: 1,
		IdempotencyKey: "p22", Method: store.MethodCash}); err != nil {
		t.Fatal(err)
	}

	h.importReference(t)
	return h
}

func section(t *testing.T, body, heading string) string {
	t.Helper()
	i := strings.Index(body, heading)
	if i < 0 {
		t.Fatalf("no %q on the page", heading)
	}
	rest := body[i:]
	if j := strings.Index(rest[1:], "<h2>"); j >= 0 {
		return rest[:j+1]
	}
	return rest
}

func TestSuggestionsOnTheScreen(t *testing.T) {
	h := scenario(t)
	_, body := h.get("/admin/reconcile")

	sugg := section(t, body, "<h2>Suggested matches</h2>")
	// Exact $150 on Phone plan: nothing to post.
	for _, want := range []string{
		"Phone plan", "$150.00", "Same amount.", "A name matches.", "nothing; the match is recorded.",
		"Row 2 of TransactionHistory.csv",
		// $1,200 in the bank against $1,190 recorded: the extra is a payment.
		"Insurance", "$1,190.00", "$1,200.00", "Differs by $10.00.", "Bank date 1 day earlier",
		"a payment of $10.00 for the extra the bank received.",
	} {
		if !strings.Contains(sugg, want) {
			t.Errorf("suggestions lack %q", want)
		}
	}
	// Two single suggestions; the tie's pairs are shown separately.
	if n := strings.Count(sugg, `class="matchrow"`); n != 2 {
		t.Errorf("%d suggestions, want 2", n)
	}
	// The three $50 lines on 9/5 and the one $50 payment: not guessed, shown
	// in their own panel.
	if !strings.Contains(body, "3 possible bank lines for one payment") {
		t.Error("the tie is not shown")
	}
	// Recorded dates are shown in the instance's timezone: the Garden payment
	// at 16:00 UTC on 9/20 is still Sep 20 in New York; one at 02:00 UTC on
	// 9/21 is Sep 20 there too.
	if !strings.Contains(body, "Sep 20, 2026") {
		t.Error("the garden payment's date is missing")
	}
	// The reversed $30 payment is never offered anywhere.
	if strings.Contains(body, "$30.00") {
		t.Error("a reversed payment appears on the screen")
	}

	unmatched := section(t, body, "<h2>Bank lines with no matching payment</h2>")
	if !strings.Contains(unmatched, "Check Deposit") || !strings.Contains(unmatched, "$75.25") {
		t.Error("the check deposit is not listed as unmatched")
	}
	if !strings.Contains(unmatched, "2 open lines below the minimum are not offered here.") ||
		!strings.Contains(body, "below the\n") && !strings.Contains(body, "$1.00 minimum") {
		t.Errorf("the dividends are not counted as small: %s", unmatched)
	}
	if strings.Contains(unmatched, "Dividend") {
		t.Error("a dividend is offered as an unmatched line")
	}
	if !strings.Contains(body, "Sep 21, 2026") || strings.Contains(body, "Sep 22, 2026") {
		t.Error("an evening payment is shown on its UTC date, not the instance's")
	}
	if !strings.Contains(body, `<h2>Recorded payments with no bank transaction</h2><span class="tabcount">2</span>`) ||
		!strings.Contains(body, "Garden") || !strings.Contains(body, "Not in the bank") {
		t.Error("the garden cash payment is not listed as unmatched")
	}
}

// The setup controls change what is suggested, and a change is logged.
func TestSetupControlsChangeSuggestions(t *testing.T) {
	h := scenario(t)
	logs := h.captureLog()

	save := func(changes map[string]string) string {
		t.Helper()
		f := url.Values{
			"csrf_token":        {h.csrfToken("/admin/reconcile")},
			"date_window_days":  {"7"},
			"tolerance_percent": {"10"},
			"tolerance_cap":     {"25.00"},
			"tolerance_floor":   {"1.00"},
			"min_line_amount":   {"1.00"},
			"use_names":         {"1"},
			"date_flag_days":    {"3"},
		}
		for k, v := range changes {
			if v == "" {
				f.Del(k)
			} else {
				f.Set(k, v)
			}
		}
		_, body := h.post("/admin/reconcile/settings", f)
		return body
	}

	// A $5 cap leaves the $10 difference outside: Insurance is no longer suggested.
	if body := save(map[string]string{"tolerance_cap": "5.00"}); !strings.Contains(body, "Reconciliation setup saved.") {
		t.Fatalf("save: %s", truncate(body))
	}
	_, body := h.get("/admin/reconcile")
	if strings.Contains(section(t, body, "<h2>Suggested matches</h2>"), "Insurance") {
		t.Error("the cap did not take Insurance out")
	}
	if _, sb := h.get("/admin/reconcile/setup"); !strings.Contains(sb, "Last changed") || !strings.Contains(sb, "by Jane Provider") {
		t.Error("who changed the setup is not shown")
	}

	// A minimum of zero offers the dividends; a minimum of $100 hides the $50s
	// and the check, so the tie disappears.
	save(map[string]string{"min_line_amount": "0"})
	_, body = h.get("/admin/reconcile")
	if !strings.Contains(section(t, body, "<h2>Bank lines with no matching payment</h2>"), "Dividend") {
		t.Error("minimum 0 did not offer the dividends")
	}
	save(map[string]string{"min_line_amount": "100.00"})
	_, body = h.get("/admin/reconcile")
	if strings.Contains(body, "possible bank lines") {
		t.Error("minimum $100 still offered the $50 lines")
	}

	// A zero-day window keeps only same-day pairs: Insurance (a day apart) goes.
	save(map[string]string{"date_window_days": "0"})
	_, body = h.get("/admin/reconcile")
	s := section(t, body, "<h2>Suggested matches</h2>")
	if strings.Contains(s, "Insurance") || !strings.Contains(s, "Phone plan") {
		t.Errorf("zero window: %s", s)
	}

	// Date flag 0: the one-day gap on Insurance is flagged.
	save(map[string]string{"date_flag_days": "0"})
	_, body = h.get("/admin/reconcile")
	if !strings.Contains(body, "Dates differ by more than 0 days") {
		t.Error("date flag 0 did not flag a one-day gap")
	}

	for _, want := range []string{"reconciliation setup changed", "by_user_id=1", "cap=25.00", "cap=5.00"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q", want)
		}
	}
	got, _ := h.db.GetReconcileSettings(t.Context())
	if got.UpdatedBy != 1 || got.DateFlagDays != 0 {
		t.Errorf("stored = %+v", got)
	}
}

func TestSetupRefusesOutOfRange(t *testing.T) {
	h := reconcileReady(t)
	cases := map[string][2]string{
		"window over 60":    {"date_window_days", "61"},
		"window negative":   {"date_window_days", "-1"},
		"window not number": {"date_window_days", "seven"},
		"percent over 100":  {"tolerance_percent", "101"},
		"cap over 10,000":   {"tolerance_cap", "10000.01"},
		"floor over 100":    {"tolerance_floor", "100.01"},
		"floor over cap":    {"tolerance_floor", "30.00"},
		"floor negative":    {"tolerance_floor", "-1"},
		"cap three decimal": {"tolerance_cap", "1.005"},
		"min over 10,000":   {"min_line_amount", "10000.01"},
		"flag over 60":      {"date_flag_days", "61"},
		"missing field":     {"date_flag_days", ""},
	}
	for name, c := range cases {
		f := url.Values{
			"csrf_token":        {h.csrfToken("/admin/reconcile")},
			"date_window_days":  {"7"},
			"tolerance_percent": {"10"},
			"tolerance_cap":     {"25.00"},
			"tolerance_floor":   {"1.00"},
			"min_line_amount":   {"1.00"},
			"date_flag_days":    {"3"},
		}
		f.Set(c[0], c[1])
		_, body := h.post("/admin/reconcile/settings", f)
		if !strings.Contains(body, "Setup not saved") {
			t.Errorf("%s: not refused", name)
		}
		if got, _ := h.db.GetReconcileSettings(t.Context()); got.Settings != reconcile.Defaults() {
			t.Fatalf("%s: settings changed to %+v", name, got.Settings)
		}
	}
	// Without a CSRF token.
	h.post("/admin/reconcile/settings", url.Values{"date_window_days": {"1"}, "tolerance_percent": {"1"},
		"tolerance_cap": {"1"}, "tolerance_floor": {"1"}, "min_line_amount": {"1"}, "date_flag_days": {"1"}})
	if got, _ := h.db.GetReconcileSettings(t.Context()); got.Settings != reconcile.Defaults() {
		t.Error("saved without a CSRF token")
	}
}

// Exit criterion: dividends below the minimum are not offered, and an ignore
// rule marks them ignored with its reason -- including when the rule comes
// before the import.
func TestIgnoreRuleOnTheWeb(t *testing.T) {
	h := reconcileReady(t)
	logs := h.captureLog()
	h.importReference(t)
	formats, _ := h.db.ListBankFormats(t.Context())
	fid := itoa(formats[0].ID)

	_, body := h.post("/admin/reconcile/rules", url.Values{
		"csrf_token": {h.csrfToken("/admin/reconcile")}, "format_id": {fid}, "contains": {"Deposit Dividend"},
	})
	if !strings.Contains(body, "Rule added; 2 open lines now ignored.") {
		t.Fatalf("add rule: %s", truncate(body))
	}
	if !strings.Contains(body, "Credit union: description contains &ldquo;Deposit Dividend&rdquo;") &&
		!strings.Contains(body, "Credit union: description contains “Deposit Dividend”") {
		t.Error("the rule is not listed")
	}
	if !strings.Contains(logs.String(), "ignore rule added") || !strings.Contains(logs.String(), "lines_ignored=2") {
		t.Errorf("rule not logged: %s", logs.String())
	}

	// A later file's dividend is imported as ignored, with the rule as reason.
	later := "Account,Date,Description,Note,Check #,Amount,Balance\r\n" +
		"PRIMARY SHARE,10/31/2026,\"Deposit Dividend\nANNUAL PERCENTAGE YIELD EARNED 0.05%\",,,$0.09,$9.00\r\n" +
		"PRIMARY SHARE,10/31/2026,\"Deposit Transfer\nFROM SAM PAYEE X1234\",,,$150.00,$159.00\r\n"
	resp, body := h.upload(t, "Oct.csv", []byte(later))
	if !strings.Contains(body, "Imported: 1 new, 1 ignored by rule, 0 outgoing dropped.") {
		t.Fatalf("import with a rule: %s", truncate(body))
	}
	if !strings.Contains(body, `Ignored: description contains &#34;Deposit Dividend&#34;`) {
		t.Errorf("the ignored line does not show its reason: %s", truncate(body))
	}
	_ = resp

	// Refusals: the same rule again, no text, two lines, an unknown layout.
	for name, f := range map[string]url.Values{
		"duplicate":      {"format_id": {fid}, "contains": {"Deposit Dividend"}},
		"empty":          {"format_id": {fid}, "contains": {"  "}},
		"multi-line":     {"format_id": {fid}, "contains": {"a\nb"}},
		"unknown layout": {"format_id": {"9999"}, "contains": {"x"}},
		"bad layout":     {"format_id": {"abc"}, "contains": {"x"}},
	} {
		f.Set("csrf_token", h.csrfToken("/admin/reconcile"))
		if _, body := h.post("/admin/reconcile/rules", f); strings.Contains(body, "Rule added") {
			t.Errorf("%s: accepted", name)
		}
	}
	if rules, _ := h.db.ListIgnoreRules(t.Context()); len(rules) != 1 {
		t.Fatalf("%d rules, want 1", len(rules))
	}

	// Remove it: lines stay ignored.
	rules, _ := h.db.ListIgnoreRules(t.Context())
	_, body = h.post("/admin/reconcile/rules/"+itoa(rules[0].ID)+"/delete", url.Values{
		"csrf_token": {h.csrfToken("/admin/reconcile")},
	})
	if !strings.Contains(body, "Rule removed") {
		t.Errorf("remove: %s", truncate(body))
	}
	if r, _ := h.post("/admin/reconcile/rules/"+itoa(rules[0].ID)+"/delete", url.Values{
		"csrf_token": {h.csrfToken("/admin/reconcile")},
	}); r.StatusCode != http.StatusNotFound {
		t.Errorf("removing twice: %d", r.StatusCode)
	}
	open, _ := h.db.ListOpenBankLines(t.Context())
	for _, l := range open {
		if strings.Contains(l.Description, "Dividend") {
			t.Errorf("a dividend reopened after the rule was removed: %+v", l)
		}
	}
}

// The setup and rule routes are behind the same guard as the rest.
func TestSetupRoutesAreGuarded(t *testing.T) {
	h := reconcileReady(t)
	h.setReconcile(1, false)
	for _, p := range []string{"/admin/reconcile/settings", "/admin/reconcile/rules", "/admin/reconcile/rules/1/delete"} {
		if r, _ := h.post(p, url.Values{"csrf_token": {h.csrfToken("/")}}); r.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s without permission: %d", p, r.StatusCode)
		}
	}
	// A non-administrator: 404.
	h.addUser("sam@example.com", "Sam", false)
	h.loginAs("sam@example.com", "a-long-enough-password")
	for _, p := range []string{"/admin/reconcile/settings", "/admin/reconcile/rules"} {
		if r, _ := h.post(p, url.Values{"csrf_token": {h.csrfToken("/")}}); r.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s as non-admin: %d", p, r.StatusCode)
		}
	}
}

func TestRedirectWithKeepsQueryBeforeFragment(t *testing.T) {
	cases := map[string]string{
		"/admin/reconcile#setup": "/admin/reconcile?ok=Saved.#setup",
		"/a?x=1#f":               "/a?x=1&ok=Saved.#f",
		"/plain":                 "/plain?ok=Saved.",
	}
	for in, want := range cases {
		rec := httptest.NewRecorder()
		redirectWith(rec, httptest.NewRequest(http.MethodPost, "/", nil), in, "ok", "Saved.")
		if got := rec.Header().Get("Location"); got != want {
			t.Errorf("redirectWith(%q) -> %q, want %q", in, got, want)
		}
	}
}
