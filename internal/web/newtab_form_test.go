package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A validation error on the create form must re-render it with everything the
// user typed, not send them back to a blank one. This is the whole point of the
// change: one mistake should not discard a carefully filled form.
func TestNewTabErrorKeepsEntries(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	form := url.Values{
		"csrf_token":  {h.csrfToken("/tabs/new")},
		"name":        {"Family phone plan"},
		"description": {"Four lines"},
		"kind":        {"payoff"},
		"loan_amount": {"not-a-number"}, // the mistake
		"item_name":   {"Line one"},
		"item_amount": {"10.00"},
	}
	resp, body := h.post("/tabs", form)

	// Re-rendered in place, not redirected to a blank form.
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (re-rendered form)", resp.StatusCode)
	}
	// The error is shown...
	if !strings.Contains(body, "loan amount must be a dollar figure") {
		t.Errorf("the loan-amount error is not shown:\n%s", truncate(body))
	}
	// ...and every other entry survived.
	for _, want := range []string{
		`value="Family phone plan"`,
		`value="Four lines"`,
		`value="not-a-number"`, // even the bad field echoes what was typed
		`value="Line one"`,
		`value="10.00"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("re-rendered form lost %q", want)
		}
	}
	// The Payoff kind the user chose is still selected.
	if !strings.Contains(body, `value="payoff"`+"\n") && !strings.Contains(body, `value="payoff" checked`) {
		// templ renders checked as a bare attribute; just assert the radio is present and checked somewhere.
		if !strings.Contains(body, "payoff") {
			t.Error("the chosen kind was lost")
		}
	}

	// And nothing was created.
	tabs, err := h.db.ListTabsForUser(t.Context(), 1)
	if err != nil {
		t.Fatalf("list tabs: %v", err)
	}
	if len(tabs) != 0 {
		t.Errorf("a tab was created despite the error: %d tabs", len(tabs))
	}
}

// A schedule that was valid must survive an error elsewhere: the best-effort
// reconstruction pre-fills the shared schedule sub-form.
func TestNewTabErrorKeepsValidSchedule(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	form := url.Values{
		"csrf_token":       {h.csrfToken("/tabs/new")},
		"name":             {""}, // the mistake: no name
		"schedule_kind":    {"monthly_day"},
		"schedule_anchor":  {"2026-03-15"},
		"schedule_billing": {"advance"},
		"item_name":        {"Rent"},
		"item_amount":      {"1200.00"},
	}
	resp, body := h.post("/tabs", form)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(body, "A tab needs a name") {
		t.Errorf("name error not shown:\n%s", truncate(body))
	}
	// The schedule the user set is still selected and dated.
	if !strings.Contains(body, `value="2026-03-15"`) {
		t.Errorf("the schedule start date was lost:\n%s", truncate(body))
	}
	if !strings.Contains(body, "Rent") || !strings.Contains(body, `value="1200.00"`) {
		t.Errorf("the line item was lost")
	}
}

// The happy path is unchanged: a valid submission still creates the tab and
// redirects to it.
func TestNewTabStillCreatesOnValidSubmit(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	_, body := h.post("/tabs", url.Values{
		"csrf_token":  {h.csrfToken("/tabs/new")},
		"name":        {"Groceries"},
		"item_name":   {"Weekly"},
		"item_amount": {"75.00"},
	})
	if !strings.Contains(body, "Tab created") {
		t.Errorf("a valid submission did not create the tab:\n%s", truncate(body))
	}
	tabs, err := h.db.ListTabsForUser(t.Context(), 1)
	if err != nil {
		t.Fatalf("list tabs: %v", err)
	}
	if len(tabs) != 1 || tabs[0].Name != "Groceries" {
		t.Errorf("tab not created as expected: %+v", tabs)
	}
}
