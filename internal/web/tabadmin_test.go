package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// makeTabAs creates a tab as whoever is currently signed in, and finds it by
// name among that user's tabs. makeTab assumes user 1; this does not.
func (h *harness) makeTabAs(name string, ownerID int64, items ...string) string {
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

	tabs, err := h.db.ListTabsForUser(h.t.Context(), ownerID)
	if err != nil {
		h.t.Fatalf("list tabs: %v", err)
	}
	for _, t := range tabs {
		if t.Name == name {
			return tabPath(t.ID)
		}
	}
	h.t.Fatalf("tab %q was not created for user %d", name, ownerID)
	return ""
}

// ---------------------------------------------------------------------------
// Tab details: name, description, kind (TAB-01, TAB-02)
// ---------------------------------------------------------------------------

func TestTabKindIsChosenAtCreationAndShown(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	// Services is the default when nothing is stated.
	_, body := h.post("/tabs", url.Values{
		"csrf_token": {h.csrfToken("/tabs/new")},
		"name":       {"Phone plan"},
	})
	if !strings.Contains(body, "Services") {
		t.Errorf("a tab created without a kind does not read as Services: %s", truncate(body))
	}
	if !strings.Contains(body, "Recurring, with no defined end") {
		t.Errorf("the Services description is not shown: %s", truncate(body))
	}

	// Payoff is selectable, and the tab says so at the top.
	_, body = h.post("/tabs", url.Values{
		"csrf_token": {h.csrfToken("/tabs/new")},
		"name":       {"Truck loan"},
		"kind":       {string(store.TabPayoff)},
	})
	if !strings.Contains(body, "Payoff") {
		t.Errorf("a Payoff tab does not read as one: %s", truncate(body))
	}
	if !strings.Contains(body, "A fixed total drawn down by payments") {
		t.Errorf("the Payoff description is not shown: %s", truncate(body))
	}

	tabID := tabIDFrom(t, body)
	tab, err := h.db.GetTab(t.Context(), tabID)
	if err != nil {
		t.Fatalf("get tab: %v", err)
	}
	if tab.Kind != store.TabPayoff {
		t.Errorf("stored kind is %q, want %q", tab.Kind, store.TabPayoff)
	}

	// An unrecognized kind is refused rather than stored.
	_, body = h.post("/tabs", url.Values{
		"csrf_token": {h.csrfToken("/tabs/new")},
		"name":       {"Nonsense"},
		"kind":       {"mortgage"},
	})
	if !strings.Contains(body, "not a kind of tab") {
		t.Errorf("an unknown kind was not refused: %s", truncate(body))
	}
}

func TestTabDetailsAreEditable(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	tab := h.makeTab("Origional Naem", "Line", "10.00")
	tabID := tabIDFrom(t, mustBody(t, h, tab))

	resp, body := h.post(tab+"/details", url.Values{
		"csrf_token":  {h.csrfToken(tab)},
		"name":        {"Family phone plan"},
		"description": {"Shared carrier bill"},
		"kind":        {string(store.TabPayoff)},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("edit details returned %d: %s", resp.StatusCode, truncate(body))
	}
	if !strings.Contains(body, "Family phone plan") {
		t.Errorf("the new name is not shown: %s", truncate(body))
	}
	if !strings.Contains(body, "Shared carrier bill") {
		t.Errorf("the new description is not shown: %s", truncate(body))
	}
	// A kind change is called out, since it changes how everything below reads.
	if !strings.Contains(body, "now a Payoff tab") {
		t.Errorf("the kind change was not explained: %s", truncate(body))
	}

	got, err := h.db.GetTab(t.Context(), tabID)
	if err != nil {
		t.Fatalf("get tab: %v", err)
	}
	if got.Name != "Family phone plan" || got.Kind != store.TabPayoff {
		t.Errorf("stored tab is %+v, want the renamed Payoff tab", got)
	}
}

// Renaming touches no entry, so no balance can move.
func TestEditingDetailsLeavesTheLedgerAlone(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	tab := h.makeTab("Phone plan", "Line", "45.00")
	h.post(tab+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"amount":     {"45.00"},
	})
	tabID := tabIDFrom(t, mustBody(t, h, tab))

	before, err := h.db.SumEntries(t.Context(), tabID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	beforeEntries, _ := h.db.ListEntries(t.Context(), tabID)

	h.post(tab+"/details", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"name":       {"Renamed"},
		"kind":       {string(store.TabPayoff)},
	})

	after, err := h.db.SumEntries(t.Context(), tabID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if after != before {
		t.Errorf("balance moved from %s to %s on a rename", before, after)
	}
	afterEntries, _ := h.db.ListEntries(t.Context(), tabID)
	if len(afterEntries) != len(beforeEntries) {
		t.Errorf("entry count changed from %d to %d on a rename", len(beforeEntries), len(afterEntries))
	}
}

func TestTabDetailsRejectBadInput(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	tab := h.makeTab("Phone plan", "Line", "10.00")

	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{"empty name", url.Values{"name": {"  "}}, "needs a name"},
		{"long name", url.Values{"name": {strings.Repeat("x", 121)}}, "name is too long"},
		{"long description", url.Values{"name": {"Fine"}, "description": {strings.Repeat("x", 501)}}, "description is too long"},
		{"unknown kind", url.Values{"name": {"Fine"}, "kind": {"mortgage"}}, "not a kind of tab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{"csrf_token": {h.csrfToken(tab)}}
			for k, v := range tc.form {
				form[k] = v
			}
			_, body := h.post(tab+"/details", form)
			if !strings.Contains(body, tc.want) {
				t.Errorf("message did not mention %q: %s", tc.want, truncate(body))
			}
		})
	}
	if !strings.Contains(mustBody(t, h, tab), "Phone plan") {
		t.Error("a refused edit changed the tab anyway")
	}
}

// ---------------------------------------------------------------------------
// Archival
// ---------------------------------------------------------------------------

// Archiving retires a tab and stops it billing, without touching its history.
func TestArchivingStopsAccrualAndKeepsHistory(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	// Weekly, anchored four weeks back: five cycles post on the first read.
	today := instanceToday(t)
	tabID, _ := h.createScheduledTab(today.AddDays(-28), schedule.Weekly, schedule.InAdvance)
	tab := tabPath(tabID)

	posted, err := h.db.ListPostedPeriods(t.Context(), tabID)
	if err != nil {
		t.Fatalf("list periods: %v", err)
	}
	if len(posted) != 5 {
		t.Fatalf("posted %d cycles, want 5", len(posted))
	}
	balanceBefore, _ := h.db.SumEntries(t.Context(), tabID)

	resp, body := h.post(tab+"/archive", url.Values{"csrf_token": {h.csrfToken(tab)}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archive returned %d: %s", resp.StatusCode, truncate(body))
	}
	if !strings.Contains(body, "archived") {
		t.Errorf("the page does not say the tab is archived: %s", truncate(body))
	}

	got, err := h.db.GetTab(t.Context(), tabID)
	if err != nil {
		t.Fatalf("get tab: %v", err)
	}
	if got.ArchivedAt == nil {
		t.Fatal("the tab was not archived")
	}

	// The history and the balance are exactly as they were: archiving is not
	// deletion.
	balanceAfter, _ := h.db.SumEntries(t.Context(), tabID)
	if balanceAfter != balanceBefore {
		t.Errorf("balance moved from %s to %s on archiving", balanceBefore, balanceAfter)
	}
	still, _ := h.db.ListPostedPeriods(t.Context(), tabID)
	if len(still) != len(posted) {
		t.Errorf("archiving changed the posted cycles: %d, want %d", len(still), len(posted))
	}

	// And it stops billing. Reading it repeatedly posts nothing more, even
	// though the schedule would otherwise have kept going.
	for range 3 {
		h.get(tab)
	}
	after, _ := h.db.ListPostedPeriods(t.Context(), tabID)
	if len(after) != len(posted) {
		t.Errorf("an archived tab kept accruing: %d cycles, want %d", len(after), len(posted))
	}

	// The dashboard shows it as archived rather than hiding it outright.
	_, dash := h.get("/")
	if !strings.Contains(dash, "archived") {
		t.Errorf("the dashboard does not mark the archived tab: %s", truncate(dash))
	}

	// Reactivating brings it back, and the catch-up it missed posts on the
	// next read.
	resp, body = h.post(tab+"/archive", url.Values{"csrf_token": {h.csrfToken(tab)}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reactivate returned %d: %s", resp.StatusCode, truncate(body))
	}
	got, _ = h.db.GetTab(t.Context(), tabID)
	if got.ArchivedAt != nil {
		t.Error("the tab was not reactivated")
	}
}

// ---------------------------------------------------------------------------
// Participants (TAB-03)
// ---------------------------------------------------------------------------

func TestParticipantCanBeRemoved(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	sam := h.addUser("sam@example.com", "Sam Payee", false)
	tab := h.makeTab("Phone plan", "Line", "10.00")
	tabID := tabIDFrom(t, mustBody(t, h, tab))

	h.post(tab+"/participants", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"user_id":    {strconv.FormatInt(sam.ID, 10)},
	})
	if people, _ := h.db.ListParticipants(t.Context(), tabID); len(people) != 2 {
		t.Fatalf("tab has %d participants, want 2", len(people))
	}

	resp, body := h.post(tab+"/participants/"+strconv.FormatInt(sam.ID, 10)+"/remove",
		url.Values{"csrf_token": {h.csrfToken(tab)}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remove returned %d: %s", resp.StatusCode, truncate(body))
	}

	people, err := h.db.ListParticipants(t.Context(), tabID)
	if err != nil {
		t.Fatalf("list participants: %v", err)
	}
	if len(people) != 1 || people[0].UserID == sam.ID {
		t.Errorf("participants are %+v, want only the provider", people)
	}

	// Sam can no longer reach the tab at all.
	h.loginAs("sam@example.com", "a-long-enough-password")
	if resp, _ := h.get(tab); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a removed participant still reaches the tab: %d", resp.StatusCode)
	}
}

// A tab must keep a Provider, or it could never be billed again and no
// provider-role check would ever admit anyone.
func TestLastProviderCannotBeRemoved(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	tab := h.makeTab("Phone plan", "Line", "10.00")
	tabID := tabIDFrom(t, mustBody(t, h, tab))
	people, _ := h.db.ListParticipants(t.Context(), tabID)
	provider := people[0].UserID

	_, body := h.post(tab+"/participants/"+strconv.FormatInt(provider, 10)+"/remove",
		url.Values{"csrf_token": {h.csrfToken(tab)}})
	if !strings.Contains(body, "at least one provider") {
		t.Errorf("removing the last provider was not refused: %s", truncate(body))
	}
	if still, _ := h.db.ListParticipants(t.Context(), tabID); len(still) != 1 {
		t.Errorf("the last provider was removed anyway")
	}
}

// ---------------------------------------------------------------------------
// The administrator exception to AUTH-05
// ---------------------------------------------------------------------------

// An administrator may manage any tab in the instance, including one they are
// not on. This is a deliberate exception so a tab whose Provider has left can
// still be repaired.
func TestAdminCanManageATabTheyAreNotOn(t *testing.T) {
	h := newHarness(t)
	h.completeSetup() // jane@example.com is the first admin

	// A second person, not an admin, owns a tab of their own.
	h.addUser("sam@example.com", "Sam Provider", false)
	h.loginAs("sam@example.com", "a-long-enough-password")
	tab := h.makeTabAs("Sam's tab", 2, "Line", "10.00")
	tabID := tabIDFrom(t, mustBody(t, h, tab))

	// The admin, who is not on that tab, can see it and is told so.
	h.loginAs("jane@example.com", "correct-horse-battery")
	resp, body := h.get(tab)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin could not reach a foreign tab: %d", resp.StatusCode)
	}
	if !strings.Contains(body, "seeing it as an administrator") {
		t.Errorf("the page does not say the access is administrative: %s", truncate(body))
	}

	// And can rename it.
	resp, body = h.post(tab+"/details", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"name":       {"Renamed by an admin"},
		"kind":       {string(store.TabServices)},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin rename returned %d: %s", resp.StatusCode, truncate(body))
	}
	got, err := h.db.GetTab(t.Context(), tabID)
	if err != nil {
		t.Fatalf("get tab: %v", err)
	}
	if got.Name != "Renamed by an admin" {
		t.Errorf("tab name is %q, want the admin's rename", got.Name)
	}

	// But money stays with the people on the tab: the payment form is not
	// offered, and posting one is refused.
	if strings.Contains(body, "Record a payment</h2>") {
		t.Errorf("a non-participant admin was offered the payment form")
	}
	resp, _ = h.post(tab+"/payments", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"amount":     {"10.00"},
		"method":     {string(store.MethodCash)},
	})
	balance, err := h.db.SumEntries(t.Context(), tabID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if balance != 0 {
		t.Errorf("a non-participant admin moved money: balance %s, want 0", balance)
	}
}

// A non-admin non-participant still gets 404, so the administrator exception
// did not widen the hole for anyone else (AUTH-05).
func TestNonAdminStillCannotReachAForeignTab(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	tab := h.makeTab("Jane's tab", "Line", "10.00")

	h.addUser("sam@example.com", "Sam Nobody", false)
	h.loginAs("sam@example.com", "a-long-enough-password")

	if resp, _ := h.get(tab); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET a foreign tab as a non-admin returned %d, want 404", resp.StatusCode)
	}
	for _, path := range []string{"/details", "/archive", "/schedule", "/items"} {
		resp, _ := h.post(tab+path, url.Values{
			"csrf_token": {h.csrfToken("/")},
			"name":       {"Injected"},
			"amount":     {"1.00"},
		})
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s as a non-admin returned %d, want 404", tab+path, resp.StatusCode)
		}
	}
}

// A Payee on a tab may see it but may not change what it is.
func TestPayeeCannotManageTheTab(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	sam := h.addUser("sam@example.com", "Sam Payee", false)
	tab := h.makeTab("Phone plan", "Line", "10.00")
	h.post(tab+"/participants", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"user_id":    {strconv.FormatInt(sam.ID, 10)},
	})

	h.loginAs("sam@example.com", "a-long-enough-password")
	resp, body := h.post(tab+"/details", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"name":       {"Renamed by the payee"},
	})
	// A participant gets a message rather than a 404, since they can already
	// see the tab and a 404 would tell them nothing true.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("returned %d, want the tab page with a refusal", resp.StatusCode)
	}
	if !strings.Contains(body, "Only the provider") {
		t.Errorf("the refusal was not explained: %s", truncate(body))
	}
	if strings.Contains(body, "Renamed by the payee") {
		t.Error("a payee renamed the tab")
	}
}
