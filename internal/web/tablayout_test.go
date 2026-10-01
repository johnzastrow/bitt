package web

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/schedule"
)

// The owner's 2026-10-01 tab-page changes: Record a payment in the top card,
// every section collapsed when the page opens, Transfer as the default method.

var transferSelected = regexp.MustCompile(`<option value="transfer" selected>`)

func TestPaymentFormInTheTopCardAndSectionsCollapsed(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	tabID := h.createPlainTab("Rent")
	_, body := h.get(tabPath(tabID))

	hero := body[strings.Index(body, `class="card tabhero`):]
	hero = hero[:strings.Index(hero, "</section>")]
	if !strings.Contains(hero, "<h2>Record a payment</h2>") || !strings.Contains(hero, `action="/tabs/`) {
		t.Error("Record a payment is not in the top card")
	}
	if strings.Count(body, "<h2>Record a payment</h2>") != 1 {
		t.Error("the payment form appears more than once")
	}
	// No section opens expanded.
	if regexp.MustCompile(`<details class="group" open`).MatchString(body) {
		t.Error("a section opens expanded")
	}
	if !strings.Contains(body, `<details class="group">`) {
		t.Error("the sections are not collapsible groups")
	}
	if !transferSelected.MatchString(hero) {
		t.Error("the tab's payment form does not default to Transfer")
	}
}

func TestTransferIsTheDefaultEverywhere(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	tabID := h.createPlainTab("Rent")
	h.post(tabPath(tabID)+"/charges", map[string][]string{
		"csrf_token": {h.csrfToken(tabPath(tabID))}, "amount": {"40.00"}, "memo": {"x"},
	})
	for _, p := range []string{tabPath(tabID), tabPath(tabID) + "/pay", tabPath(tabID) + "/settle"} {
		_, body := h.get(p)
		if !strings.Contains(body, `name="method"`) {
			t.Errorf("%s has no method choice", p)
			continue
		}
		if !transferSelected.MatchString(body) {
			t.Errorf("%s does not preselect Transfer", p)
		}
		if strings.Contains(body, `<option value="cash" selected>`) {
			t.Errorf("%s still preselects Cash", p)
		}
	}
}

func TestLoanProgressStartsCollapsed(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	id, _ := h.createPayoffTab("10000.00", "500.00", schedule.DateOf(time.Now()), nil)
	_, body := h.get(tabPath(id))
	if !strings.Contains(body, `<details class="card fold loanfold">`) || strings.Contains(body, `loanfold" open`) {
		t.Error("loan progress is not a collapsed section")
	}
	if !strings.Contains(body, "still owed</span></summary>") {
		t.Error("the collapsed loan heading does not say what is owed")
	}
}
