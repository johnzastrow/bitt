package web

import (
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/johnzastrow/bitt/internal/config"
	"github.com/johnzastrow/bitt/internal/store"
)

// A tab's own reminders win over the instance defaults.
func TestReminderForTabPrefersItsOwn(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	tab := h.makeTab("Rent", "Rent", "400.00")
	id := tabIDFrom2(t, tab)

	h.srv().cfg.Reminders = []config.Reminder{
		{Days: 14, Title: "instance 14", Body: "instance body"},
		{Days: 7, Title: "instance 7", Body: "instance body"},
	}

	// Uncustomised: the instance defaults apply.
	if got, ok := h.srv().reminderForTab(t.Context(), id, 14); !ok || got.Title != "instance 14" {
		t.Fatalf("uncustomised tab at 14 days = %+v, %v; want the instance default", got, ok)
	}

	if err := h.db.SetTabReminders(t.Context(), id, []store.TabReminder{
		{Days: 14, Title: "tab 14", Body: "tab body"},
		{Days: 2, Title: "tab 2", Body: "tab body"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	if got, ok := h.srv().reminderForTab(t.Context(), id, 14); !ok || got.Title != "tab 14" {
		t.Errorf("customised tab at 14 days = %+v, %v; want the tab's own", got, ok)
	}
	// A lead time only the tab has still fires.
	if got, ok := h.srv().reminderForTab(t.Context(), id, 2); !ok || got.Title != "tab 2" {
		t.Errorf("tab-only lead time at 2 days = %+v, %v; want the tab's own", got, ok)
	}
	// A customised tab does NOT inherit the instance's other lead times --
	// otherwise dropping the 7-day notice from a tab would be impossible.
	if got, ok := h.srv().reminderForTab(t.Context(), id, 7); ok {
		t.Errorf("customised tab still inherited the instance 7-day reminder: %+v", got)
	}

	// Clearing hands the tab back to the defaults.
	if err := h.db.SetTabReminders(t.Context(), id, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, ok := h.srv().reminderForTab(t.Context(), id, 7); !ok || got.Title != "instance 7" {
		t.Errorf("after clearing, 7 days = %+v, %v; want the instance default back", got, ok)
	}
}

// The Provider can set, see, and clear a tab's reminders through the form.
func TestTabRemindersFormRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	tab := h.makeTab("Rent", "Rent", "400.00")
	h.srv().cfg.Reminders = []config.Reminder{{Days: 14, Title: "instance", Body: "instance"}}

	// The card is on the page, and says the tab is on the instance defaults.
	_, body := h.get(tab)
	if !strings.Contains(body, "Save reminders") {
		t.Fatalf("no reminders card on the tab page: %s", truncate(body))
	}
	if !strings.Contains(body, "uses the instance defaults") {
		t.Errorf("an uncustomised tab does not say it is on the defaults: %s", truncate(body))
	}

	resp, body := h.post(tab+"/reminders", url.Values{
		"csrf_token":     {h.csrfToken(tab)},
		"reminder_days":  {"10, 3, 3, 1"},
		"reminder_title": {"{tab}: {amount} due {when}"},
		"reminder_body":  {"Your {days}-day reminder.\n{due} -- {url}"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("save returned %d: %s", resp.StatusCode, truncate(body))
	}
	if !strings.Contains(body, "10, 3, 1") {
		t.Errorf("the confirmation does not name the saved days: %s", truncate(body))
	}
	if !strings.Contains(body, "sends its own reminders") {
		t.Errorf("the card does not report the tab as customised: %s", truncate(body))
	}

	got, err := h.db.ListTabReminders(t.Context(), tabIDFrom2(t, tab))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Deduplicated, and longest lead first.
	if len(got) != 3 || got[0].Days != 10 || got[1].Days != 3 || got[2].Days != 1 {
		t.Fatalf("stored %+v, want 10/3/1 deduplicated and ordered", got)
	}
	if got[0].Title != "{tab}: {amount} due {when}" {
		t.Errorf("title stored as %q", got[0].Title)
	}

	// Clearing the days returns the tab to the instance defaults.
	_, body = h.post(tab+"/reminders", url.Values{
		"csrf_token":     {h.csrfToken(tab)},
		"reminder_days":  {"  "},
		"reminder_title": {"still here"},
		"reminder_body":  {"still here"},
	})
	if !strings.Contains(body, "instance defaults again") {
		t.Errorf("clearing did not report the fallback: %s", truncate(body))
	}
	if got, err := h.db.ListTabReminders(t.Context(), tabIDFrom2(t, tab)); err != nil || len(got) != 0 {
		t.Errorf("after clearing: %+v (%v), want none", got, err)
	}
}

// A browser submits a <textarea> with CRLF line endings. That is a form-encoding
// artifact rather than something the Provider typed, and refusing it made every
// multi-line message written in a real browser impossible to save -- which is
// what the first Playwright pass over this form hit, after the Go tests (which
// post LF directly) had all passed.
func TestTabRemindersAcceptBrowserLineEndings(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	tab := h.makeTab("Rent", "Rent", "400.00")

	_, body := h.post(tab+"/reminders", url.Values{
		"csrf_token":     {h.csrfToken(tab)},
		"reminder_days":  {"7"},
		"reminder_title": {"{tab} due {when}"},
		"reminder_body":  {"Your {days}-day reminder.\r\n{amount} is owed.\r\n{url}"},
	})
	if strings.Contains(body, "control characters") {
		t.Fatalf("a browser's CRLF body was refused: %s", truncate(body))
	}

	got, err := h.db.ListTabReminders(t.Context(), tabIDFrom2(t, tab))
	if err != nil || len(got) != 1 {
		t.Fatalf("stored %+v (%v), want one reminder", got, err)
	}
	if strings.Contains(got[0].Body, "\r") {
		t.Errorf("a carriage return was stored: %q", got[0].Body)
	}
	if want := "Your {days}-day reminder.\n{amount} is owed.\n{url}"; got[0].Body != want {
		t.Errorf("body stored as %q, want %q", got[0].Body, want)
	}
}

// The templates are the first user-controlled text to reach a mail header, so
// bad input is refused at the form rather than failing every send days later.
func TestTabRemindersRejectBadInput(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	tab := h.makeTab("Rent", "Rent", "400.00")

	cases := []struct {
		name       string
		days       string
		title      string
		body       string
		wantPhrase string
	}{
		{"a non-numeric day", "soon", "T", "B", "comma-separated list"},
		{"a zero day", "0", "T", "B", "comma-separated list"},
		{"a negative day", "-3", "T", "B", "comma-separated list"},
		{"too many lead times", "1,2,3,4,5,6,7", "T", "B", "at most 6 reminders"},
		{"a newline in the title", "7", "Due\nnow", "B", "no line breaks"},
		{"a carriage return in the title", "7", "Due\rnow", "B", "no line breaks"},
		{"an empty title", "7", "   ", "B", "no line breaks"},
		{"a control character in the body", "7", "T", "Owed\x07now", "control characters"},
		{"an empty body", "7", "T", "", "control characters"},
		{"an over-long title", "7", strings.Repeat("x", 200), "B", "The limit is 120"},
		// The message ceiling is ntfy.sh's, and the refusal says so.
		{"a message over the ntfy limit", "7", "T", strings.Repeat("x", 4097), "free ntfy.sh account"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, body := h.post(tab+"/reminders", url.Values{
				"csrf_token":     {h.csrfToken(tab)},
				"reminder_days":  {tc.days},
				"reminder_title": {tc.title},
				"reminder_body":  {tc.body},
			})
			if !strings.Contains(body, tc.wantPhrase) {
				t.Errorf("no refusal mentioning %q: %s", tc.wantPhrase, truncate(body))
			}
			if got, err := h.db.ListTabReminders(t.Context(), tabIDFrom2(t, tab)); err != nil || len(got) != 0 {
				t.Errorf("a refused save still stored %+v (%v)", got, err)
			}
		})
	}
}

// Reminders are a tab setting, so a payee can neither see the card nor post to
// the endpoint.
func TestTabRemindersAreProviderOnly(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	tab := h.makeTab("Rent", "Rent", "400.00")

	payee := h.addUser("payee@example.com", "Payee", false)
	if resp, b := h.post(tab+"/participants", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"user_id":    {itoa(payee.ID)},
	}); resp.StatusCode != 200 {
		t.Fatalf("attach payee returned %d: %s", resp.StatusCode, truncate(b))
	}

	h.loginAs("payee@example.com", "a-long-enough-password")

	_, body := h.get(tab)
	if strings.Contains(body, "Save reminders") {
		t.Errorf("a payee was shown the reminders card: %s", truncate(body))
	}

	_, body = h.post(tab+"/reminders", url.Values{
		"csrf_token":     {h.csrfToken(tab)},
		"reminder_days":  {"7"},
		"reminder_title": {"mine now"},
		"reminder_body":  {"mine now"},
	})
	if !strings.Contains(body, "Only the provider") {
		t.Errorf("a payee's reminder change was not refused: %s", truncate(body))
	}
	if got, err := h.db.ListTabReminders(t.Context(), tabIDFrom2(t, tab)); err != nil || len(got) != 0 {
		t.Errorf("a payee stored %+v (%v)", got, err)
	}
}

// tabIDFrom2 pulls the id back out of a "/tabs/{id}" path.
func tabIDFrom2(t *testing.T, path string) int64 {
	t.Helper()
	id, err := strconv.ParseInt(strings.TrimPrefix(path, "/tabs/"), 10, 64)
	if err != nil {
		t.Fatalf("tab id from %q: %v", path, err)
	}
	return id
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
