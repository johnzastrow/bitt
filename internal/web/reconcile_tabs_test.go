package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
)

// The owner's 2026-10-01 additions: a tabbed Reconciliation screen, renaming
// layouts, counts of what is still unaddressed, accounting for payments with
// no bank transaction, and a tab-history filter for unreconciled payments.

var tabCountRe = regexp.MustCompile(`aria-current="page">\s*Reconcile\s*<span class="tabcount">(\d+)</span>`)

func TestReconciliationTabs(t *testing.T) {
	h := scenario(t)
	cases := []struct {
		path, current string
		has, lacks    []string
	}{
		{"/admin/reconcile", "Reconcile", []string{"<h2>Suggested matches</h2>", "not yet addressed"},
			[]string{"Import a bank statement", "Date window, days"}},
		{"/admin/reconcile/upload", "Upload", []string{"Import a bank statement", "Recent imports"},
			[]string{"<h2>Suggested matches</h2>", "Date window, days"}},
		{"/admin/reconcile/setup", "Setup", []string{"Date window, days", "Ignore rules", "Saved layouts"},
			[]string{"<h2>Suggested matches</h2>", "Import a bank statement"}},
	}
	for _, c := range cases {
		resp, body := h.get(c.path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", c.path, resp.StatusCode)
		}
		if !regexp.MustCompile(`aria-current="page">\s*` + c.current).MatchString(body) {
			t.Errorf("%s: %s is not the current tab", c.path, c.current)
		}
		for _, w := range c.has {
			if !strings.Contains(body, w) {
				t.Errorf("%s lacks %q", c.path, w)
			}
		}
		for _, w := range c.lacks {
			if strings.Contains(body, w) {
				t.Errorf("%s shows %q, which belongs on another tab", c.path, w)
			}
		}
		// Every tab carries the tab bar with all three links.
		for _, href := range []string{`href="/admin/reconcile"`, `href="/admin/reconcile/upload"`, `href="/admin/reconcile/setup"`} {
			if !strings.Contains(body, href) {
				t.Errorf("%s: tab bar lacks %s", c.path, href)
			}
		}
	}
	// The upload and setup tabs are guarded like the rest.
	h.setReconcile(1, false)
	for _, p := range []string{"/admin/reconcile/upload", "/admin/reconcile/setup"} {
		if r, _ := h.get(p); r.StatusCode != http.StatusForbidden {
			t.Errorf("%s without permission: %d", p, r.StatusCode)
		}
	}
}

// The Reconcile tab counts what is still unaddressed, lines and payments.
func TestUnaddressedCounts(t *testing.T) {
	h := scenario(t)
	_, body := h.get("/admin/reconcile")
	// 8 kept lines, all open (2 of them dividends under the minimum). Payments
	// within Sep 1-30: $150, $50, $1,190, $20, $22; the reversed $30 is not
	// one that stands.
	for _, want := range []string{
		"<strong>8</strong> bank lines not yet addressed", "(2 below the $1.00 minimum",
		"<strong>5</strong> recorded payments with no bank transaction yet",
		// $150 and $1,190 are suggested; the $50 is in the tie.
		"(3 have a suggested match or a possible pair above)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("summary lacks %q", want)
		}
	}
	if m := tabCountRe.FindStringSubmatch(body); m == nil || m[1] != "13" {
		t.Errorf("tab count = %v, want 13", m)
	}
	// Confirming the exact $150 addresses one line and one payment.
	h.confirmPair(t, lineByRow(t, h, 2).ID, paymentOn(t, h, "Phone plan", 15000).Seq, false)
	_, body = h.get("/admin/reconcile")
	if !strings.Contains(body, "<strong>7</strong> bank lines") || !strings.Contains(body, "<strong>4</strong> recorded payments") {
		t.Error("counts did not fall after a confirm")
	}
	// Ignoring a line addresses it; the count is on every tab.
	h.lineAction(t, lineByRow(t, h, 9).ID, "ignore", nil)
	_, body = h.get("/admin/reconcile/setup")
	if !regexp.MustCompile(`Reconcile\s*<span class="tabcount">10</span>`).MatchString(body) {
		t.Error("the Setup tab does not carry the updated count")
	}
}

func TestPaymentNotInBankOnTheWeb(t *testing.T) {
	h := scenario(t)
	logs := h.captureLog()
	garden := paymentOn(t, h, "Garden", 2000)
	_, body := h.post("/admin/reconcile/payments/"+itoa(garden.Seq)+"/not-in-bank", url.Values{
		"csrf_token": {h.csrfToken("/admin/reconcile")}, "note": {"<i>cash</i> at the market"},
	})
	if !strings.Contains(body, "Set aside: $20.00 on Garden is not expected in the bank.") {
		t.Fatalf("set aside: %s", truncate(body))
	}
	if !strings.Contains(body, "<strong>4</strong> recorded payments") {
		t.Error("the payment count did not fall")
	}
	set := section(t, body, "<h2>Set aside: not in the bank</h2>")
	if !strings.Contains(set, "&lt;i&gt;cash&lt;/i&gt; at the market") || !strings.Contains(set, "Jane Provider") ||
		strings.Contains(set, "<i>cash</i>") {
		t.Errorf("set-aside list: %s", set)
	}
	if !strings.Contains(logs.String(), "payment set aside as not in the bank") {
		t.Error("not logged")
	}
	// The tab shows it, set apart from a bank match.
	_, tb := h.get(tabPath(garden.TabID))
	if !strings.Contains(tb, `class="bankmark aside"`) || !strings.Contains(tb, "not in the bank (set aside)") {
		t.Error("the tab does not mark the set-aside payment")
	}
	// Twice, and a matched payment, are refused.
	if _, body := h.post("/admin/reconcile/payments/"+itoa(garden.Seq)+"/not-in-bank", url.Values{
		"csrf_token": {h.csrfToken("/admin/reconcile")},
	}); !strings.Contains(body, "already matched or set aside") {
		t.Errorf("twice: %s", truncate(body))
	}
	// Undo.
	rs, _ := h.db.ListPaymentReviews(t.Context(), 0, 5)
	_, body = h.post("/admin/reconcile/reviews/"+itoa(rs[0].ID)+"/undo", url.Values{"csrf_token": {h.csrfToken("/admin/reconcile")}})
	if !strings.Contains(body, "back among those to account for") || !strings.Contains(body, "<strong>5</strong> recorded payments") {
		t.Errorf("undo: %s", truncate(body))
	}
	// Refusals: no token, a long note, an unknown entry, a charge.
	for name, f := range map[string]url.Values{
		"no token":  {"note": {"x"}},
		"long note": {"csrf_token": {h.csrfToken("/admin/reconcile")}, "note": {strings.Repeat("x", 501)}},
	} {
		if _, body := h.post("/admin/reconcile/payments/"+itoa(garden.Seq)+"/not-in-bank", f); strings.Contains(body, "is not expected in the bank") {
			t.Errorf("%s: accepted", name)
		}
		if rs, _ := h.db.ListPaymentReviews(t.Context(), 0, 5); len(rs) != 1 || rs[0].Active() {
			t.Fatalf("%s: a review was stored", name)
		}
	}
	if _, body := h.post("/admin/reconcile/payments/99999/not-in-bank", url.Values{"csrf_token": {h.csrfToken("/admin/reconcile")}}); !strings.Contains(body, "not a payment") {
		t.Errorf("unknown entry: %s", truncate(body))
	}
	// Guarded.
	h.setReconcile(1, false)
	if r, _ := h.post("/admin/reconcile/payments/"+itoa(garden.Seq)+"/not-in-bank", url.Values{"csrf_token": {h.csrfToken("/")}}); r.StatusCode != http.StatusForbidden {
		t.Errorf("without permission: %d", r.StatusCode)
	}
}

func TestRenameLayout(t *testing.T) {
	h := scenario(t)
	logs := h.captureLog()
	fs, _ := h.db.ListBankFormats(t.Context())
	path := "/admin/reconcile/formats/" + itoa(fs[0].ID) + "/rename"
	_, body := h.post(path, url.Values{"csrf_token": {h.csrfToken("/admin/reconcile/setup")}, "name": {"  Credit union, checking  "}})
	if !strings.Contains(body, "Layout renamed.") || !strings.Contains(body, `value="Credit union, checking"`) {
		t.Fatalf("rename: %s", truncate(body))
	}
	got, _ := h.db.GetBankFormat(t.Context(), fs[0].ID)
	if got.Name != "Credit union, checking" || got.Mapping != fs[0].Mapping {
		t.Errorf("after rename = %+v", got)
	}
	// The same name again is fine (a no-op save on MariaDB too).
	if _, body := h.post(path, url.Values{"csrf_token": {h.csrfToken("/admin/reconcile/setup")}, "name": {"Credit union, checking"}}); !strings.Contains(body, "Layout renamed.") {
		t.Error("renaming to the same name failed")
	}
	for name, v := range map[string]string{"empty": "  ", "too long": strings.Repeat("x", 121)} {
		if _, body := h.post(path, url.Values{"csrf_token": {h.csrfToken("/admin/reconcile/setup")}, "name": {v}}); !strings.Contains(body, "Give the layout a name") {
			t.Errorf("%s accepted", name)
		}
	}
	h.post(path, url.Values{"name": {"No token"}})
	if got, _ := h.db.GetBankFormat(t.Context(), fs[0].ID); got.Name != "Credit union, checking" {
		t.Error("renamed without a CSRF token")
	}
	if r, _ := h.post("/admin/reconcile/formats/99999/rename", url.Values{"csrf_token": {h.csrfToken("/admin/reconcile/setup")}, "name": {"x"}}); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown layout: %d", r.StatusCode)
	}
	if !strings.Contains(logs.String(), "bank layout renamed") {
		t.Error("rename not logged")
	}
	// The import list and the next upload use the new name.
	_, body = h.get("/admin/reconcile/upload")
	if !strings.Contains(body, "Credit union, checking") {
		t.Error("the import list shows the old name")
	}
}

// "Show only unreconciled payments" on the tab history: holders only.
func TestUnreconciledFilter(t *testing.T) {
	h := scenario(t)
	phone := tabByName(t, h, "Phone plan")
	h.confirmPair(t, lineByRow(t, h, 2).ID, paymentOn(t, h, "Phone plan", 15000).Seq, false)

	_, body := h.get(tabPath(phone.ID))
	if !strings.Contains(body, "Show only unreconciled payments") {
		t.Fatal("no filter for the holder")
	}
	_, body = h.get(tabPath(phone.ID) + "?unreconciled=1")
	if !strings.Contains(body, "Showing only unreconciled payments.") || !strings.Contains(body, "Show all entries") {
		t.Error("filtered view does not say so")
	}
	hist := body[strings.Index(body, "<h2>History</h2>"):]
	hist = hist[:strings.Index(hist, "</section>")]
	// Left: the $50 payment. Gone: the matched $150, and the reversed $30 and
	// its reversal.
	if !strings.Contains(hist, "$50.00") {
		t.Error("the unreconciled $50 is missing")
	}
	for _, gone := range []string{"$150.00", "$30.00", "matched to bank"} {
		if strings.Contains(hist, gone) {
			t.Errorf("filtered history shows %q", gone)
		}
	}
	// All reconciled: says so rather than "Nothing posted yet".
	h.confirmPair(t, lineByRow(t, h, 6).ID, paymentOn(t, h, "Phone plan", 5000).Seq, false)
	if _, body := h.get(tabPath(phone.ID) + "?unreconciled=1"); !strings.Contains(body, "Every payment on this tab is reconciled.") {
		t.Error("an all-reconciled filter does not say so")
	}

	// A payee gets no filter, and the parameter changes nothing for them.
	h.loginAs("sam@example.com", "a-long-enough-password")
	_, all := h.get(tabPath(phone.ID))
	_, filtered := h.get(tabPath(phone.ID) + "?unreconciled=1")
	if strings.Contains(all, "Show only unreconciled payments") {
		t.Error("the payee is offered the filter")
	}
	if strings.Count(all, "<tr") != strings.Count(filtered, "<tr") {
		t.Error("the parameter filtered a payee's history")
	}
	_ = money.Cents(0)
	_ = store.KindPayment
}
