package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/store"
)

// RECON-05: recording an unmatched line as a payment, marking a line Not
// BitTabby, undoing either, and the line's history of who did what and when.

func (h *harness) lineAction(t *testing.T, lineID int64, action string, extra url.Values) (*http.Response, string) {
	t.Helper()
	f := url.Values{"csrf_token": {h.csrfToken(linePath(lineID))}}
	for k, v := range extra {
		f[k] = v
	}
	return h.post(linePath(lineID)+"/"+action, f)
}

func tabByName(t *testing.T, h *harness, name string) store.Tab {
	t.Helper()
	tabs, _ := h.db.ListAllTabs(t.Context())
	for _, tb := range tabs {
		if tb.Name == name {
			return tb
		}
	}
	t.Fatalf("no tab %q", name)
	return store.Tab{}
}

func TestRecordLineAsPayment(t *testing.T) {
	h := scenario(t)
	logs := h.captureLog()
	check := lineByRow(t, h, 9) // $75.25 check deposit, no payment
	phone := tabByName(t, h, "Phone plan")

	// The line page offers the record form, Phone plan not first (no name in
	// "Check Deposit"), and the Not BitTabby form.
	_, body := h.get(linePath(check.ID))
	for _, want := range []string{"Record as a payment", "Posts $75.25 on Sep 8, 2026", "Mark Not BitTabby",
		"Bank reconciliation: Check Deposit (line 9 of TransactionHistory.csv)", "Ref 1042"} {
		if !strings.Contains(body, want) {
			t.Errorf("line page lacks %q", want)
		}
	}

	before := tabBalance(t, h, phone.ID)
	_, body = h.lineAction(t, check.ID, "record", url.Values{"tab_id": {itoa(phone.ID)}, "method": {"transfer"},
		"memo": {"Sam's check for September"}})
	if !strings.Contains(body, "Recorded a payment of $75.25 on Phone plan.") {
		t.Fatalf("record: %s", truncate(body))
	}
	if got := tabBalance(t, h, phone.ID) - before; got != 7525 {
		t.Errorf("balance moved %s, want $75.25", got)
	}
	line, _ := h.db.GetBankLine(t.Context(), check.ID)
	if line.State != store.BankLineRecorded || line.RecordedEntrySeq == nil {
		t.Fatalf("line = %+v", line)
	}
	e, _ := h.db.GetEntry(t.Context(), *line.RecordedEntrySeq)
	ny, _ := time.LoadLocation("America/New_York")
	if e.Kind != store.KindPayment || e.Amount != 7525 || e.Method != store.MethodTransfer || e.ActorUserID != 1 ||
		e.Memo != "Sam's check for September" || !strings.HasPrefix(e.IdempotencyKey, "recon:line:") ||
		!e.EffectiveAt.Equal(time.Date(2026, 9, 8, 12, 0, 0, 0, ny)) {
		t.Errorf("recorded entry = %+v", e)
	}
	// Who and when are on the line's history; the page shows the recording.
	_, body = h.get(linePath(check.ID))
	for _, want := range []string{"Recorded as a payment", "Recorded as a payment by Jane Provider", "Undo the recording", "Phone plan"} {
		if !strings.Contains(body, want) {
			t.Errorf("recorded line page lacks %q", want)
		}
	}
	// It left the unmatched list, and the payment is not offered for matching.
	_, body = h.get("/admin/reconcile")
	if strings.Contains(section(t, body, "<h2>Bank lines with no matching payment</h2>"), "Check Deposit") {
		t.Error("a recorded line is still listed as unmatched")
	}
	ps, _ := h.db.ListPaymentCandidates(t.Context(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	for _, p := range ps {
		if p.Seq == e.Seq {
			t.Error("a payment recorded from a line is offered for matching")
		}
	}
	// Recording twice is refused.
	if _, body := h.lineAction(t, check.ID, "record", url.Values{"tab_id": {itoa(phone.ID)}, "method": {"transfer"}}); !strings.Contains(body, "no longer open") {
		t.Errorf("second record: %s", truncate(body))
	}
	if !strings.Contains(logs.String(), "bank line recorded") {
		t.Error("recording not logged")
	}

	// Undo the recording from the line page: reversed, open, in the history.
	before = tabBalance(t, h, phone.ID)
	if _, body := h.lineAction(t, check.ID, "unrecord", nil); !strings.Contains(body, "reversed and the line is open again") {
		t.Fatalf("unrecord: %s", truncate(body))
	}
	if got := tabBalance(t, h, phone.ID) - before; got != -7525 {
		t.Errorf("unrecord moved %s", got)
	}
	_, body = h.get(linePath(check.ID))
	if !strings.Contains(body, "Recording undone by Jane Provider") || !strings.Contains(body, "Record as a payment") {
		t.Error("the line page does not show the undo and the form again")
	}
}

// Undoing the recorded payment on its tab reopens the line too.
func TestTabUndoReopensRecordedLine(t *testing.T) {
	h := scenario(t)
	check := lineByRow(t, h, 9)
	phone := tabByName(t, h, "Phone plan")
	h.lineAction(t, check.ID, "record", url.Values{"tab_id": {itoa(phone.ID)}, "method": {"other"}})
	line, _ := h.db.GetBankLine(t.Context(), check.ID)
	_, body := h.post(tabPath(phone.ID)+"/entries/"+itoa(*line.RecordedEntrySeq)+"/undo", url.Values{
		"csrf_token": {h.csrfToken(tabPath(phone.ID))},
	})
	if !strings.Contains(body, "Undone.") {
		t.Fatalf("tab undo: %s", truncate(body))
	}
	line, _ = h.db.GetBankLine(t.Context(), check.ID)
	if line.State != store.BankLineOpen || line.RecordedEntrySeq != nil {
		t.Errorf("line after tab undo = %+v", line)
	}
	evs, _ := h.db.ListBankLineEvents(t.Context(), check.ID)
	if len(evs) != 2 || evs[1].Action != "unrecorded" || evs[1].Note != "undone on the tab" {
		t.Errorf("events = %+v", evs)
	}
}

func TestRecordLineNoteAndRefusals(t *testing.T) {
	h := scenario(t)
	// Row 6 has a bank note; it is $50 and the tie keeps it open.
	noted := lineByRow(t, h, 6)
	phone := tabByName(t, h, "Phone plan")
	_, body := h.get(linePath(noted.ID))
	if !strings.Contains(body, `name="include_note"`) {
		t.Fatal("no note checkbox for a line with a note")
	}
	if !strings.Contains(body, "Phone plan (Sam Payee, Jane Provider) -- likely") {
		t.Errorf("the likely tab is not marked: %s", truncate(body))
	}

	archived, _ := h.db.CreateTab(t.Context(), store.Tab{Name: "Old", Kind: store.TabServices, CreatedBy: 1}, nil)
	if err := h.db.SetTabArchived(t.Context(), archived.ID, true); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]url.Values{
		"archived tab": {"tab_id": {itoa(archived.ID)}, "method": {"transfer"}},
		"no tab":       {"tab_id": {"x"}, "method": {"transfer"}},
		"unknown tab":  {"tab_id": {"99999"}, "method": {"transfer"}},
		"cash":         {"tab_id": {itoa(phone.ID)}, "method": {"cash"}},
		"no method":    {"tab_id": {itoa(phone.ID)}},
		"long memo":    {"tab_id": {itoa(phone.ID)}, "method": {"transfer"}, "memo": {strings.Repeat("x", 501)}},
	} {
		if _, body := h.lineAction(t, noted.ID, "record", f); strings.Contains(body, "Recorded a payment") {
			t.Errorf("%s: accepted", name)
		}
	}
	// And with no CSRF token.
	h.post(linePath(noted.ID)+"/record", url.Values{"tab_id": {itoa(phone.ID)}, "method": {"transfer"}})
	if l, _ := h.db.GetBankLine(t.Context(), noted.ID); l.State != store.BankLineOpen {
		t.Fatal("a refused or token-less record changed the line")
	}

	// With the box ticked, the note is appended to the memo.
	h.lineAction(t, noted.ID, "record", url.Values{"tab_id": {itoa(phone.ID)}, "method": {"transfer"}, "include_note": {"1"}})
	l, _ := h.db.GetBankLine(t.Context(), noted.ID)
	e, _ := h.db.GetEntry(t.Context(), *l.RecordedEntrySeq)
	if !strings.HasSuffix(e.Memo, "\nBank note: Phone plan, September\nsplit with Ann") ||
		!strings.HasPrefix(e.Memo, "Bank reconciliation: Deposit Transfer FROM SAM PAYEE X1234 (line 6 of") {
		t.Errorf("memo = %q", e.Memo)
	}
	// Without it, not.
	other := lineByRow(t, h, 7)
	h.lineAction(t, other.ID, "record", url.Values{"tab_id": {itoa(phone.ID)}, "method": {"transfer"}})
	l, _ = h.db.GetBankLine(t.Context(), other.ID)
	e, _ = h.db.GetEntry(t.Context(), *l.RecordedEntrySeq)
	if strings.Contains(e.Memo, "Bank note") {
		t.Errorf("unticked memo carries a note: %q", e.Memo)
	}
}

func TestIgnoreAndUnignore(t *testing.T) {
	h := scenario(t)
	check := lineByRow(t, h, 9)
	_, body := h.lineAction(t, check.ID, "ignore", url.Values{"note": {"<b>refund</b> from the utility"}})
	if !strings.Contains(body, "Marked Not BitTabby") {
		t.Fatalf("ignore: %s", truncate(body))
	}
	if strings.Contains(section(t, body, "<h2>Bank lines with no matching payment</h2>"), "Check Deposit") {
		t.Error("an ignored line is still listed")
	}
	_, body = h.get(linePath(check.ID))
	for _, want := range []string{"Not BitTabby", "&lt;b&gt;refund&lt;/b&gt; from the utility", "Marked Not BitTabby by Jane Provider", "Un-ignore"} {
		if !strings.Contains(body, want) {
			t.Errorf("ignored line page lacks %q", want)
		}
	}
	if strings.Contains(body, "<b>refund</b>") {
		t.Error("the ignore note is rendered as markup")
	}
	// Ignoring twice, or ignoring a matched line, is refused.
	if _, body := h.lineAction(t, check.ID, "ignore", nil); !strings.Contains(body, "Only an open line") {
		t.Errorf("ignore twice: %s", truncate(body))
	}
	_, body = h.lineAction(t, check.ID, "unignore", nil)
	if !strings.Contains(body, "open again") {
		t.Fatalf("unignore: %s", truncate(body))
	}
	if l, _ := h.db.GetBankLine(t.Context(), check.ID); l.State != store.BankLineOpen || l.IgnoreReason != "" || l.IgnoreNote != "" {
		t.Errorf("after unignore = %+v", l)
	}
	if _, body := h.lineAction(t, check.ID, "unignore", nil); !strings.Contains(body, "not ignored") {
		t.Errorf("unignore an open line: %s", truncate(body))
	}
	evs, _ := h.db.ListBankLineEvents(t.Context(), check.ID)
	if len(evs) != 2 || evs[0].Action != "ignored" || evs[0].ByName != "Jane Provider" || evs[1].Action != "unignored" || evs[0].At.IsZero() {
		t.Errorf("events = %+v", evs)
	}

	// A line ignored by a rule can be un-ignored too.
	formats, _ := h.db.ListBankFormats(t.Context())
	h.post("/admin/reconcile/rules", url.Values{"csrf_token": {h.csrfToken("/admin/reconcile")},
		"format_id": {itoa(formats[0].ID)}, "contains": {"Deposit Dividend"}})
	imps, _ := h.db.ListBankImports(t.Context(), 1)
	ls, _ := h.db.ListBankLines(t.Context(), imps[0].ID)
	var div store.BankLine
	for _, l := range ls {
		if l.State == store.BankLineIgnored {
			div = l
			break
		}
	}
	if div.ID == 0 {
		t.Fatal("no rule-ignored line")
	}
	if _, body := h.lineAction(t, div.ID, "unignore", nil); !strings.Contains(body, "open again") {
		t.Errorf("unignore a rule-ignored line: %s", truncate(body))
	}
}

// Every line route is behind the guard.
func TestLineRoutesAreGuarded(t *testing.T) {
	h := scenario(t)
	id := lineByRow(t, h, 9).ID
	h.setReconcile(1, false)
	if r, _ := h.get(linePath(id)); r.StatusCode != http.StatusForbidden {
		t.Errorf("GET line without permission: %d", r.StatusCode)
	}
	for _, a := range []string{"record", "unrecord", "ignore", "unignore"} {
		if r, _ := h.post(linePath(id)+"/"+a, url.Values{"csrf_token": {h.csrfToken("/")}}); r.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s without permission: %d", a, r.StatusCode)
		}
	}
	h.setReconcile(1, true)
	if r, _ := h.get(linePath(99999)); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown line: %d", r.StatusCode)
	}
	if l, _ := h.db.GetBankLine(t.Context(), id); l.State != store.BankLineOpen {
		t.Error("a refused request changed the line")
	}
}

// Exit criterion: for any file, each row's outcome; the import page links each
// line to its page, which carries its history.
func TestImportPageShowsOutcomes(t *testing.T) {
	h := scenario(t)
	phone := tabByName(t, h, "Phone plan")
	h.confirmPair(t, lineByRow(t, h, 2).ID, paymentOn(t, h, "Phone plan", 15000).Seq, false)
	h.lineAction(t, lineByRow(t, h, 9).ID, "ignore", nil)
	h.lineAction(t, lineByRow(t, h, 6).ID, "record", url.Values{"tab_id": {itoa(phone.ID)}, "method": {"transfer"}})
	imps, _ := h.db.ListBankImports(t.Context(), 1)
	_, body := h.get("/admin/reconcile/imports/" + itoa(imps[0].ID))
	for _, want := range []string{">matched<", ">ignored<", ">recorded<", "Line details and history"} {
		if !strings.Contains(body, want) {
			t.Errorf("import page lacks %q", want)
		}
	}
}
