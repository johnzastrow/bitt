package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
)

// Bank reconciliation writes (SPEC-BANK-RECONCILE, RECON-04).
//
// This is the documented exception to AUTH-05's administrator rule: an
// administrator holding Can reconcile may post the difference between a bank
// line and a recorded payment, on a tab they are not on. The exception lives
// here, narrowed to entries backed by a bank line and tied to a match row in
// the same transaction; the caller (the web layer) checks the permission.

// MatchStore is the store's reconciliation half. It is exported so the store
// package can assert at compile time that it satisfies it: New detects it with
// a type assertion, and a method renamed on one side only would otherwise
// leave the ledger silently without it.
type MatchStore interface {
	ConfirmBankMatch(ctx context.Context, m store.NewBankMatch, delta *store.NewEntry) (store.BankMatch, bool, error)
	UnmakeBankMatch(ctx context.Context, u store.UnmakeMatch) ([]store.Entry, error)
	GetBankMatch(ctx context.Context, id int64) (store.BankMatch, error)
	ReconLinkForEntry(ctx context.Context, seq int64) (store.ReconLink, error)
	GetBankLine(ctx context.Context, id int64) (store.OpenBankLine, error)
	RecordBankLine(ctx context.Context, lineID, by int64, e store.NewEntry) (store.Entry, error)
	UnrecordBankLine(ctx context.Context, lineID, by int64, reversal store.NewEntry, note string) (store.Entry, error)
}

// ErrNoMatchStore means the store cannot record matches; fail closed.
var ErrNoMatchStore = errors.New("ledger: this store does not support bank matches")

// Confirmation is what a person confirmed: a bank line against a payment.
type Confirmation struct {
	Match store.NewBankMatch
	// BankAt is when the delta takes effect: the bank date, as an instant.
	BankAt time.Time
	// Description, FileName and Row identify the line in the memo.
	Description string
	FileName    string
	Row         int
	// Note is the bank note; it reaches the memo only when Match.NoteInMemo.
	Note string
}

// DeltaFor is what confirming will post for a bank amount B against a
// recorded amount R (spec section 8): B > R, a payment of the difference
// (money moved); B < R, a debit adjustment (money recorded but not moved);
// equal, nothing.
func DeltaFor(bank, recorded money.Cents) (store.EntryKind, money.Cents) {
	switch {
	case bank > recorded:
		return store.KindPayment, bank - recorded
	case bank < recorded:
		return store.KindAdjustment, -(recorded - bank)
	}
	return "", 0
}

// ConfirmMatch records a match and posts its delta in one transaction. The
// recorded payment is never touched, and dates are only flagged.
func (s *Service) ConfirmMatch(ctx context.Context, c Confirmation) (store.BankMatch, bool, error) {
	if s.matches == nil {
		return store.BankMatch{}, false, ErrNoMatchStore
	}
	m := c.Match
	if m.BankAmount <= 0 || m.RecordedAmount <= 0 {
		return store.BankMatch{}, false, fmt.Errorf("%w: bank %s, recorded %s", ErrNonPositive, m.BankAmount, m.RecordedAmount)
	}

	kind, amount := DeltaFor(m.BankAmount, m.RecordedAmount)
	var delta *store.NewEntry
	desc := clip(oneLine(c.Description), maxMemoDescription)
	switch kind {
	case store.KindPayment:
		memo := fmt.Sprintf("Bank reconciliation: %s (line %d of %s)", desc, c.Row, c.FileName)
		delta = &store.NewEntry{
			TabID: m.TabID, Kind: store.KindPayment, Amount: amount, Method: store.MethodTransfer,
			Memo: s.withNote(memo, c), EffectiveAt: c.BankAt, ActorUserID: m.ConfirmedBy,
		}
	case store.KindAdjustment:
		memo := fmt.Sprintf("Bank reconciliation: recorded %s, bank shows %s (%s)",
			m.RecordedAmount.Display(), m.BankAmount.Display(), desc)
		delta = &store.NewEntry{
			TabID: m.TabID, Kind: store.KindAdjustment, Amount: amount, Method: store.MethodNone,
			Memo: s.withNote(memo, c), EffectiveAt: c.BankAt, ActorUserID: m.ConfirmedBy,
		}
	default:
		// Equal amounts post nothing; the note stays on the match only.
		m.NoteInMemo = false
	}
	if delta != nil && delta.EffectiveAt.IsZero() {
		delta.EffectiveAt = s.now()
	}
	return s.matches.ConfirmBankMatch(ctx, m, delta)
}

// withNote appends the bank note when the person chose to (off by default: a
// ledger memo is permanent and seen by everyone on the tab).
func (s *Service) withNote(memo string, c Confirmation) string {
	if !c.Match.NoteInMemo || strings.TrimSpace(c.Note) == "" {
		return fitMemo(memo)
	}
	return fitMemo(memo + "\nBank note: " + c.Note)
}

// Memo bounds. A memo is VARCHAR(1000) on MariaDB, and bank text is free text
// up to 16 KB a row: unbounded, a long description or note made a confirm fail
// there (and only there). The description is shortened inside the memo so the
// "(line N of file)" provenance always survives; the full text stays on the
// bank line and the match.
const (
	maxMemo            = 1000
	maxMemoDescription = 300
)

// clip shortens s to n runes, marking the cut.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-3]) + "..."
}

func fitMemo(s string) string { return clip(s, maxMemo) }

// Reasons recorded on an unmade match.
const (
	UnmadeFromReconciliation = "undone from Reconciliation"
	UnmadePaymentUndone      = "the payment was undone on its tab"
	UnmadeDifferenceUndone   = "the difference was undone on its tab"
)

// UndoMatch unmakes a standing match from the Reconciliation screen: its
// difference (if any) is reversed, the match is marked undone with who, when
// and why, and the line and payment are unmatched again -- one transaction.
func (s *Service) UndoMatch(ctx context.Context, matchID, actorUserID int64) error {
	_, err := s.unmake(ctx, matchID, actorUserID, UnmadeFromReconciliation, nil)
	return err
}

// unmake reverses a match's difference, plus extra (the payment's own
// reversal, when the payment is what was undone), in one transaction with
// marking the match undone. It returns the posted reversals, extra first.
func (s *Service) unmake(ctx context.Context, matchID, actorUserID int64, reason string, extra *store.NewEntry) ([]store.Entry, error) {
	if s.matches == nil {
		return nil, ErrNoMatchStore
	}
	m, err := s.matches.GetBankMatch(ctx, matchID)
	if err != nil {
		return nil, err
	}
	if !m.Active() {
		return nil, store.ErrMatchUndone
	}
	var reversals []store.NewEntry
	if extra != nil {
		reversals = append(reversals, *extra)
	}
	if m.DeltaEntrySeq != nil {
		delta, err := s.store.GetEntry(ctx, *m.DeltaEntrySeq)
		if err != nil {
			return nil, err
		}
		seq := delta.Seq
		reversals = append(reversals, store.NewEntry{
			TabID: delta.TabID, Kind: store.KindReversal, Amount: delta.Amount.Neg(),
			Memo:        fmt.Sprintf("Bank reconciliation undone (match %d): %s", m.ID, reason),
			EffectiveAt: s.now(), ActorUserID: actorUserID, ReversesSeq: &seq, Method: store.MethodNone,
			IdempotencyKey: fmt.Sprintf("%s:undo:%d", store.ReconKeyPrefix, m.ID),
		})
	}
	posted, err := s.matches.UnmakeBankMatch(ctx, store.UnmakeMatch{
		MatchID: matchID, By: actorUserID, Reason: reason, Reversals: reversals,
	})
	if errors.Is(err, store.ErrConflict) {
		return nil, ErrAlreadyReversed
	}
	return posted, err
}

// reverseReconciled handles a tab-page undo of an entry reconciliation ties
// to (decided 2026-10-01): rather than refusing, it unmakes. Undoing a matched
// payment reverses it and unmakes its match; undoing a difference unmakes its
// match; undoing a payment recorded from a bank line reopens the line. Each is
// one transaction, recorded with who and when. handled is false when the
// entry has no tie.
func (s *Service) reverseReconciled(ctx context.Context, original store.Entry, reversal store.NewEntry) (store.Entry, bool, error) {
	if s.matches == nil {
		return store.Entry{}, false, nil
	}
	link, err := s.matches.ReconLinkForEntry(ctx, original.Seq)
	if err != nil {
		return store.Entry{}, true, err
	}
	switch link.Kind {
	case store.ReconMatchedPayment:
		posted, err := s.unmake(ctx, link.MatchID, reversal.ActorUserID, UnmadePaymentUndone, &reversal)
		if errors.Is(err, store.ErrMatchUndone) {
			// Unmade by someone else since the lookup: the payment is now an
			// ordinary one, and is reversed the ordinary way.
			return store.Entry{}, false, nil
		}
		if err != nil {
			return store.Entry{}, true, err
		}
		return posted[0], true, nil
	case store.ReconDelta:
		posted, err := s.unmake(ctx, link.MatchID, reversal.ActorUserID, UnmadeDifferenceUndone, nil)
		if err != nil {
			return store.Entry{}, true, err
		}
		return posted[0], true, nil
	case store.ReconRecordedLine:
		e, err := s.matches.UnrecordBankLine(ctx, link.LineID, reversal.ActorUserID, reversal, "undone on the tab")
		if errors.Is(err, store.ErrConflict) {
			err = ErrAlreadyReversed
		}
		return e, true, err
	}
	return store.Entry{}, false, nil
}

// RecordLine posts a payment recorded from an open bank line (RECON-05): the
// bank amount, on the bank date, method as given (transfer by default), and
// marks the line recorded -- one transaction. Like a delta, this is an AUTH-05
// exception backed by a bank line; the caller checks Can reconcile.
func (s *Service) RecordLine(ctx context.Context, lineID, tabID, actorUserID int64, method store.PaymentMethod,
	memo string, at time.Time) (store.Entry, error) {
	if s.matches == nil {
		return store.Entry{}, ErrNoMatchStore
	}
	if !method.Valid() || method == store.MethodNone {
		return store.Entry{}, fmt.Errorf("%w: %q", ErrBadMethod, method)
	}
	line, err := s.matches.GetBankLine(ctx, lineID)
	if err != nil {
		return store.Entry{}, err
	}
	key, err := NewIdempotencyKey()
	if err != nil {
		return store.Entry{}, err
	}
	return s.matches.RecordBankLine(ctx, lineID, actorUserID, store.NewEntry{
		TabID: tabID, Kind: store.KindPayment, Amount: line.Amount, Method: method, Memo: fitMemo(memo),
		EffectiveAt: at, ActorUserID: actorUserID,
		IdempotencyKey: fmt.Sprintf("%s:line:%d:%s", store.ReconKeyPrefix, lineID, key),
	})
}

// UnrecordLine reverses the payment a line was recorded as, from the
// Reconciliation screen, and reopens the line.
func (s *Service) UnrecordLine(ctx context.Context, lineID, actorUserID int64) error {
	if s.matches == nil {
		return ErrNoMatchStore
	}
	line, err := s.matches.GetBankLine(ctx, lineID)
	if err != nil {
		return err
	}
	if line.State != store.BankLineRecorded || line.RecordedEntrySeq == nil {
		return store.ErrLineNotOpen
	}
	e, err := s.store.GetEntry(ctx, *line.RecordedEntrySeq)
	if err != nil {
		return err
	}
	seq := e.Seq
	key, err := NewIdempotencyKey()
	if err != nil {
		return err
	}
	_, err = s.matches.UnrecordBankLine(ctx, lineID, actorUserID, store.NewEntry{
		TabID: e.TabID, Kind: store.KindReversal, Amount: e.Amount.Neg(),
		Memo:        "Bank line unrecorded from Reconciliation",
		EffectiveAt: s.now(), ActorUserID: actorUserID, ReversesSeq: &seq, Method: store.MethodNone,
		IdempotencyKey: fmt.Sprintf("%s:unrecord:%d:%s", store.ReconKeyPrefix, lineID, key),
	}, "undone from Reconciliation")
	if errors.Is(err, store.ErrConflict) {
		return ErrAlreadyReversed
	}
	return err
}

// oneLine joins a multi-line bank description for a memo.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
