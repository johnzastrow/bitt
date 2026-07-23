package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/johnzastrow/bitt/internal/fee"
	"github.com/johnzastrow/bitt/internal/ledger"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
)

// maxGraceDays and maxCap bound the fee inputs so a typo cannot configure a
// grace of a thousand years or a cap larger than any real debt.
const (
	maxGraceDays = 365
	maxPercentBP = 10000 // 100%
)

// postFeePolicy sets or clears a tab's late-fee policy (FEE-01, FEE-02, FEE-06).
//
// Nothing here assesses a fee. The next read of the tab does that, through the
// same lazy path charges and periods use (FEE-03), so a policy set on an
// already-overdue tab assesses the catch-up on the following read.
func (s *Server) postFeePolicy(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	access, ok := s.requireTabManager(w, r, id, user, "Only the provider can change this tab's late fee.")
	if !ok {
		return
	}

	policy, err := parseFeePolicy(r)
	if err != nil {
		redirectWith(w, r, tabPath(id), "err", err.Error())
		return
	}

	if err := s.store.SetFeePolicy(r.Context(), access.Tab.ID, policy); err != nil {
		s.serverError(w, r, err)
		return
	}

	s.log.Info("fee policy set", "tab_id", access.Tab.ID, "user_id", user.ID,
		"as_admin", access.Admin, "kind", string(policy.Kind))
	if !policy.Set() {
		redirectWith(w, r, tabPath(id), "ok", "Late fee removed.")
		return
	}
	redirectWith(w, r, tabPath(id), "ok", policy.Describe()+".")
}

// parseFeePolicy reads the fee fields from a submitted form. An empty kind
// clears the policy, which is a legitimate choice (no late fee).
func parseFeePolicy(r *http.Request) (fee.Policy, error) {
	kind := fee.Kind(strings.TrimSpace(r.PostFormValue("fee_kind")))
	if kind == fee.None {
		return fee.Policy{}, nil
	}
	if !kind.Valid() {
		return fee.Policy{}, errors.New("That is not a late-fee kind BitTabby offers.")
	}

	p := fee.Policy{Kind: kind}

	grace, err := strconv.Atoi(strings.TrimSpace(orZero(r.PostFormValue("fee_grace_days"))))
	if err != nil || grace < 0 || grace > maxGraceDays {
		return fee.Policy{}, errors.New("Grace days must be a whole number between 0 and 365.")
	}
	p.GraceDays = grace

	switch kind {
	case fee.Fixed:
		amount, err := money.Parse(r.PostFormValue("fee_fixed"))
		if err != nil || amount <= 0 {
			return fee.Policy{}, errors.New("A fixed late fee needs a dollar amount greater than zero.")
		}
		p.Fixed = amount
	case fee.Percent:
		bp, err := parsePercentBP(r.PostFormValue("fee_percent"))
		if err != nil {
			return fee.Policy{}, err
		}
		p.PercentBP = bp
	}

	if raw := strings.TrimSpace(r.PostFormValue("fee_cap")); raw != "" {
		capAmount, err := money.Parse(raw)
		if err != nil || capAmount < 0 {
			return fee.Policy{}, errors.New("The cap must be a dollar amount, or blank for no cap.")
		}
		p.Cap = capAmount
	}

	if err := p.Validate(); err != nil {
		return fee.Policy{}, errors.New("That late-fee setting is not usable.")
	}
	return p, nil
}

// parsePercentBP reads a percentage like "5" or "2.5" into basis points,
// without routing through a float.
func parsePercentBP(raw string) (int64, error) {
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "%"))
	if raw == "" {
		return 0, errors.New("A percentage late fee needs a rate.")
	}
	// Reuse the money parser: a percentage with up to two decimals is the same
	// shape as a dollar amount with up to two decimals, and basis points are to
	// a percent what cents are to a dollar.
	bp, err := money.Parse(raw)
	if err != nil || bp <= 0 || int64(bp) > maxPercentBP {
		return 0, errors.New("The rate must be a percentage between 0 and 100.")
	}
	return int64(bp), nil
}

func orZero(s string) string {
	if strings.TrimSpace(s) == "" {
		return "0"
	}
	return s
}

// maxInterestAPRBp bounds the interest rate so a typo cannot configure a
// thousand-percent loan. 100% APR is already far beyond any real family loan.
const maxInterestAPRBp = 10000

// postInterestRate sets or clears a Payoff loan's annual interest rate.
//
// Nothing here charges interest. The next read of the tab accrues it, through
// the same lazy path charges and fees use, so a rate set on an existing loan
// begins accruing from its next period.
func (s *Server) postInterestRate(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	access, ok := s.requireTabManager(w, r, id, user, "Only the provider can change this loan's interest.")
	if !ok {
		return
	}

	bp, err := parseInterestBP(r.PostFormValue("interest_apr"))
	if err != nil {
		redirectWith(w, r, tabPath(id), "err", err.Error())
		return
	}

	if err := s.store.SetInterestRate(r.Context(), access.Tab.ID, bp); err != nil {
		s.serverError(w, r, err)
		return
	}

	s.log.Info("interest rate set", "tab_id", access.Tab.ID, "user_id", user.ID,
		"as_admin", access.Admin, "apr_bp", bp)
	if bp == 0 {
		redirectWith(w, r, tabPath(id), "ok", "Interest removed. This loan carries no interest now.")
		return
	}
	redirectWith(w, r, tabPath(id), "ok", fee.PercentString(bp)+" annual interest set.")
}

// parseInterestBP reads an APR like "6" or "6.5" into basis points. Empty or
// zero clears the rate.
func parseInterestBP(raw string) (int64, error) {
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "%"))
	if raw == "" {
		return 0, nil
	}
	// A percentage with up to two decimals maps to basis points the same way a
	// dollar amount maps to cents.
	v, err := money.Parse(raw)
	if err != nil || v < 0 || int64(v) > maxInterestAPRBp {
		return 0, errors.New("Interest must be an annual percentage between 0 and 100.")
	}
	return int64(v), nil
}

// ---------------------------------------------------------------------------
// Payoff progress (PAYOFF-01, PAYOFF-02, PAYOFF-03)
// ---------------------------------------------------------------------------

// payoffFor computes a Payoff tab's derived state. It is meaningful only for
// Payoff tabs; callers guard on kind.
func (s *Server) payoffFor(r *http.Request, tab store.Tab) (ledger.Payoff, error) {
	entries, err := s.ledger.History(r.Context(), tab.ID)
	if err != nil {
		return ledger.Payoff{}, err
	}
	items, err := s.store.ListItems(r.Context(), tab.ID)
	if err != nil {
		return ledger.Payoff{}, err
	}
	installment, ok := money.Sum(itemAmounts(items))
	if !ok {
		return ledger.Payoff{}, errors.New("installment total overflow")
	}
	return ledger.ComputePayoff(tab, entries, installment, s.today(r.Context())), nil
}
