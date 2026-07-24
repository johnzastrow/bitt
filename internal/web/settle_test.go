package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/store"
)

// addUser creates an account directly and returns it.
func (h *harness) addUser(email, name string, admin bool) store.User {
	h.t.Helper()
	hash, err := auth.HashPassword("a-long-enough-password")
	if err != nil {
		h.t.Fatalf("hash: %v", err)
	}
	u, err := h.db.CreateUser(h.t.Context(), store.User{
		Email: email, DisplayName: name, PasswordHash: hash, IsAdmin: admin,
	})
	if err != nil {
		h.t.Fatalf("create user %s: %v", email, err)
	}
	return u
}

// loginAs switches the harness client to another account.
func (h *harness) loginAs(email, password string) {
	h.t.Helper()
	h.post("/logout", url.Values{"csrf_token": {h.csrfToken("/")}})
	_, body := h.post("/login", url.Values{
		"csrf_token": {h.csrfToken("/login")},
		"email":      {email},
		"password":   {password},
	})
	if strings.Contains(body, "not correct") {
		h.t.Fatalf("login as %s failed", email)
	}
}

// makeTab creates a tab as the signed-in user and returns its path.
func (h *harness) makeTab(name string, items ...string) string {
	h.t.Helper()
	form := url.Values{
		"csrf_token": {h.csrfToken("/tabs/new")},
		"name":       {name},
	}
	for i := 0; i+1 < len(items); i += 2 {
		form.Add("item_name", items[i])
		form.Add("item_amount", items[i+1])
	}
	h.post("/tabs", form)

	// Match by name rather than by position: ListTabsForUser returns newest
	// first, and indexing into it silently returned the wrong tab.
	tabs, err := h.db.ListTabsForUser(h.t.Context(), 1)
	if err != nil {
		h.t.Fatalf("list tabs: %v", err)
	}
	for _, t := range tabs {
		if t.Name == name {
			return tabPath(t.ID)
		}
	}
	h.t.Fatalf("tab %q was not created", name)
	return ""
}

// hiddenValue scrapes a hidden input from a rendered form.
func hiddenValue(body, name string) string {
	marker := `name="` + name + `" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// The headline Phase 2 exit criterion: two people, one tab, a settle in one tap
// plus one confirmation, with every field prefilled.
func TestSettleLoopEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.completeSetup() // Jane, admin and provider

	// Jane adds Sam through the admin screen (AUTH-04).
	_, body := h.post("/admin/users", url.Values{
		"csrf_token":   {h.csrfToken("/admin/users")},
		"display_name": {"Sam Payee"},
		"email":        {"sam@example.com"},
		"password":     {"sams-long-password"},
	})
	if !strings.Contains(body, "Sam Payee") {
		t.Fatalf("Sam was not created: %s", truncate(body))
	}

	// Jane creates a tab and charges it.
	tab := h.makeTab("Family phone plan", "Jane line", "45.00", "Sam line", "30.00")
	_, body = h.post(tab+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"amount":     {"75.00"},
		"memo":       {"October"},
	})
	if !strings.Contains(body, "$75.00 owed") {
		t.Fatalf("charge did not land: %s", truncate(body))
	}

	// Jane attaches Sam as Payee (TAB-03).
	sam, err := h.db.GetUserByEmail(t.Context(), "sam@example.com")
	if err != nil {
		t.Fatal(err)
	}
	_, body = h.post(tab+"/participants", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"user_id":    {itoa64(sam.ID)},
	})
	if !strings.Contains(body, "Sam Payee") {
		t.Errorf("Sam not shown on the tab after attaching: %s", truncate(body))
	}

	// The tab now appears on Sam's dashboard, with a settle control.
	h.loginAs("sam@example.com", "sams-long-password")
	_, body = h.get("/")
	if !strings.Contains(body, "Family phone plan") {
		t.Fatalf("tab did not appear on the payee's dashboard: %s", truncate(body))
	}
	if !strings.Contains(body, "Settle $75.00") {
		t.Errorf("dashboard settle control missing or not prefilled: %s", truncate(body))
	}
	if !strings.Contains(body, "you are the payee") {
		t.Errorf("card does not show the viewer's role")
	}

	// Tap one: the confirmation fragment, every field prefilled (UI-03).
	_, confirm := h.get(tab + "/settle")
	if !strings.Contains(confirm, "Record a payment of") {
		t.Fatalf("confirmation did not render: %s", truncate(confirm))
	}
	if got := hiddenValue(confirm, "amount"); got != "75.00" {
		t.Errorf("confirmation amount = %q, want 75.00 prefilled", got)
	}
	key := hiddenValue(confirm, "idempotency_key")
	if key == "" {
		t.Fatal("confirmation carries no idempotency key")
	}

	// Tap two: confirm. The card comes back settled.
	_, card := h.post(tab+"/settle", url.Values{
		"csrf_token":      {hiddenValue(confirm, "csrf_token")},
		"idempotency_key": {key},
		"amount":          {"75.00"},
		"method":          {"transfer"},
	})
	if !strings.Contains(card, "Settled up") {
		t.Errorf("card did not come back settled: %s", truncate(card))
	}
	if strings.Contains(card, "Settle $") {
		t.Error("settle control still offered on a settled tab")
	}

	// The ledger holds exactly one payment, attributed to Sam, with its method.
	entries, err := h.db.ListEntries(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	var payments int
	for _, e := range entries {
		if e.Kind == store.KindPayment {
			payments++
			if e.Amount != 7500 {
				t.Errorf("payment amount = %s, want 75.00", e.Amount)
			}
			if e.ActorUserID != sam.ID {
				t.Errorf("payment attributed to user %d, want Sam (%d)", e.ActorUserID, sam.ID)
			}
			if e.Method != store.MethodTransfer {
				t.Errorf("payment method = %q, want transfer", e.Method)
			}
		}
	}
	if payments != 1 {
		t.Errorf("%d payments recorded, want 1", payments)
	}

	if b, _ := h.db.SumEntries(t.Context(), 1); b != 0 {
		t.Errorf("balance = %s after settling, want 0.00", b)
	}
}

// A double-submitted settle posts exactly one entry (LEDGER-07).
func TestDoubleSettlePostsOnce(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	tab := h.makeTab("Phone plan")
	h.post(tab+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"amount":     {"45.00"},
	})

	_, confirm := h.get(tab + "/settle")
	form := url.Values{
		"csrf_token":      {hiddenValue(confirm, "csrf_token")},
		"idempotency_key": {hiddenValue(confirm, "idempotency_key")},
		"amount":          {"45.00"},
		"method":          {"cash"},
	}
	h.post(tab+"/settle", form)
	h.post(tab+"/settle", form) // the double tap

	entries, _ := h.db.ListEntries(t.Context(), 1)
	payments := 0
	for _, e := range entries {
		if e.Kind == store.KindPayment {
			payments++
		}
	}
	if payments != 1 {
		t.Errorf("%d payments from a double-submitted settle, want 1", payments)
	}
	if b, _ := h.db.SumEntries(t.Context(), 1); b != 0 {
		t.Errorf("balance = %s, want 0.00 -- the double tap paid twice", b)
	}
}

// PAY-03: a Provider may record on the Payee's behalf, attributed to the Provider.
func TestProviderRecordsOnPayeeBehalf(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	sam := h.addUser("sam@example.com", "Sam", false)

	tab := h.makeTab("Phone plan")
	h.post(tab+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tab)}, "amount": {"45.00"},
	})
	h.post(tab+"/participants", url.Values{
		"csrf_token": {h.csrfToken(tab)}, "user_id": {itoa64(sam.ID)},
	})

	// Jane (the provider) records the payment.
	_, body := h.post(tab+"/payments", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"amount":     {"45.00"},
		"method":     {"cash"},
		"memo":       {"Sam paid me in cash"},
	})
	if !strings.Contains(body, "Settled up") {
		t.Errorf("tab not settled: %s", truncate(body))
	}

	entries, _ := h.db.ListEntries(t.Context(), 1)
	for _, e := range entries {
		if e.Kind != store.KindPayment {
			continue
		}
		if e.ActorUserID != 1 {
			t.Errorf("payment attributed to user %d, want the provider (1)", e.ActorUserID)
		}
		if e.Method != store.MethodCash {
			t.Errorf("method = %q, want cash", e.Method)
		}
	}
}

// PAY-05: paying ahead creates a credit that offsets the next charge, because
// the balance is a sum -- no separate credit application step exists.
func TestPayingAheadCreatesCredit(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	tab := h.makeTab("Phone plan")

	// Pay $100 with nothing owed.
	_, body := h.post(tab+"/payments", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"amount":     {"100.00"},
		"method":     {"transfer"},
	})
	if !strings.Contains(body, "$100.00 in credit") {
		t.Fatalf("advance payment did not read as credit: %s", truncate(body))
	}

	// A $75 charge is absorbed by the credit, leaving $25.
	_, body = h.post(tab+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tab)}, "amount": {"75.00"},
	})
	if !strings.Contains(body, "$25.00 in credit") {
		t.Errorf("credit did not offset the charge: %s", truncate(body))
	}

	// A further $40 charge exhausts it and tips into owing $15.
	_, body = h.post(tab+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tab)}, "amount": {"40.00"},
	})
	if !strings.Contains(body, "$15.00 owed") {
		t.Errorf("balance after exhausting credit: %s", truncate(body))
	}

	if b, _ := h.db.SumEntries(t.Context(), 1); b != -1500 {
		t.Errorf("balance = %s, want -15.00", b)
	}
}

// PAY-04 / LEDGER-02: undo posts a reversing entry and leaves the original.
func TestUndoLeavesOriginalVisible(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	tab := h.makeTab("Phone plan")
	h.post(tab+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tab)}, "amount": {"45.00"}, "memo": {"Mistake"},
	})

	_, body := h.post(tab+"/entries/1/undo", url.Values{"csrf_token": {h.csrfToken(tab)}})
	if !strings.Contains(body, "Settled up") {
		t.Errorf("balance not restored after undo: %s", truncate(body))
	}
	// The original is still shown, and so is its reversal.
	if !strings.Contains(body, "Mistake") {
		t.Error("the original entry vanished from the history")
	}
	if !strings.Contains(body, "reversal") {
		t.Error("no reversal entry appears in the history")
	}

	entries, _ := h.db.ListEntries(t.Context(), 1)
	if len(entries) != 2 {
		t.Fatalf("%d entries after undo, want 2", len(entries))
	}

	// A second undo of the same entry is refused.
	_, body = h.post(tab+"/entries/1/undo", url.Values{"csrf_token": {h.csrfToken(tab)}})
	if !strings.Contains(body, "already undone") {
		t.Errorf("second undo not refused: %s", truncate(body))
	}
	entries, _ = h.db.ListEntries(t.Context(), 1)
	if len(entries) != 2 {
		t.Errorf("%d entries after a blocked second undo, want 2", len(entries))
	}
}

// A Payee may undo only what they recorded, so one participant cannot silently
// reverse the other's entries.
func TestPayeeCannotUndoProviderEntry(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	sam := h.addUser("sam@example.com", "Sam", false)

	tab := h.makeTab("Phone plan")
	h.post(tab+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tab)}, "amount": {"45.00"},
	})
	h.post(tab+"/participants", url.Values{
		"csrf_token": {h.csrfToken(tab)}, "user_id": {itoa64(sam.ID)},
	})

	h.loginAs("sam@example.com", "a-long-enough-password")
	_, body := h.post(tab+"/entries/1/undo", url.Values{"csrf_token": {h.csrfToken(tab)}})
	if !strings.Contains(body, "only undo entries you recorded") {
		t.Errorf("payee was allowed to undo the provider's charge: %s", truncate(body))
	}
	if b, _ := h.db.SumEntries(t.Context(), 1); b != -4500 {
		t.Errorf("balance = %s, want -45.00 unchanged", b)
	}
}

// An entry on another tab cannot be reversed by guessing its sequence number.
func TestUndoRejectsForeignEntry(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	tabA := h.makeTab("Tab A")
	h.post(tabA+"/charges", url.Values{"csrf_token": {h.csrfToken(tabA)}, "amount": {"10.00"}})
	tabB := h.makeTab("Tab B")
	h.post(tabB+"/charges", url.Values{"csrf_token": {h.csrfToken(tabB)}, "amount": {"20.00"}})

	// Entry 1 belongs to tab A; try to undo it through tab B.
	resp, _ := h.post(tabB+"/entries/1/undo", url.Values{"csrf_token": {h.csrfToken(tabB)}})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-tab undo returned %d, want 404", resp.StatusCode)
	}
	if b, _ := h.db.SumEntries(t.Context(), 1); b != -1000 {
		t.Errorf("tab A balance = %s, want -10.00 unchanged", b)
	}
}

// AUTH-05: a Payee cannot post charges, and cannot attach people.
func TestPayeeCannotBillOrAttach(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	sam := h.addUser("sam@example.com", "Sam", false)
	other := h.addUser("kim@example.com", "Kim", false)

	tab := h.makeTab("Phone plan")
	h.post(tab+"/participants", url.Values{
		"csrf_token": {h.csrfToken(tab)}, "user_id": {itoa64(sam.ID)},
	})

	h.loginAs("sam@example.com", "a-long-enough-password")

	_, body := h.post(tab+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tab)}, "amount": {"999.00"},
	})
	if !strings.Contains(body, "Only the provider can post a charge") {
		t.Errorf("payee was allowed to post a charge: %s", truncate(body))
	}

	_, body = h.post(tab+"/participants", url.Values{
		"csrf_token": {h.csrfToken(tab)}, "user_id": {itoa64(other.ID)},
	})
	if !strings.Contains(body, "Only the provider can attach") {
		t.Errorf("payee was allowed to attach someone: %s", truncate(body))
	}

	if b, _ := h.db.SumEntries(t.Context(), 1); b != 0 {
		t.Errorf("balance = %s, want 0.00", b)
	}
	people, _ := h.db.ListParticipants(t.Context(), 1)
	if len(people) != 2 {
		t.Errorf("%d participants, want 2", len(people))
	}
}

// A non-participant cannot reach the settle endpoints either.
func TestNonParticipantCannotSettle(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	h.addUser("stranger@example.com", "Stranger", false)

	tab := h.makeTab("Phone plan")
	h.post(tab+"/charges", url.Values{"csrf_token": {h.csrfToken(tab)}, "amount": {"45.00"}})

	h.loginAs("stranger@example.com", "a-long-enough-password")

	for _, path := range []string{tab + "/card", tab + "/settle"} {
		if resp, _ := h.get(path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s returned %d, want 404", path, resp.StatusCode)
		}
	}
	resp, _ := h.post(tab+"/settle", url.Values{
		"csrf_token": {h.csrfToken("/")}, "amount": {"45.00"}, "method": {"cash"},
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST settle returned %d, want 404", resp.StatusCode)
	}
	if b, _ := h.db.SumEntries(t.Context(), 1); b != -4500 {
		t.Errorf("balance = %s, want -45.00 unchanged", b)
	}
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// ---------------------------------------------------------------------------
// Adjustments: correcting a balance after reconciliation (CHG-03)
// ---------------------------------------------------------------------------

// The Provider reduces what is owed without pretending money moved.
func TestAdjustmentCreditsTheBalance(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	tab := h.makeTab("Reconciled", "Line", "100.00")
	h.post(tab+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"amount":     {"100.00"},
		"memo":       {"January"},
	})

	id := tabIDFrom(t, mustBody(t, h, tab))
	before, err := h.db.SumEntries(t.Context(), id)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}

	_, body := h.post(tab+"/adjustments", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"direction":  {"credit"},
		"amount":     {"40.00"},
		"reason":     {"Reconciliation: double-billed the service fee"},
	})
	if !strings.Contains(body, "credited") {
		t.Errorf("no confirmation of the credit: %s", truncate(body))
	}

	after, err := h.db.SumEntries(t.Context(), id)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if after-before != 4_000 {
		t.Errorf("balance moved by %s, want $40.00 toward zero", (after - before).Display())
	}

	// It is an adjustment, not a payment: the distinction is the point.
	entries, err := h.db.ListEntries(t.Context(), id)
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Kind == store.KindAdjustment {
			found = true
			if e.Amount != 4_000 {
				t.Errorf("adjustment amount %s, want +$40.00", e.Amount.Display())
			}
			if !strings.Contains(e.Memo, "double-billed") {
				t.Errorf("the reason was not stored: %q", e.Memo)
			}
			if e.Method != store.MethodNone {
				t.Errorf("adjustment carries payment method %q", e.Method)
			}
		}
		if e.Kind == store.KindPayment {
			t.Error("the credit was recorded as a payment")
		}
	}
	if !found {
		t.Error("no adjustment entry was written")
	}

	// History labels it by direction rather than by kind.
	_, body = h.get(tab)
	if !strings.Contains(body, "credit") {
		t.Errorf("history does not label the credit: %s", truncate(body))
	}
}

func TestAdjustmentCanIncreaseWhatIsOwed(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	tab := h.makeTab("Reconciled", "Line", "100.00")
	id := tabIDFrom(t, mustBody(t, h, tab))
	before, _ := h.db.SumEntries(t.Context(), id)

	h.post(tab+"/adjustments", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"direction":  {"debit"},
		"amount":     {"15.00"},
		"reason":     {"Missed a delivery on the 4th"},
	})

	after, _ := h.db.SumEntries(t.Context(), id)
	if before-after != 1_500 {
		t.Errorf("balance moved by %s, want $15.00 more owed", (before - after).Display())
	}
}

func TestAdjustmentValidation(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	tab := h.makeTab("Reconciled", "Line", "100.00")

	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{"no reason", url.Values{"direction": {"credit"}, "amount": {"10.00"}}, "needs a reason"},
		{"zero amount", url.Values{"direction": {"credit"}, "amount": {"0"}, "reason": {"x"}}, "greater than zero"},
		{"negative amount", url.Values{"direction": {"credit"}, "amount": {"-5.00"}, "reason": {"x"}}, "greater than zero"},
		{"not a number", url.Values{"direction": {"credit"}, "amount": {"abc"}, "reason": {"x"}}, "not a valid dollar figure"},
		{"unknown direction", url.Values{"direction": {"sideways"}, "amount": {"10.00"}, "reason": {"x"}}, "reduces or increases"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{"csrf_token": {h.csrfToken(tab)}}
			for k, v := range tc.form {
				form[k] = v
			}
			_, body := h.post(tab+"/adjustments", form)
			if !strings.Contains(body, tc.want) {
				t.Errorf("expected %q in the response: %s", tc.want, truncate(body))
			}
		})
	}
}

// Only the Provider may adjust. A Payee on the same tab may not.
func TestAdjustmentIsProviderOnly(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	sam := h.addUser("sam@example.com", "Sam Payee", false)
	tab := h.makeTab("Shared", "Line", "100.00")
	h.post(tab+"/participants", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"user_id":    {strconv.FormatInt(sam.ID, 10)},
	})

	// Sam joins as the payee, and may record payments but not rewrite the debt.
	h.loginAs("sam@example.com", "a-long-enough-password")
	_, body := h.post(tab+"/adjustments", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"direction":  {"credit"},
		"amount":     {"10.00"},
		"reason":     {"I would like to owe less"},
	})
	if !strings.Contains(body, "Only the provider") {
		t.Errorf("a payee was allowed to adjust the balance: %s", truncate(body))
	}
}
