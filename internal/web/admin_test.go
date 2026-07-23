package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/johnzastrow/bitt/internal/store"
)

// AUTH-04: an admin adds an account and the new person can sign in.
func TestAdminAddsUser(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	_, body := h.post("/admin/users", url.Values{
		"csrf_token":   {h.csrfToken("/admin/users")},
		"display_name": {"Sam"},
		"email":        {"sam@example.com"},
		"password":     {"sams-long-password"},
	})
	if !strings.Contains(body, "can now sign in") {
		t.Fatalf("account not created: %s", truncate(body))
	}

	h.loginAs("sam@example.com", "sams-long-password")
	if _, body := h.get("/"); !strings.Contains(body, "Your tabs") {
		t.Errorf("new account cannot sign in: %s", truncate(body))
	}
	// A non-admin does not see the admin screen at all.
	if resp, _ := h.get("/admin/users"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("non-admin got %d for /admin/users, want 404", resp.StatusCode)
	}
}

// A non-admin cannot create accounts even by posting directly.
func TestNonAdminCannotCreateUsers(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	h.addUser("sam@example.com", "Sam", false)
	h.loginAs("sam@example.com", "a-long-enough-password")

	resp, _ := h.post("/admin/users", url.Values{
		"csrf_token":   {h.csrfToken("/")},
		"display_name": {"Sneaky"},
		"email":        {"sneaky@example.com"},
		"password":     {"a-long-enough-password"},
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /admin/users as non-admin returned %d, want 404", resp.StatusCode)
	}

	users, _ := h.db.ListUsers(t.Context())
	if len(users) != 2 {
		t.Errorf("%d accounts exist, want 2 -- a non-admin created one", len(users))
	}
}

// Duplicate emails are refused with a useful message rather than a 500.
func TestAdminDuplicateEmailRefused(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	form := url.Values{
		"csrf_token":   {h.csrfToken("/admin/users")},
		"display_name": {"Sam"},
		"email":        {"SAM@example.com"},
		"password":     {"sams-long-password"},
	}
	h.post("/admin/users", form)

	form.Set("csrf_token", h.csrfToken("/admin/users"))
	form.Set("email", "sam@EXAMPLE.com") // same address, different case
	_, body := h.post("/admin/users", form)
	if !strings.Contains(body, "already exists") {
		t.Errorf("duplicate email not refused: %s", truncate(body))
	}

	users, _ := h.db.ListUsers(t.Context())
	if len(users) != 2 {
		t.Errorf("%d accounts, want 2", len(users))
	}
}

// Short passwords are refused at the admin form, not just at setup.
func TestAdminPasswordPolicy(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	_, body := h.post("/admin/users", url.Values{
		"csrf_token":   {h.csrfToken("/admin/users")},
		"display_name": {"Sam"},
		"email":        {"sam@example.com"},
		"password":     {"short"},
	})
	if !strings.Contains(body, "at least 12 characters") {
		t.Errorf("weak password accepted: %s", truncate(body))
	}
	if users, _ := h.db.ListUsers(t.Context()); len(users) != 1 {
		t.Errorf("%d accounts, want 1", len(users))
	}
}

// Deactivation ends the account's sessions immediately rather than letting them
// run until expiry.
func TestDeactivationEndsSessions(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	sam := h.addUser("sam@example.com", "Sam", false)

	// Sam signs in on their own client.
	sams := newClientFor(t, h)
	sams.login("sam@example.com", "a-long-enough-password")
	if _, body := sams.get("/"); !strings.Contains(body, "Your tabs") {
		t.Fatal("Sam could not sign in")
	}

	// Jane deactivates the account.
	_, body := h.post("/admin/users/"+itoa64(sam.ID)+"/active", url.Values{
		"csrf_token": {h.csrfToken("/admin/users")},
		"active":     {"false"},
	})
	if !strings.Contains(body, "sessions ended") {
		t.Fatalf("deactivation failed: %s", truncate(body))
	}

	// Sam's existing session no longer works.
	if _, body := sams.get("/"); strings.Contains(body, "Your tabs") {
		t.Error("a deactivated account still has a working session")
	}
	// And cannot sign in again.
	sams.login("sam@example.com", "a-long-enough-password")
	if _, body := sams.get("/"); strings.Contains(body, "Your tabs") {
		t.Error("a deactivated account was able to sign in")
	}
}

// The last active administrator cannot be deactivated, or nobody could
// administer the instance again.
func TestCannotDeactivateLastAdmin(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	// A second admin, so Jane is not the only one and self-deactivation is the
	// only rule being tested here.
	second := h.addUser("kim@example.com", "Kim", true)

	// Jane cannot deactivate herself regardless.
	_, body := h.post("/admin/users/1/active", url.Values{
		"csrf_token": {h.csrfToken("/admin/users")},
		"active":     {"false"},
	})
	if !strings.Contains(body, "cannot deactivate your own account") {
		t.Errorf("self-deactivation not refused: %s", truncate(body))
	}

	// Deactivating the other admin is fine while Jane remains.
	_, body = h.post("/admin/users/"+itoa64(second.ID)+"/active", url.Values{
		"csrf_token": {h.csrfToken("/admin/users")},
		"active":     {"false"},
	})
	if !strings.Contains(body, "deactivated") {
		t.Errorf("deactivating a non-last admin failed: %s", truncate(body))
	}

	// The store refuses to strip the last admin even when asked directly,
	// which is the guarantee the handler's self-check alone would not give.
	if err := h.db.SetUserActive(t.Context(), 1, false); err == nil {
		t.Error("the store allowed the last administrator to be deactivated")
	} else if !strings.Contains(err.Error(), "last administrator") {
		t.Errorf("unexpected error deactivating last admin: %v", err)
	}

	users, _ := h.db.ListUsers(t.Context())
	activeAdmins := 0
	for _, u := range users {
		if u.IsAdmin && u.Active() {
			activeAdmins++
		}
	}
	if activeAdmins < 1 {
		t.Fatal("the instance has no active administrator left")
	}
}

// A reactivated account works again.
func TestReactivation(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	sam := h.addUser("sam@example.com", "Sam", false)

	h.post("/admin/users/"+itoa64(sam.ID)+"/active", url.Values{
		"csrf_token": {h.csrfToken("/admin/users")}, "active": {"false"},
	})
	h.post("/admin/users/"+itoa64(sam.ID)+"/active", url.Values{
		"csrf_token": {h.csrfToken("/admin/users")}, "active": {"true"},
	})

	got, err := h.db.GetUser(t.Context(), sam.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Active() {
		t.Error("account was not reactivated")
	}

	sams := newClientFor(t, h)
	sams.login("sam@example.com", "a-long-enough-password")
	if _, body := sams.get("/"); !strings.Contains(body, "Your tabs") {
		t.Error("reactivated account cannot sign in")
	}
}

// A deactivated account cannot be attached to a tab.
func TestDeactivatedUserNotAttachable(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	sam := h.addUser("sam@example.com", "Sam", false)
	tab := h.makeTab("Phone plan")

	h.post("/admin/users/"+itoa64(sam.ID)+"/active", url.Values{
		"csrf_token": {h.csrfToken("/admin/users")}, "active": {"false"},
	})

	_, body := h.post(tab+"/participants", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"user_id":    {itoa64(sam.ID)},
	})
	if !strings.Contains(body, "deactivated") {
		t.Errorf("a deactivated account was attachable: %s", truncate(body))
	}

	people, _ := h.db.ListParticipants(t.Context(), 1)
	if len(people) != 1 {
		t.Errorf("%d participants, want 1", len(people))
	}
}

// Attaching the same person twice is refused rather than duplicating a row.
func TestDuplicateAttachRefused(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	sam := h.addUser("sam@example.com", "Sam", false)
	tab := h.makeTab("Phone plan")

	form := url.Values{"csrf_token": {h.csrfToken(tab)}, "user_id": {itoa64(sam.ID)}}
	h.post(tab+"/participants", form)

	form.Set("csrf_token", h.csrfToken(tab))
	_, body := h.post(tab+"/participants", form)
	if !strings.Contains(body, "already on this tab") {
		t.Errorf("duplicate attach not refused: %s", truncate(body))
	}

	people, _ := h.db.ListParticipants(t.Context(), 1)
	if len(people) != 2 {
		t.Errorf("%d participants, want 2", len(people))
	}
	roles := map[store.Role]int{}
	for _, p := range people {
		roles[p.Role]++
	}
	if roles[store.RoleProvider] != 1 || roles[store.RolePayee] != 1 {
		t.Errorf("roles = %v, want one provider and one payee", roles)
	}
}
