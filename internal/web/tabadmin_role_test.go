package web

import (
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/johnzastrow/bitt/internal/store"
)

// idFromTabPath extracts the numeric id from a "/tabs/{id}" path.
func idFromTabPath(t *testing.T, path string) int64 {
	t.Helper()
	id, err := strconv.ParseInt(strings.TrimPrefix(path, "/tabs/"), 10, 64)
	if err != nil {
		t.Fatalf("bad tab path %q: %v", path, err)
	}
	return id
}

// A Provider can attach someone as a per-tab administrator, and the stored role
// reflects it -- not the payee default.
func TestAttachAsAdmin(t *testing.T) {
	h := newHarness(t)
	h.completeSetup() // provider is user 1
	other := h.addUser("other@example.com", "Other Person", false)
	tab := h.makeTab("Household")
	tabID := idFromTabPath(t, tab)

	_, body := h.post(tab+"/participants", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"user_id":    {strconv.FormatInt(other.ID, 10)},
		"role":       {"admin"},
	})
	if !strings.Contains(body, "as an administrator") {
		t.Errorf("expected the attach-as-admin confirmation:\n%s", truncate(body))
	}

	role, err := h.db.ParticipantRole(t.Context(), tabID, other.ID)
	if err != nil {
		t.Fatalf("participant role: %v", err)
	}
	if role != store.RoleAdmin {
		t.Errorf("stored role = %q, want admin", role)
	}
}

// A tab administrator can both manage the tab (change its schedule) and transact
// on it (post a charge), the way a Provider can -- and unlike the instance
// administrator, who cannot transact on a tab they are not a member of.
func TestTabAdminCanManageAndTransact(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	admin := h.addUser("tabadmin@example.com", "Tab Admin", false)
	tab := h.makeTab("Shared plan")
	tabID := idFromTabPath(t, tab)

	// Provider attaches them as an administrator.
	h.post(tab+"/participants", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"user_id":    {strconv.FormatInt(admin.ID, 10)},
		"role":       {"admin"},
	})

	// Act as the tab administrator from here.
	h.loginAs("tabadmin@example.com", "a-long-enough-password")

	// Manage: set a schedule, and confirm it actually stuck (a denial would leave
	// the tab unscheduled).
	h.post(tab+"/schedule", url.Values{
		"csrf_token":       {h.csrfToken(tab)},
		"schedule_kind":    {"monthly_day"},
		"schedule_anchor":  {instanceToday(t).String()},
		"schedule_billing": {"advance"},
	})
	got, err := h.db.GetTab(t.Context(), tabID)
	if err != nil {
		t.Fatalf("get tab: %v", err)
	}
	if !got.Schedule.Set() {
		t.Error("a tab administrator's schedule change did not take -- they were denied managing")
	}

	// Transact: post a charge, and confirm the balance moved (membership grants
	// this; the instance administrator would be refused here).
	if resp, _ := h.post(tab+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"amount":     {"25.00"},
		"memo":       {"Supplies"},
	}); resp.StatusCode != 200 {
		t.Fatalf("charge post returned %d", resp.StatusCode)
	}
	bal, err := h.srv().ledger.Balance(t.Context(), tabID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal >= 0 {
		t.Errorf("a tab administrator's charge did not post; balance = %d, want negative", bal)
	}
}

// A payee is unchanged: they may see the tab but not manage it. This guards the
// boundary the new role widened -- CanManage must not have leaked to payees.
func TestPayeeStillCannotManage(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	payee := h.addUser("payee@example.com", "Payee", false)
	tab := h.makeTab("Rent")

	h.post(tab+"/participants", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"user_id":    {strconv.FormatInt(payee.ID, 10)},
		"role":       {"payee"},
	})
	h.loginAs("payee@example.com", "a-long-enough-password")

	_, body := h.post(tab+"/schedule", url.Values{
		"csrf_token":    {h.csrfToken(tab)},
		"schedule_kind": {""},
	})
	if !strings.Contains(body, "Only the provider") {
		t.Errorf("a payee was allowed to manage the tab:\n%s", truncate(body))
	}
}

// An unrecognised role is refused rather than silently treated as a payee, so a
// typo or a tampered form cannot quietly attach someone with the wrong rights.
func TestAttachRejectsUnknownRole(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	other := h.addUser("other@example.com", "Other", false)
	tab := h.makeTab("Tab")
	tabID := idFromTabPath(t, tab)

	h.post(tab+"/participants", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"user_id":    {strconv.FormatInt(other.ID, 10)},
		"role":       {"superuser"},
	})
	if _, err := h.db.ParticipantRole(t.Context(), tabID, other.ID); err == nil {
		t.Error("an unknown role attached the person anyway")
	}
}
