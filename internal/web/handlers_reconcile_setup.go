package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/reconcile"
	"github.com/johnzastrow/bitt/internal/store"
	"github.com/johnzastrow/bitt/internal/web/views"
)

// Reconciliation setup controls, ignore rules, and suggestions (RECON-03).
// Every handler here sits behind requireReconcile.

// buildSuggestions runs matching over every open line and the payments around
// them, and joins the result back to the rows for display.
func (s *Server) buildSuggestions(ctx context.Context, set reconcile.Settings) (views.Suggestions, error) {
	var out views.Suggestions
	lines, err := s.store.ListOpenBankLines(ctx)
	if err != nil || len(lines) == 0 {
		return out, err
	}

	loc := s.location(ctx)
	var first, last time.Time
	rl := make([]reconcile.Line, 0, len(lines))
	byLine := map[int64]store.OpenBankLine{}
	for _, l := range lines {
		d, err := time.Parse("2006-01-02", l.PostedOn)
		if err != nil {
			return out, err
		}
		if first.IsZero() || d.Before(first) {
			first = d
		}
		if d.After(last) {
			last = d
		}
		rl = append(rl, reconcile.Line{ID: l.ID, PostedOn: d, Amount: l.Amount,
			Text: l.Description + "\n" + l.Note})
		byLine[l.ID] = l
	}

	// Payments around the lines: the earliest date less the window, to the
	// latest plus the window, as whole days in the instance's timezone.
	w := set.DateWindowDays
	from := civil(first.AddDate(0, 0, -w), loc)
	to := civil(last.AddDate(0, 0, w+1), loc)
	pays, err := s.store.ListPaymentCandidates(ctx, from, to)
	if err != nil {
		return out, err
	}
	rp := make([]reconcile.Payment, 0, len(pays))
	byPay := map[int64]store.PaymentCandidate{}
	for _, p := range pays {
		eff := p.EffectiveAt.In(loc)
		rp = append(rp, reconcile.Payment{
			Seq: p.Seq, TabID: p.TabID, Amount: p.Amount, Method: string(p.Method),
			EffectiveOn: time.Date(eff.Year(), eff.Month(), eff.Day(), 0, 0, 0, 0, time.UTC),
			Names:       append([]string{p.TabName}, p.Participants...),
		})
		// Shown in the instance's timezone, the same day matching used.
		p.EffectiveAt = eff
		byPay[p.Seq] = p
	}

	res := reconcile.Suggest(rl, rp, set)
	for _, sg := range res.Suggestions {
		out.Matches = append(out.Matches, views.SuggestedMatch{
			Line: byLine[sg.LineID], Payment: byPay[sg.PaymentSeq], Suggestion: sg,
			DateFlagged: abs(sg.DateDiff) > set.DateFlagDays,
		})
	}
	for _, t := range res.Ties {
		var tie views.SuggestedTie
		for _, id := range t.LineIDs {
			tie.Lines = append(tie.Lines, byLine[id])
		}
		for _, seq := range t.PaymentSeqs {
			tie.Payments = append(tie.Payments, byPay[seq])
		}
		out.Ties = append(out.Ties, tie)
	}
	for _, id := range res.UnmatchedLines {
		out.UnmatchedLines = append(out.UnmatchedLines, byLine[id])
	}
	out.Small = len(res.Small)
	for _, seq := range res.UnmatchedPayments {
		out.UnmatchedPayments = append(out.UnmatchedPayments, byPay[seq])
	}
	return out, nil
}

// civil is midnight of a date's calendar day in loc, as an instant.
func civil(d time.Time, loc *time.Location) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc).UTC()
}

// postReconcileSettings saves the setup controls. Every field is required;
// a value out of range refuses the whole save, with which one and why.
func (s *Server) postReconcileSettings(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	const back = "/admin/reconcile#setup"
	if err := r.ParseForm(); err != nil || !auth.CheckCSRF(r) {
		redirectWith(w, r, back, "err", "Your session expired. Please try again.")
		return
	}
	set, err := settingsFromForm(r)
	if err == nil {
		err = set.Validate()
	}
	if err != nil {
		redirectWith(w, r, back, "err", "Setup not saved: "+err.Error()+".")
		return
	}
	before, err := s.store.GetReconcileSettings(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.store.SetReconcileSettings(r.Context(), set, user.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("reconciliation setup changed", "by_user_id", user.ID,
		"before", settingsLog(before.Settings), "after", settingsLog(set))
	redirectWith(w, r, back, "ok", "Reconciliation setup saved.")
}

// settingsLog is a compact record of the controls for the change log.
func settingsLog(s reconcile.Settings) string {
	return "window=" + strconv.Itoa(s.DateWindowDays) + "d pct=" + strconv.Itoa(s.TolerancePercent) +
		" cap=" + s.ToleranceCap.String() + " floor=" + s.ToleranceFloor.String() +
		" min=" + s.MinLineAmount.String() + " names=" + strconv.FormatBool(s.UseNames) +
		" flag=" + strconv.Itoa(s.DateFlagDays) + "d"
}

func settingsFromForm(r *http.Request) (reconcile.Settings, error) {
	var (
		s   reconcile.Settings
		err error
	)
	ints := []struct {
		field, label string
		dst          *int
	}{
		{"date_window_days", "date window", &s.DateWindowDays},
		{"tolerance_percent", "tolerance percent", &s.TolerancePercent},
		{"date_flag_days", "date flag", &s.DateFlagDays},
	}
	for _, f := range ints {
		*f.dst, err = strconv.Atoi(strings.TrimSpace(r.PostFormValue(f.field)))
		if err != nil {
			return s, errors.New("the " + f.label + " must be a whole number")
		}
	}
	amounts := []struct {
		field, label string
		dst          *money.Cents
	}{
		{"tolerance_cap", "tolerance cap", &s.ToleranceCap},
		{"tolerance_floor", "tolerance floor", &s.ToleranceFloor},
		{"min_line_amount", "minimum line amount", &s.MinLineAmount},
	}
	for _, f := range amounts {
		*f.dst, err = money.Parse(r.PostFormValue(f.field))
		if err != nil {
			return s, errors.New("the " + f.label + " must be an amount such as 25.00")
		}
	}
	s.UseNames = r.PostFormValue("use_names") == "1"
	return s, nil
}

// postIgnoreRule adds "description contains ..." for one layout, and ignores
// that layout's open lines that match it.
func (s *Server) postIgnoreRule(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	const back = "/admin/reconcile#setup"
	if err := r.ParseForm(); err != nil || !auth.CheckCSRF(r) {
		redirectWith(w, r, back, "err", "Your session expired. Please try again.")
		return
	}
	formatID, err := strconv.ParseInt(r.PostFormValue("format_id"), 10, 64)
	if err != nil {
		redirectWith(w, r, back, "err", "Choose the layout the rule is for.")
		return
	}
	if _, err := s.store.GetBankFormat(r.Context(), formatID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			redirectWith(w, r, back, "err", "Choose the layout the rule is for.")
			return
		}
		s.serverError(w, r, err)
		return
	}
	contains := strings.TrimSpace(r.PostFormValue("contains"))
	if contains == "" || len([]rune(contains)) > 120 || strings.ContainsAny(contains, "\r\n") {
		redirectWith(w, r, back, "err", "Give the text to look for, up to 120 characters on one line.")
		return
	}

	rule, n, err := s.store.AddIgnoreRule(r.Context(), store.IgnoreRule{
		FormatID: formatID, Contains: contains, CreatedBy: user.ID,
	})
	if errors.Is(err, store.ErrConflict) {
		redirectWith(w, r, back, "err", "That rule already exists for this layout.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("ignore rule added", "rule_id", rule.ID, "format_id", formatID,
		"contains", contains, "lines_ignored", n, "by_user_id", user.ID)
	redirectWith(w, r, back, "ok", "Rule added; "+strconv.Itoa(n)+" open lines now ignored.")
}

func (s *Server) postIgnoreRuleDelete(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	const back = "/admin/reconcile#setup"
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil || !auth.CheckCSRF(r) {
		redirectWith(w, r, back, "err", "Your session expired. Please try again.")
		return
	}
	err := s.store.DeleteIgnoreRule(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("ignore rule removed", "rule_id", id, "by_user_id", user.ID)
	redirectWith(w, r, back, "ok", "Rule removed. Lines it ignored stay ignored.")
}
