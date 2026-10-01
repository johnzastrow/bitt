package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// RECON-04 on the web: Confirm, Confirm all exact, Undo, and what they post.

// lineByRow finds an imported line by its row number in the file.
func lineByRow(t *testing.T, h *harness, row int) store.OpenBankLine {
	t.Helper()
	open, err := h.db.ListOpenBankLines(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range open {
		if l.Row == row {
			return l
		}
	}
	t.Fatalf("no open line at row %d", row)
	return store.OpenBankLine{}
}

// paymentOn finds the unreversed payment of an amount on a tab by name.
func paymentOn(t *testing.T, h *harness, tabName string, cents money.Cents) store.PaymentCandidate {
	t.Helper()
	ps, err := h.db.ListPaymentCandidates(t.Context(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.TabName == tabName && p.Amount == cents {
			return p
		}
	}
	t.Fatalf("no candidate %s on %s", cents, tabName)
	return store.PaymentCandidate{}
}

func (h *harness) confirmPair(t *testing.T, lineID, seq int64, note bool) (*http.Response, string) {
	t.Helper()
	f := url.Values{
		"csrf_token": {h.csrfToken("/admin/reconcile")},
		"line_id":    {itoa(lineID)},
		"entry_seq":  {itoa(seq)},
	}
	if note {
		f.Set("include_note", "1")
	}
	return h.post("/admin/reconcile/confirm", f)
}

func tabBalance(t *testing.T, h *harness, tabID int64) money.Cents {
	t.Helper()
	b, err := h.db.SumEntries(t.Context(), tabID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestConfirmOnTheWeb(t *testing.T) {
	h := scenario(t)
	logs := h.captureLog()

	// Exact: $150 on Phone plan, nothing posted.
	phone := paymentOn(t, h, "Phone plan", 15000)
	before := tabBalance(t, h, phone.TabID)
	_, body := h.confirmPair(t, lineByRow(t, h, 2).ID, phone.Seq, false)
	if !strings.Contains(body, "Matched to Phone plan. Amounts agree; nothing posted.") {
		t.Fatalf("exact confirm: %s", truncate(body))
	}
	if tabBalance(t, h, phone.TabID) != before {
		t.Error("an exact match moved the balance")
	}

	// $1,200 in the bank against $1,190 recorded: a $10 payment.
	ins := paymentOn(t, h, "Insurance", 119000)
	before = tabBalance(t, h, ins.TabID)
	_, body = h.confirmPair(t, lineByRow(t, h, 4).ID, ins.Seq, false)
	if !strings.Contains(body, "Matched to Insurance. Posted a payment of $10.00.") {
		t.Fatalf("delta confirm: %s", truncate(body))
	}
	if got := tabBalance(t, h, ins.TabID) - before; got != 1000 {
		t.Errorf("balance moved %s, want $10.00", got)
	}

	// Both appear under Recent matches; neither is suggested any more.
	_, body = h.get("/admin/reconcile")
	recent := section(t, body, "<h2>Recent matches</h2>")
	for _, want := range []string{"Phone plan", "Insurance", "Nothing posted.", "Posted a payment of $10.00.",
		"Row 4 of TransactionHistory.csv", "confirmed by Jane Provider", "Undo this match"} {
		if !strings.Contains(recent, want) {
			t.Errorf("recent matches lack %q", want)
		}
	}
	// Only the $50 tie is left among the suggestions: no single-pair cards.
	sugg := section(t, body, "<h2>Suggested matches</h2>")
	if strings.Contains(sugg, `class="matchrow"`) || strings.Contains(sugg, "Insurance") {
		t.Error("confirmed pairs are still suggested")
	}
	// The tab shows the delta as an ordinary entry with its memo.
	_, tabBody := h.get(tabPath(ins.TabID))
	if !strings.Contains(tabBody, "Bank reconciliation: Deposit Transfer FROM ALEX OTHER (line 4 of TransactionHistory.csv)") {
		t.Error("the delta's memo is not on the tab")
	}

	// Confirming the same pair again posts nothing.
	before = tabBalance(t, h, ins.TabID)
	if _, body := h.confirmPair(t, lineByRow4(t, h), ins.Seq, false); !strings.Contains(body, "already confirmed") &&
		!strings.Contains(body, "already matched") {
		t.Errorf("second confirm: %s", truncate(body))
	}
	if tabBalance(t, h, ins.TabID) != before {
		t.Error("a second confirm moved the balance")
	}

	for _, want := range []string{"bank match confirmed", "by_user_id=1", "tab_id="} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q", want)
		}
	}
}

// lineByRow4 finds row 4 in any state (it is matched by now).
func lineByRow4(t *testing.T, h *harness) int64 {
	t.Helper()
	ms, _ := h.db.ListBankMatches(t.Context(), store.BankMatchFilter{})
	for _, m := range ms {
		if m.Row == 4 {
			return m.LineID
		}
	}
	t.Fatal("row 4 not matched")
	return 0
}

// A tie is resolved by confirming one pair; the others stay open.
func TestConfirmOneOfATie(t *testing.T) {
	h := scenario(t)
	_, body := h.get("/admin/reconcile")
	if strings.Count(body, "Confirm this pair") != 3 {
		t.Fatalf("%d pair buttons in the tie, want 3", strings.Count(body, "Confirm this pair"))
	}
	fifty := paymentOn(t, h, "Phone plan", 5000)
	_, body = h.confirmPair(t, lineByRow(t, h, 7).ID, fifty.Seq, false)
	if !strings.Contains(body, "Matched to Phone plan") {
		t.Fatalf("tie confirm: %s", truncate(body))
	}
	_, body = h.get("/admin/reconcile")
	if strings.Contains(body, "possible bank lines") {
		t.Error("the tie is still shown after it was resolved")
	}
	// Rows 6 and 8 are open again, now without a candidate.
	unmatched := section(t, body, "<h2>Bank lines with no matching payment</h2>")
	if !strings.Contains(unmatched, "Row 6 of") || !strings.Contains(unmatched, "Row 8 of") {
		t.Error("the other tied lines are not back among the unmatched")
	}
}

// A hand-picked pair outside the window or tolerance is refused, posting
// nothing; so is a pair whose line or payment is taken.
func TestConfirmRefusalsOnTheWeb(t *testing.T) {
	h := scenario(t)
	garden := paymentOn(t, h, "Garden", 2000)
	before := tabBalance(t, h, garden.TabID)
	_, body := h.confirmPair(t, lineByRow(t, h, 9).ID, garden.Seq, false) // $75.25 vs $20
	if !strings.Contains(body, "outside the date window or amount tolerance") {
		t.Errorf("out of tolerance: %s", truncate(body))
	}
	if tabBalance(t, h, garden.TabID) != before {
		t.Error("a refused pair posted")
	}

	fifty := paymentOn(t, h, "Phone plan", 5000)
	h.confirmPair(t, lineByRow(t, h, 6).ID, fifty.Seq, false)
	if _, body := h.confirmPair(t, lineByRow(t, h, 7).ID, fifty.Seq, false); !strings.Contains(body, "already matched to another bank line") {
		t.Errorf("payment taken: %s", truncate(body))
	}
	for name, f := range map[string]url.Values{
		"no csrf":    {"line_id": {"1"}, "entry_seq": {"1"}},
		"bad ids":    {"csrf_token": {h.csrfToken("/admin/reconcile")}, "line_id": {"x"}, "entry_seq": {"1"}},
		"no such":    {"csrf_token": {h.csrfToken("/admin/reconcile")}, "line_id": {"99999"}, "entry_seq": {"1"}},
		"not a paym": {"csrf_token": {h.csrfToken("/admin/reconcile")}, "line_id": {itoa(lineByRow(t, h, 8).ID)}, "entry_seq": {"99999"}},
	} {
		if r, body := h.post("/admin/reconcile/confirm", f); r.StatusCode != http.StatusOK || strings.Contains(body, "Matched to") {
			t.Errorf("%s: %d %s", name, r.StatusCode, truncate(body))
		}
	}
	if ms, _ := h.db.ListBankMatches(t.Context(), store.BankMatchFilter{}); len(ms) != 1 {
		t.Errorf("%d matches, want only the first", len(ms))
	}
}

func TestConfirmAllExact(t *testing.T) {
	h := scenario(t)
	_, body := h.get("/admin/reconcile")
	if !strings.Contains(body, "Confirm all 1 exact matches") {
		t.Fatalf("no confirm-all button: %s", section(t, body, "<h2>Suggested matches</h2>"))
	}
	_, body = h.post("/admin/reconcile/confirm-exact", url.Values{"csrf_token": {h.csrfToken("/admin/reconcile")}})
	if !strings.Contains(body, "Confirmed 1 exact matches.") {
		t.Fatalf("confirm all: %s", truncate(body))
	}
	ms, _ := h.db.ListBankMatches(t.Context(), store.BankMatchFilter{})
	if len(ms) != 1 || ms[0].TabName != "Phone plan" || ms[0].DeltaEntrySeq != nil {
		t.Errorf("matches = %+v", ms)
	}
	// The non-exact Insurance suggestion was left for a person.
	_, body = h.get("/admin/reconcile")
	if !strings.Contains(section(t, body, "<h2>Suggested matches</h2>"), "Insurance") || strings.Contains(body, "Confirm all") {
		t.Error("confirm-all touched a non-exact suggestion, or the button remained")
	}
}

func TestUndoOnTheWeb(t *testing.T) {
	h := scenario(t)
	ins := paymentOn(t, h, "Insurance", 119000)
	before := tabBalance(t, h, ins.TabID)
	h.confirmPair(t, lineByRow(t, h, 4).ID, ins.Seq, false)
	ms, _ := h.db.ListBankMatches(t.Context(), store.BankMatchFilter{})

	_, body := h.post("/admin/reconcile/matches/"+itoa(ms[0].ID)+"/undo", url.Values{
		"csrf_token": {h.csrfToken("/admin/reconcile")},
	})
	if !strings.Contains(body, "Match undone.") {
		t.Fatalf("undo: %s", truncate(body))
	}
	if tabBalance(t, h, ins.TabID) != before {
		t.Errorf("undo left the balance at %s, want %s", tabBalance(t, h, ins.TabID), before)
	}
	// The unmade match is still listed, with who, when and why; the pair is
	// suggested again.
	_, body = h.get("/admin/reconcile")
	recent := section(t, body, "<h2>Recent matches</h2>")
	if !strings.Contains(recent, ">unmade<") || strings.Contains(recent, "Undo this match") ||
		!strings.Contains(recent, "by Jane Provider: undone from Reconciliation.") {
		t.Errorf("the unmade match is not shown with who and why: %s", recent)
	}
	if !strings.Contains(section(t, body, "<h2>Suggested matches</h2>"), "Insurance") {
		t.Error("the pair is not suggested again after undo")
	}
	// Undo twice, and an unknown match.
	if _, body := h.post("/admin/reconcile/matches/"+itoa(ms[0].ID)+"/undo", url.Values{
		"csrf_token": {h.csrfToken("/admin/reconcile")},
	}); !strings.Contains(body, "already undone") {
		t.Errorf("undo twice: %s", truncate(body))
	}
	if r, _ := h.post("/admin/reconcile/matches/99999/undo", url.Values{
		"csrf_token": {h.csrfToken("/admin/reconcile")},
	}); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown match: %d", r.StatusCode)
	}
}

// Exit criterion: a date flag appears when the dates differ by more than the
// setting, or straddle one of the tab's due dates; no entry's date changes.
func TestDateFlags(t *testing.T) {
	h := reconcileReady(t)
	ctx := t.Context()
	sam := h.addUser("sam@example.com", "Sam Payee", false)
	tab, err := h.db.CreateTab(ctx, store.Tab{Name: "Rent", Kind: store.TabServices, CreatedBy: sam.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Due on the 5th of each month.
	if err := h.db.SetSchedule(ctx, tab.ID, schedule.Schedule{Kind: schedule.MonthlyDay,
		Anchor: schedule.NewDate(2026, time.January, 5), Billing: schedule.InAdvance, Interval: 1}); err != nil {
		t.Fatal(err)
	}
	post := func(key string, cents money.Cents, day int) store.Entry {
		e, _, err := h.db.PostEntry(ctx, store.NewEntry{TabID: tab.ID, Kind: store.KindPayment, Amount: cents,
			EffectiveAt: time.Date(2026, 9, day, 16, 0, 0, 0, time.UTC), ActorUserID: sam.ID,
			IdempotencyKey: key, Method: store.MethodTransfer})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	onTime := post("a", 10000, 5) // recorded on the due date
	early := post("b", 20000, 10) // recorded 9/10
	plain := post("c", 30000, 14) // recorded 9/14
	csv := "Date,Description,Amount\n9/6/2026,Rent A,100.00\n9/15/2026,Rent B,200.00\n9/15/2026,Rent C,300.00\n"
	resp, body := h.upload(t, "rent.csv", []byte(csv))
	po := previewOfRe.FindStringSubmatch(body)
	h.post(resp.Request.URL.Path, url.Values{
		"csrf_token": {h.csrfToken(resp.Request.URL.Path)}, "preview_of": {po[1]},
		"date": {"0"}, "date_layout": {"m/d/yyyy"}, "amount": {"2"}, "debit": {"-1"}, "credit": {"-1"},
		"incoming": {"positive"}, "description": {"1"}, "account": {"-1"}, "note": {"-1"},
		"reference": {"-1"}, "name": {"Rent bank"}, "action": {"save"},
	})

	// 9/5 recorded, 9/6 in the bank: one day apart, but across the due date.
	_, body = h.confirmPair(t, lineByRow(t, h, 2).ID, onTime.Seq, false)
	if !strings.Contains(body, "Flagged: the dates fall on either side of the due date") {
		t.Errorf("straddle not flagged: %s", truncate(body))
	}
	// 9/10 recorded, 9/15 in the bank: five days, over the 3-day setting.
	_, body = h.confirmPair(t, lineByRow(t, h, 3).ID, early.Seq, false)
	if !strings.Contains(body, "Flagged: bank and recorded dates are 5 days apart") {
		t.Errorf("distance not flagged: %s", truncate(body))
	}
	// 9/14 recorded, 9/15 in the bank: neither.
	_, body = h.confirmPair(t, lineByRow(t, h, 4).ID, plain.Seq, false)
	if strings.Contains(body, "Flagged") {
		t.Errorf("an ordinary pair was flagged: %s", truncate(body))
	}
	// No entry's date changed.
	for _, e := range []store.Entry{onTime, early, plain} {
		got, _ := h.db.GetEntry(ctx, e.Seq)
		if !got.EffectiveAt.Equal(e.EffectiveAt) {
			t.Errorf("entry %d date changed", e.Seq)
		}
	}
	ms, _ := h.db.ListBankMatches(ctx, store.BankMatchFilter{TabID: tab.ID})
	flagged := 0
	for _, m := range ms {
		if m.DateFlagged {
			flagged++
		}
	}
	if flagged != 2 {
		t.Errorf("%d flagged matches stored, want 2", flagged)
	}
}

// The bank note reaches the memo only when the box is ticked; the box is
// offered only when there is a note and an entry to put it in.
func TestBankNoteCheckbox(t *testing.T) {
	h := reconcileReady(t)
	ctx := t.Context()
	sam := h.addUser("sam@example.com", "Sam Payee", false)
	tab, _ := h.db.CreateTab(ctx, store.Tab{Name: "Phone", Kind: store.TabServices, CreatedBy: sam.ID}, nil)
	pay := func(key string, cents money.Cents) store.Entry {
		e, _, err := h.db.PostEntry(ctx, store.NewEntry{TabID: tab.ID, Kind: store.KindPayment, Amount: cents,
			EffectiveAt: time.Date(2026, 9, 1, 16, 0, 0, 0, time.UTC), ActorUserID: sam.ID,
			IdempotencyKey: key, Method: store.MethodTransfer})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	// Amounts chosen so each line has exactly one candidate.
	p1, p2, p3 := pay("a", 5000), pay("b", 6000), pay("c", 7000)
	csv := "Date,Description,Note,Amount\n" +
		"9/1/2026,Transfer A,\"private: split with Ann\nline two\",52.00\n" +
		"9/1/2026,Transfer B,\"keep this to myself\",63.00\n" +
		"9/1/2026,Transfer C,\"equal amounts\",70.00\n"
	resp, body := h.upload(t, "notes.csv", []byte(csv))
	po := previewOfRe.FindStringSubmatch(body)
	h.post(resp.Request.URL.Path, url.Values{
		"csrf_token": {h.csrfToken(resp.Request.URL.Path)}, "preview_of": {po[1]},
		"date": {"0"}, "date_layout": {"m/d/yyyy"}, "amount": {"3"}, "debit": {"-1"}, "credit": {"-1"},
		"incoming": {"positive"}, "description": {"1"}, "account": {"-1"}, "note": {"2"},
		"reference": {"-1"}, "name": {"Notes bank"}, "action": {"save"},
	})
	_, body = h.get("/admin/reconcile")
	// Two differing amounts with notes get the box; the equal one does not.
	if n := strings.Count(body, `name="include_note"`); n != 2 {
		t.Errorf("%d note checkboxes, want 2", n)
	}
	if strings.Contains(body, `name="include_note" value="1" checked`) {
		t.Error("the note checkbox is ticked by default")
	}

	h.confirmPair(t, lineByRow(t, h, 2).ID, p1.Seq, true)
	h.confirmPair(t, lineByRow(t, h, 3).ID, p2.Seq, false)
	h.confirmPair(t, lineByRow(t, h, 4).ID, p3.Seq, true) // equal: nothing posted
	ms, _ := h.db.ListBankMatches(ctx, store.BankMatchFilter{TabID: tab.ID})
	memos := map[int]string{}
	for _, m := range ms {
		if m.DeltaEntrySeq != nil {
			e, _ := h.db.GetEntry(ctx, *m.DeltaEntrySeq)
			memos[m.Row] = e.Memo
		} else if m.NoteInMemo {
			t.Error("an equal-amount match claims its note went into a memo")
		}
	}
	if !strings.HasSuffix(memos[2], "\nBank note: private: split with Ann\nline two") {
		t.Errorf("ticked memo = %q", memos[2])
	}
	if strings.Contains(memos[3], "keep this to myself") || strings.Contains(memos[3], "Bank note") {
		t.Errorf("unticked memo carries the note: %q", memos[3])
	}
}

// The AUTH-05 exception: an administrator with the permission, not on the tab,
// posts the delta; it is attributed to them. The ordinary form still refuses.
func TestAdminNotOnTabConfirms(t *testing.T) {
	h := reconcileReady(t)
	ctx := t.Context()
	sam := h.addUser("sam@example.com", "Sam Payee", false)
	tab, _ := h.db.CreateTab(ctx, store.Tab{Name: "Sam only", Kind: store.TabServices, CreatedBy: sam.ID}, nil)
	if role, err := h.db.ParticipantRole(ctx, tab.ID, 1); err == nil {
		t.Fatalf("precondition: Jane is on the tab as %s", role)
	}
	p, _, _ := h.db.PostEntry(ctx, store.NewEntry{TabID: tab.ID, Kind: store.KindPayment, Amount: 5000,
		EffectiveAt: time.Date(2026, 9, 1, 16, 0, 0, 0, time.UTC), ActorUserID: sam.ID,
		IdempotencyKey: "s", Method: store.MethodTransfer})
	csv := "Date,Description,Amount\n9/1/2026,From Sam,52.00\n"
	resp, body := h.upload(t, "s.csv", []byte(csv))
	po := previewOfRe.FindStringSubmatch(body)
	h.post(resp.Request.URL.Path, url.Values{
		"csrf_token": {h.csrfToken(resp.Request.URL.Path)}, "preview_of": {po[1]},
		"date": {"0"}, "date_layout": {"m/d/yyyy"}, "amount": {"2"}, "debit": {"-1"}, "credit": {"-1"},
		"incoming": {"positive"}, "description": {"1"}, "account": {"-1"}, "note": {"-1"},
		"reference": {"-1"}, "name": {"S"}, "action": {"save"},
	})
	if _, body := h.confirmPair(t, lineByRow(t, h, 2).ID, p.Seq, false); !strings.Contains(body, "Posted a payment of $2.00") {
		t.Fatalf("confirm: %s", truncate(body))
	}
	ms, _ := h.db.ListBankMatches(ctx, store.BankMatchFilter{TabID: tab.ID})
	delta, _ := h.db.GetEntry(ctx, *ms[0].DeltaEntrySeq)
	if delta.ActorUserID != 1 {
		t.Errorf("delta attributed to %d, want the administrator", delta.ActorUserID)
	}
	// The ordinary payment form on that tab still refuses her.
	before := tabBalance(t, h, tab.ID)
	h.post(tabPath(tab.ID)+"/payments", url.Values{
		"csrf_token": {h.csrfToken(tabPath(tab.ID))}, "amount": {"10.00"}, "method": {"transfer"},
	})
	if tabBalance(t, h, tab.ID) != before {
		t.Error("the ordinary payment form accepted an administrator not on the tab")
	}
}

func TestConfirmRoutesAreGuarded(t *testing.T) {
	h := scenario(t)
	h.setReconcile(1, false)
	for _, p := range []string{"/admin/reconcile/confirm", "/admin/reconcile/confirm-exact", "/admin/reconcile/matches/1/undo"} {
		if r, _ := h.post(p, url.Values{"csrf_token": {h.csrfToken("/")}}); r.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s without permission: %d", p, r.StatusCode)
		}
	}
	h.loginAs("sam@example.com", "a-long-enough-password")
	for _, p := range []string{"/admin/reconcile/confirm", "/admin/reconcile/confirm-exact", "/admin/reconcile/matches/1/undo"} {
		if r, _ := h.post(p, url.Values{"csrf_token": {h.csrfToken("/")}}); r.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s as non-admin: %d", p, r.StatusCode)
		}
	}
	if ms, _ := h.db.ListBankMatches(t.Context(), store.BankMatchFilter{}); len(ms) != 0 {
		t.Error("a refused request matched something")
	}
}

// Undoing a matched payment on its tab unmakes the match (decided
// 2026-10-01): the payment and its difference are reversed together, the
// line reopens, and the match records who did it, when and why.
func TestTabUndoUnmakesOnTheWeb(t *testing.T) {
	h := scenario(t)
	ins := paymentOn(t, h, "Insurance", 119000)
	beforePay := tabBalance(t, h, ins.TabID) - ins.Amount
	h.confirmPair(t, lineByRow(t, h, 4).ID, ins.Seq, false)

	// Alex recorded the payment, so Alex may undo it on the tab.
	h.loginAs("alex@example.com", "a-long-enough-password")
	_, body := h.post(tabPath(ins.TabID)+"/entries/"+itoa(ins.Seq)+"/undo", url.Values{
		"csrf_token": {h.csrfToken(tabPath(ins.TabID))},
	})
	if !strings.Contains(body, "Undone.") {
		t.Fatalf("tab undo: %s", truncate(body))
	}
	if got := tabBalance(t, h, ins.TabID); got != beforePay {
		t.Errorf("balance %s, want %s: payment and difference both reversed", got, beforePay)
	}
	ms, _ := h.db.ListBankMatches(t.Context(), store.BankMatchFilter{TabID: ins.TabID})
	if len(ms) != 1 || ms[0].Active() || ms[0].UndoneByName != "Alex Other" ||
		ms[0].UndoReason != "the payment was undone on its tab" {
		t.Errorf("match = %+v", ms)
	}
	h.loginAs("jane@example.com", "correct-horse-battery")
	if lineByRow(t, h, 4).State != store.BankLineOpen {
		t.Error("the line did not reopen")
	}
}
