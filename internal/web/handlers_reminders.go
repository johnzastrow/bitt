package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/johnzastrow/bitt/internal/notify"
	"github.com/johnzastrow/bitt/internal/store"
)

// maxTabReminders bounds how many lead times one tab may carry. Six is already
// more notice than any payment needs, and the bound is what stops a pasted list
// from turning one due date into a hundred messages.
const maxTabReminders = 6

// postTabReminders replaces a tab's own payment reminders (the per-tab override
// of the instance-wide defaults).
//
// Provider-only, like every other tab setting: the Provider owns the billing
// cadence, so the Provider says when its payees hear about it. An empty days
// field clears the tab's reminders and returns it to the instance defaults,
// which is the only way back and so must stay easy.
//
// The templates saved here are the first USER-controlled text to reach a mail
// header, via {tab} in a Subject. They are validated on the way in by
// internal/notify -- the same header-safety rule the sender applies -- so a
// template that would fail every send closed cannot be saved in the first
// place. The sender still checks at delivery, because the substituted values
// are not known here.
func (s *Server) postTabReminders(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	access, ok := s.requireTabManager(w, r, id, user,
		"Only the provider can change this tab's reminders.")
	if !ok {
		return
	}

	rs, err := parseTabReminders(
		r.PostFormValue("reminder_days"),
		r.PostFormValue("reminder_title"),
		r.PostFormValue("reminder_body"),
	)
	if err != nil {
		redirectWith(w, r, tabPath(id), "err", err.Error())
		return
	}

	if err := s.store.SetTabReminders(r.Context(), access.Tab.ID, rs); err != nil {
		s.serverError(w, r, err)
		return
	}

	s.log.Info("tab reminders set", "tab_id", access.Tab.ID, "user_id", user.ID,
		"as_admin", access.Admin, "count", len(rs))
	if len(rs) == 0 {
		redirectWith(w, r, tabPath(id), "ok",
			"Reminders cleared. This tab uses the instance defaults again.")
		return
	}
	redirectWith(w, r, tabPath(id), "ok", remindersSetNote(rs))
}

// parseTabReminders validates the reminder form into the set to store.
//
// An empty days list clears the tab, whatever the templates say -- "remind on
// no days" is how a Provider hands the tab back to the instance defaults, and
// demanding a title to express that would be a trap.
func parseTabReminders(rawDays, title, body string) ([]store.TabReminder, error) {
	days, err := parseReminderDays(rawDays)
	if err != nil {
		return nil, err
	}
	if len(days) == 0 {
		return nil, nil
	}

	title = strings.TrimSpace(title)
	// A <textarea> submits its line endings as CRLF, per the HTML form spec.
	// That is a transport artifact, not something the Provider typed, so it is
	// normalised before validation -- otherwise every multi-line message written
	// in a browser would be refused for containing a control character, which is
	// exactly what the first visual test of this form did.
	body = strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	// Size and shape are reported separately, because they are different
	// mistakes with different fixes and a message covering both tells the
	// Provider neither.
	if len(title) > notify.MaxTitleTemplate {
		return nil, fmt.Errorf("The reminder title is %d bytes. The limit is %d.",
			len(title), notify.MaxTitleTemplate)
	}
	if !notify.ValidTitleTemplate(title) {
		return nil, errors.New(
			"The reminder title must be one line of text, with no line breaks and nothing left blank.")
	}
	if len(body) > notify.MaxBodyTemplate {
		return nil, fmt.Errorf(
			"The reminder message is %d bytes. A free ntfy.sh account refuses anything over %d, so that is the limit here.",
			len(body), notify.MaxBodyTemplate)
	}
	if !notify.ValidBodyTemplate(body) {
		return nil, errors.New(
			"The reminder message cannot be blank or contain control characters.")
	}

	// The same message on every lead time, which is how the instance default
	// behaves. The table stores it per row, so a future interface can let the
	// one-day notice read differently from the two-week one without a
	// migration.
	out := make([]store.TabReminder, 0, len(days))
	for _, d := range days {
		out = append(out, store.TabReminder{Days: d, Title: title, Body: body})
	}
	return out, nil
}

// parseReminderDays reads a comma-separated lead-time list into sorted,
// deduplicated days, longest first -- the order they fire in. It mirrors the
// validation on BITT_REMINDER_DAYS, because the two configure the same thing.
func parseReminderDays(raw string) ([]int, error) {
	seen := map[int]bool{}
	var days []int
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		// A negative value is an overdue notice, fired that many days AFTER the
		// due date. Zero is refused because "on the due date" is ambiguous
		// between the two and nobody would agree which they meant.
		n, err := strconv.Atoi(p)
		if err != nil || n == 0 || n < -3650 || n > 3650 {
			return nil, errors.New(
				"Reminder days must be a comma-separated list of whole days, like \"14, 7, 1\". " +
					"A negative number is an overdue notice sent that many days after the due date, like \"-1, -7\".")
		}
		if !seen[n] {
			seen[n] = true
			days = append(days, n)
		}
	}
	if len(days) > maxTabReminders {
		return nil, fmt.Errorf("A tab can have at most %d reminders. That list has %d.",
			maxTabReminders, len(days))
	}
	// Longest lead first, so the stored order is the order they fire in
	// regardless of how they were typed.
	for i := 1; i < len(days); i++ {
		for j := i; j > 0 && days[j] > days[j-1]; j-- {
			days[j], days[j-1] = days[j-1], days[j]
		}
	}
	return days, nil
}

// remindersSetNote reports what was saved, naming the days so the Provider can
// see the list was read the way they meant it.
func remindersSetNote(rs []store.TabReminder) string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, strconv.Itoa(r.Days))
	}
	what := "reminders"
	if len(rs) == 1 {
		what = "reminder"
	}
	return fmt.Sprintf("Saved. This tab sends its own %s, %s days before each due date.",
		what, strings.Join(parts, ", "))
}
