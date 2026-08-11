package web

import (
	"net/url"
	"strings"
	"testing"
)

// REM-03: an administrator fixes another account's notification settings.
//
// The gap: two accounts held a retired ntfy topic and published into a void,
// and nobody but those two people could correct it.

func TestAdminCanSetAnotherUsersNotificationSettings(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	ctx := t.Context()

	them := h.addUser("them@example.com", "Them", false)
	if err := h.db.SetNotifyPrefs(ctx, them.ID, "FuzzyShark14", true, true); err != nil {
		t.Fatalf("seed prefs: %v", err)
	}

	_, body := h.post("/admin/users/"+itoa(them.ID)+"/notify", url.Values{
		"csrf_token":   {h.csrfToken("/admin/users")},
		"ntfy_topic":   {"btabby-them"},
		"notify_email": {"1"},
		"notify_ntfy":  {"1"},
	})
	if !strings.Contains(body, "Notification settings updated") {
		t.Fatalf("no confirmation: %s", truncate(body))
	}

	got, err := h.db.GetUser(ctx, them.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.NtfyTopic != "btabby-them" {
		t.Errorf("topic = %q, want the one the admin set", got.NtfyTopic)
	}
	if !got.NotifyEmail || !got.NotifyNtfy {
		t.Errorf("toggles not applied: email=%v ntfy=%v", got.NotifyEmail, got.NotifyNtfy)
	}
}

// An unchecked box means off. A form that only ever turned things ON would make
// the control useless for switching a noisy channel off for somebody.
func TestAdminCanTurnAChannelOffForAnotherUser(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	ctx := t.Context()

	them := h.addUser("them@example.com", "Them", false)
	if err := h.db.SetNotifyPrefs(ctx, them.ID, "btabby-them", true, true); err != nil {
		t.Fatalf("seed: %v", err)
	}

	h.post("/admin/users/"+itoa(them.ID)+"/notify", url.Values{
		"csrf_token": {h.csrfToken("/admin/users")},
		"ntfy_topic": {"btabby-them"},
		// both boxes unchecked
	})

	got, _ := h.db.GetUser(ctx, them.ID)
	if got.NotifyEmail || got.NotifyNtfy {
		t.Errorf("unchecked boxes did not switch the channels off: email=%v ntfy=%v",
			got.NotifyEmail, got.NotifyNtfy)
	}
}

// The same validation the person's own profile applies. A topic an
// administrator sets must be one the owner could have set themselves.
func TestAdminCannotSetAnInvalidTopic(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	ctx := t.Context()

	them := h.addUser("them@example.com", "Them", false)
	if err := h.db.SetNotifyPrefs(ctx, them.ID, "btabby-them", true, true); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, body := h.post("/admin/users/"+itoa(them.ID)+"/notify", url.Values{
		"csrf_token": {h.csrfToken("/admin/users")},
		"ntfy_topic": {"not a valid topic/../etc"},
	})
	if !strings.Contains(body, "not valid") {
		t.Errorf("no refusal shown: %s", truncate(body))
	}
	got, _ := h.db.GetUser(ctx, them.ID)
	if got.NtfyTopic != "btabby-them" {
		t.Errorf("an invalid topic was stored: %q", got.NtfyTopic)
	}
}

// Scope: notification settings only. An administrator resetting a password or
// changing an email is a different feature with a different risk profile, and
// this endpoint must not become a way to do it.
func TestAdminNotifyEndpointChangesNothingElse(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	ctx := t.Context()

	them := h.addUser("them@example.com", "Them", false)
	before, _ := h.db.GetUser(ctx, them.ID)

	h.post("/admin/users/"+itoa(them.ID)+"/notify", url.Values{
		"csrf_token":   {h.csrfToken("/admin/users")},
		"ntfy_topic":   {"btabby-them"},
		"email":        {"attacker@example.com"},
		"display_name": {"Renamed"},
		"password":     {"a-new-password-entirely"},
		"is_admin":     {"1"},
	})

	after, _ := h.db.GetUser(ctx, them.ID)
	if after.Email != before.Email {
		t.Errorf("email changed: %q -> %q", before.Email, after.Email)
	}
	if after.DisplayName != before.DisplayName {
		t.Errorf("display name changed: %q -> %q", before.DisplayName, after.DisplayName)
	}
	if after.PasswordHash != before.PasswordHash {
		t.Error("password hash changed -- this endpoint must not reset passwords")
	}
	if after.IsAdmin != before.IsAdmin {
		t.Errorf("admin flag changed: %v -> %v", before.IsAdmin, after.IsAdmin)
	}
}

// Only administrators. An ordinary account must not be able to edit anyone's
// settings, including its own through this route.
func TestNonAdminCannotUseTheAdminNotifyEndpoint(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	ctx := t.Context()

	them := h.addUser("them@example.com", "Them", false)
	other := h.addUser("other@example.com", "Other", false)
	if err := h.db.SetNotifyPrefs(ctx, them.ID, "btabby-them", true, true); err != nil {
		t.Fatalf("seed: %v", err)
	}

	c := h.newClient()
	c.loginAs("other@example.com", "a-long-enough-password")
	c.post("/admin/users/"+itoa(them.ID)+"/notify", url.Values{
		"csrf_token": {c.csrfToken("/")},
		"ntfy_topic": {"hijacked"},
	})
	_ = other

	got, _ := h.db.GetUser(ctx, them.ID)
	if got.NtfyTopic != "btabby-them" {
		t.Errorf("a non-admin changed another account's topic: %q", got.NtfyTopic)
	}
}

// The People screen shows every topic, so two accounts sharing one is visible
// at a glance rather than buried in separate profiles.
func TestPeopleScreenShowsTopics(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	ctx := t.Context()

	a := h.addUser("a@example.com", "Ann", false)
	b := h.addUser("b@example.com", "Bob", false)
	for _, id := range []int64{a.ID, b.ID} {
		if err := h.db.SetNotifyPrefs(ctx, id, "SharedTopic", true, true); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	_, body := h.get("/admin/users")
	if strings.Count(body, "SharedTopic") < 2 {
		t.Errorf("both accounts' topics should appear, so a shared one is obvious: %s", truncate(body))
	}
}
