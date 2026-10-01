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

type matchStore interface {
	ConfirmBankMatch(ctx context.Context, m store.NewBankMatch, delta *store.NewEntry) (store.BankMatch, bool, error)
	UndoBankMatch(ctx context.Context, matchID, by int64, reversal *store.NewEntry) error
	GetBankMatch(ctx context.Context, id int64) (store.BankMatch, error)
	EntryHasActiveMatch(ctx context.Context, seq int64) (bool, error)
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
	desc := oneLine(c.Description)
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
		return memo
	}
	return memo + "\nBank note: " + c.Note
}

// UndoMatch undoes a standing match: its delta (if any) is reversed with the
// normal reversal, the match is marked undone, and the line and payment are
// unmatched again -- one transaction.
func (s *Service) UndoMatch(ctx context.Context, matchID, actorUserID int64) error {
	if s.matches == nil {
		return ErrNoMatchStore
	}
	m, err := s.matches.GetBankMatch(ctx, matchID)
	if err != nil {
		return err
	}
	if !m.Active() {
		return store.ErrMatchUndone
	}
	var reversal *store.NewEntry
	if m.DeltaEntrySeq != nil {
		delta, err := s.store.GetEntry(ctx, *m.DeltaEntrySeq)
		if err != nil {
			return err
		}
		seq := delta.Seq
		reversal = &store.NewEntry{
			TabID: delta.TabID, Kind: store.KindReversal, Amount: delta.Amount.Neg(),
			Memo:        fmt.Sprintf("Bank reconciliation undone (match %d)", m.ID),
			EffectiveAt: s.now(), ActorUserID: actorUserID, ReversesSeq: &seq, Method: store.MethodNone,
		}
	}
	err = s.matches.UndoBankMatch(ctx, matchID, actorUserID, reversal)
	if errors.Is(err, store.ErrConflict) {
		return ErrAlreadyReversed
	}
	return err
}

// oneLine joins a multi-line bank description for a memo.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
