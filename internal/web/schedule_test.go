package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// tabIDFrom scrapes the id of the tab the client is currently looking at.
func tabIDFrom(t *testing.T, body string) int64 {
	t.Helper()
	m := regexp.MustCompile(`/tabs/(\d+)/payments`).FindStringSubmatch(body)
	if len(m) < 2 {
		t.Fatalf("no tab id in the rendered page")
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("tab id %q: %v", m[1], err)
	}
	return id
}

// createScheduledTab makes a tab whose schedule is already overdue, so the very
// next read has something to post.
func (h *harness) createScheduledTab(anchor schedule.Date, kind schedule.Kind, billing schedule.Billing) (int64, string) {
	h.t.Helper()
	form := url.Values{
		"csrf_token":       {h.csrfToken("/tabs/new")},
		"name":             {"Lawn service"},
		"description":      {"Every other week"},
		"item_name":        {"Mowing", "Edging"},
		"item_amount":      {"60.00", "15.00"},
		"schedule_kind":    {string(kind)},
		"schedule_anchor":  {anchor.String()},
		"schedule_billing": {string(billing)},
	}
	resp, body := h.post("/tabs", form)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("create scheduled tab returned %d: %s", resp.StatusCode, truncate(body))
	}
	return tabIDFrom(h.t, body), body
}

// today in the instance timezone, which the harness sets to America/New_York.
func instanceToday(t *testing.T) schedule.Date {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	return schedule.DateOf(time.Now().In(loc))
}

// The Phase 3 exit criteria, walked end to end: a tab charges itself on a
// schedule, and both parties can see what changed.
func TestPhase3Recurrence(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	// A schedule anchored ten weeks back, billed weekly in advance. Creating
	// the tab posts nothing by itself -- the read that follows does (SCHED-03).
	today := instanceToday(t)
	anchor := today.AddDays(-70)
	tabID, body := h.createScheduledTab(anchor, schedule.Weekly, schedule.InAdvance)

	// The tab page states its own schedule and when the next charge lands.
	if !strings.Contains(body, "Weekly on") {
		t.Errorf("tab page does not state its schedule: %s", truncate(body))
	}
	if !strings.Contains(body, "next charge") {
		t.Errorf("tab page does not say when the next charge lands (SCHED-05)")
	}

	// Eleven cycles have come due: the anchor plus ten weeks. Each posts once.
	periods, err := h.db.ListPostedPeriods(t.Context(), tabID)
	if err != nil {
		t.Fatalf("list periods: %v", err)
	}
	const wantCycles = 11
	if len(periods) != wantCycles {
		t.Fatalf("posted %d cycles on first read, want %d", len(periods), wantCycles)
	}

	// $75 a cycle, eleven cycles.
	balance, err := h.db.SumEntries(t.Context(), tabID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if want := int64(-7500 * wantCycles); int64(balance) != want {
		t.Fatalf("balance %s after catch-up, want %d cents", balance, want)
	}

	// The page said so rather than silently moving the number (SCHED-03).
	if !strings.Contains(body, "periods came due and were posted just now") {
		t.Errorf("catch-up was not explained on the page: %s", truncate(body))
	}

	// Reading again posts nothing more. This is the "exactly once" criterion
	// as a user experiences it: refreshing does not bill you again.
	_, body = h.get(tabPath(tabID))
	after, err := h.db.ListPostedPeriods(t.Context(), tabID)
	if err != nil {
		t.Fatalf("list periods: %v", err)
	}
	if len(after) != wantCycles {
		t.Fatalf("a second read posted more cycles: %d, want %d", len(after), wantCycles)
	}
	if strings.Contains(body, "periods came due and were posted just now") {
		t.Errorf("a quiet read claimed to have posted cycles")
	}

	// A period statement renders the charge, its breakdown, its due date, and
	// what has been paid against it (CHG-04).
	if !strings.Contains(body, "Periods") {
		t.Errorf("no period statements on the tab page")
	}
	for _, want := range []string{"Due ", "Mowing", "Edging", "outstanding"} {
		if !strings.Contains(body, want) {
			t.Errorf("period statement missing %q", want)
		}
	}

	// Paying settles the oldest period first, and the statement says so.
	form := url.Values{
		"csrf_token": {h.csrfToken(tabPath(tabID))},
		"amount":     {"75.00"},
		"method":     {string(store.MethodTransfer)},
	}
	resp, body := h.post(tabPath(tabID)+"/payments", form)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("payment returned %d: %s", resp.StatusCode, truncate(body))
	}
	if !strings.Contains(body, "paid") {
		t.Errorf("no period reads as paid after covering one cycle: %s", truncate(body))
	}
}

// Changing an item's amount takes effect next period and leaves posted entries
// untouched (CHG-02).
func TestItemChangeLeavesPostedPeriodsAlone(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	// Anchored today, monthly, so exactly one cycle has posted.
	today := instanceToday(t)
	tabID, body := h.createScheduledTab(today, schedule.MonthlyDay, schedule.InAdvance)

	periods, err := h.db.ListPostedPeriods(t.Context(), tabID)
	if err != nil {
		t.Fatalf("list periods: %v", err)
	}
	if len(periods) != 1 {
		t.Fatalf("posted %d cycles, want 1", len(periods))
	}
	postedSeq := periods[0].EntrySeq

	before, err := h.db.GetEntry(t.Context(), postedSeq)
	if err != nil {
		t.Fatalf("get entry: %v", err)
	}
	if before.Amount != -7500 {
		t.Fatalf("first cycle charged %s, want -75.00", before.Amount)
	}

	// The provider raises the mowing price.
	items, err := h.db.ListItems(t.Context(), tabID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var mowing store.TabItem
	for _, it := range items {
		if it.Name == "Mowing" {
			mowing = it
		}
	}
	if mowing.ID == 0 {
		t.Fatal("could not find the Mowing item")
	}

	form := url.Values{
		"csrf_token": {h.csrfToken(tabPath(tabID))},
		"name":       {"Mowing"},
		"amount":     {"80.00"},
	}
	resp, body := h.post(tabPath(tabID)+"/items/"+strconv.FormatInt(mowing.ID, 10), form)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("item update returned %d: %s", resp.StatusCode, truncate(body))
	}

	// The posted cycle is unchanged, amount and breakdown both.
	after, err := h.db.GetEntry(t.Context(), postedSeq)
	if err != nil {
		t.Fatalf("re-read entry: %v", err)
	}
	if after.Amount != before.Amount {
		t.Errorf("a posted cycle changed from %s to %s after an item edit", before.Amount, after.Amount)
	}
	snapshot, err := h.db.ListEntryItems(t.Context(), postedSeq)
	if err != nil {
		t.Fatalf("entry items: %v", err)
	}
	for _, it := range snapshot {
		if it.Name == "Mowing" && it.Amount != 6000 {
			t.Errorf("the posted breakdown now reads %s for Mowing, want the 60.00 it was billed at", it.Amount)
		}
	}

	// And the tab's own per-period total reflects the new price going forward.
	if !strings.Contains(body, "$95.00") {
		t.Errorf("the per-period total does not reflect the change: %s", truncate(body))
	}

	// Superseding, not overwriting: the old row is still there, dated.
	history, err := h.db.ListItemHistory(t.Context(), tabID)
	if err != nil {
		t.Fatalf("item history: %v", err)
	}
	if len(history) != 3 {
		t.Errorf("item history holds %d rows, want 3 -- an edit should supersede", len(history))
	}
}

// A tab with no schedule keeps working exactly as it did in Phase 2. Billing
// by hand stays a legitimate way to run one (CHG-03).
func TestUnscheduledTabStillWorks(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	form := url.Values{
		"csrf_token":  {h.csrfToken("/tabs/new")},
		"name":        {"Odd jobs"},
		"item_name":   {"Labour"},
		"item_amount": {"20.00"},
		// No schedule fields at all.
	}
	resp, body := h.post("/tabs", form)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create returned %d: %s", resp.StatusCode, truncate(body))
	}
	tabID := tabIDFrom(t, body)

	if !strings.Contains(body, "No schedule") {
		t.Errorf("an unscheduled tab does not say so: %s", truncate(body))
	}

	periods, err := h.db.ListPostedPeriods(t.Context(), tabID)
	if err != nil {
		t.Fatalf("list periods: %v", err)
	}
	if len(periods) != 0 {
		t.Errorf("an unscheduled tab posted %d cycles", len(periods))
	}
	balance, err := h.db.SumEntries(t.Context(), tabID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if balance != 0 {
		t.Errorf("an unscheduled tab has a balance of %s, want zero", balance)
	}
}

// A schedule can be added to an existing tab, and removed again.
func TestScheduleCanBeSetAndCleared(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	form := url.Values{
		"csrf_token":  {h.csrfToken("/tabs/new")},
		"name":        {"Storage unit"},
		"item_name":   {"Unit 12"},
		"item_amount": {"50.00"},
	}
	_, body := h.post("/tabs", form)
	tabID := tabIDFrom(t, body)

	// Add a schedule anchored today.
	today := instanceToday(t)
	form = url.Values{
		"csrf_token":       {h.csrfToken(tabPath(tabID))},
		"schedule_kind":    {string(schedule.MonthlyDay)},
		"schedule_anchor":  {today.String()},
		"schedule_billing": {string(schedule.InAdvance)},
	}
	resp, body := h.post(tabPath(tabID)+"/schedule", form)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set schedule returned %d: %s", resp.StatusCode, truncate(body))
	}
	if !strings.Contains(body, "Monthly on the") {
		t.Errorf("schedule not shown after being set: %s", truncate(body))
	}

	// Setting it does not bill; the read that follows does, and it did.
	periods, err := h.db.ListPostedPeriods(t.Context(), tabID)
	if err != nil {
		t.Fatalf("list periods: %v", err)
	}
	if len(periods) != 1 {
		t.Fatalf("posted %d cycles after setting a schedule anchored today, want 1", len(periods))
	}

	// Clear it. The cycle already posted stays posted -- clearing a schedule
	// must not un-bill anything.
	form = url.Values{
		"csrf_token":    {h.csrfToken(tabPath(tabID))},
		"schedule_kind": {""},
	}
	resp, body = h.post(tabPath(tabID)+"/schedule", form)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear schedule returned %d: %s", resp.StatusCode, truncate(body))
	}
	if !strings.Contains(body, "No schedule") {
		t.Errorf("schedule not cleared: %s", truncate(body))
	}

	still, err := h.db.ListPostedPeriods(t.Context(), tabID)
	if err != nil {
		t.Fatalf("list periods: %v", err)
	}
	if len(still) != 1 {
		t.Errorf("clearing the schedule changed the posted cycles: %d, want 1", len(still))
	}
	balance, err := h.db.SumEntries(t.Context(), tabID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if balance != -5000 {
		t.Errorf("balance %s after clearing a schedule, want the -50.00 already billed", balance)
	}
}

// A malformed schedule is refused with a message rather than a 500 or a tab
// that silently never bills.
func TestScheduleRejectsBadInput(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	form := url.Values{
		"csrf_token":  {h.csrfToken("/tabs/new")},
		"name":        {"Bad schedule"},
		"item_name":   {"Thing"},
		"item_amount": {"10.00"},
	}
	_, body := h.post("/tabs", form)
	tabID := tabIDFrom(t, body)

	today := instanceToday(t)
	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{
			name: "unknown recurrence",
			form: url.Values{"schedule_kind": {"quarterly"}, "schedule_anchor": {today.String()}},
			want: "not a recurrence",
		},
		{
			name: "missing anchor",
			form: url.Values{"schedule_kind": {string(schedule.Weekly)}, "schedule_anchor": {""}},
			want: "needs a start date",
		},
		{
			name: "impossible anchor",
			form: url.Values{"schedule_kind": {string(schedule.Weekly)}, "schedule_anchor": {"2026-02-31"}},
			want: "not a real date",
		},
		{
			name: "anchor far in the past",
			form: url.Values{"schedule_kind": {string(schedule.Weekly)}, "schedule_anchor": {"1970-01-01"}},
			want: "within about five years",
		},
		{
			name: "unknown billing rule",
			form: url.Values{
				"schedule_kind":    {string(schedule.Weekly)},
				"schedule_anchor":  {today.String()},
				"schedule_billing": {"eventually"},
			},
			want: "start or the end",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{"csrf_token": {h.csrfToken(tabPath(tabID))}}
			for k, v := range tc.form {
				form[k] = v
			}
			resp, body := h.post(tabPath(tabID)+"/schedule", form)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("returned %d, want the tab page with a message", resp.StatusCode)
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("message did not mention %q: %s", tc.want, truncate(body))
			}
			// And nothing was billed.
			periods, err := h.db.ListPostedPeriods(t.Context(), tabID)
			if err != nil {
				t.Fatalf("list periods: %v", err)
			}
			if len(periods) != 0 {
				t.Errorf("a refused schedule still posted %d cycles", len(periods))
			}
		})
	}
}

// AUTH-05 extends to the Phase 3 routes: only the tab's Provider may change its
// schedule or its items, and a non-participant cannot even tell the tab exists.
func TestScheduleAndItemRoutesAreProviderOnly(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	// The provider's tab, and one of its items.
	today := instanceToday(t)
	tabID, _ := h.createScheduledTab(today, schedule.MonthlyDay, schedule.InAdvance)
	items, err := h.db.ListItems(t.Context(), tabID)
	if err != nil || len(items) == 0 {
		t.Fatalf("list items: %v", err)
	}
	itemID := strconv.FormatInt(items[0].ID, 10)

	// A second account, with no connection to that tab.
	h.addUser("sam@example.com", "Sam Payee", false)
	h.loginAs("sam@example.com", "a-long-enough-password")

	paths := []string{
		tabPath(tabID) + "/schedule",
		tabPath(tabID) + "/items",
		tabPath(tabID) + "/items/" + itemID,
		tabPath(tabID) + "/items/" + itemID + "/remove",
	}
	for _, path := range paths {
		resp, _ := h.post(path, url.Values{
			"csrf_token": {h.csrfToken("/")},
			"name":       {"Injected"},
			"amount":     {"999.00"},
			// A valid schedule, so a refusal is authorization and not validation.
			"schedule_kind":    {string(schedule.Weekly)},
			"schedule_anchor":  {today.String()},
			"schedule_billing": {string(schedule.InAdvance)},
		})
		// 404 rather than 403, so tab ids cannot be enumerated.
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s as a non-participant returned %d, want 404", path, resp.StatusCode)
		}
	}

	// Nothing the outsider sent reached the tab.
	stillItems, err := h.db.ListItems(t.Context(), tabID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(stillItems) != len(items) {
		t.Errorf("a non-participant changed the tab's items")
	}
	tab, err := h.db.GetTab(t.Context(), tabID)
	if err != nil {
		t.Fatalf("get tab: %v", err)
	}
	if tab.Schedule.Kind != schedule.MonthlyDay {
		t.Errorf("a non-participant changed the tab's schedule to %q", tab.Schedule.Kind)
	}
}
