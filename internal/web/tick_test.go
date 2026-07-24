package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/config"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// The tick endpoint fails CLOSED when no secret is configured: it must reject
// every request, so a deployment that has not set BITT_TICK_SECRET cannot be
// driven at all.
func TestTickFailsClosedWithoutSecret(t *testing.T) {
	h := newHarness(t) // harness config sets no tick secret
	resp := h.postRaw(t, "/internal/tick", "")
	if resp.StatusCode == http.StatusOK {
		t.Errorf("tick accepted a request with no secret configured (status %d)", resp.StatusCode)
	}
}

// With a secret configured, a request must present it; a missing or wrong
// secret is refused before any work.
func TestTickRequiresTheSecret(t *testing.T) {
	h := newHarnessWithTick(t, "the-cron-secret")

	if resp := h.postRaw(t, "/internal/tick", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no secret: status %d, want 401", resp.StatusCode)
	}
	if resp := h.postRaw(t, "/internal/tick", "Bearer wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong secret: status %d, want 401", resp.StatusCode)
	}
	resp := h.postRaw(t, "/internal/tick", "Bearer the-cron-secret")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("correct secret: status %d, want 200", resp.StatusCode)
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":  "abc",
		"bearer abc":  "abc",
		"Bearer  abc": "abc",
		"abc":         "",
		"":            "",
		"Bearer ":     "",
	}
	for in, want := range cases {
		if got := bearerToken(in); got != want {
			t.Errorf("bearerToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReminderMessageKeepsBodyClean(t *testing.T) {
	h := newHarness(t)
	spec := config.Reminder{Days: 7, Title: "Payment due {when}", Body: "Tab: {tab}"}
	m := h.srv().reminderMessage(spec, tabNamed("Rent"), dateOf(2026, 8, 1), 7, -5000)
	if !strings.Contains(m.Body, "Rent") {
		t.Errorf("body should carry the tab name: %q", m.Body)
	}
	if m.Title != "Payment due in one week" {
		t.Errorf("title = %q", m.Title)
	}
}

func tabNamed(name string) store.Tab { return store.Tab{Name: name} }
func dateOf(y int, m, d int) schedule.Date {
	return schedule.NewDate(y, time.Month(m), d)
}

func TestReminderMessageRendersVariables(t *testing.T) {
	h := newHarness(t)
	h.srv().cfg.BaseURL = "https://bitt.example"
	spec := config.Reminder{Days: 7, Title: "Due {when}: {tab}", Body: "You owe {amount} by {due}. Pay: {url}"}
	m := h.srv().reminderMessage(spec, store.Tab{ID: 4, Name: "Rent"}, dateOf(2026, 8, 1), 7, -50000)
	if m.Title != "Due in one week: Rent" {
		t.Errorf("title = %q", m.Title)
	}
	if !strings.Contains(m.Body, "$500.00") || !strings.Contains(m.Body, "Aug 1, 2026") ||
		!strings.Contains(m.Body, "https://bitt.example/tabs/4") {
		t.Errorf("body missing a variable: %q", m.Body)
	}
}
