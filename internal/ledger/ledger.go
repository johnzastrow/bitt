// Package ledger is the only write boundary for financial entries.
//
// LEDGER-01: no code outside this package posts to the entries table. The
// store's EntryStore interface exposes no update or delete method, and the
// database carries abort triggers as a second line of defense, but this
// package is the first: everything that moves money calls through here.
//
// Sign convention (TAB-06): a tab balance is negative when the Payee owes and
// positive when they hold a credit. Callers pass positive amounts and choose a
// verb; this package applies the sign. No caller should ever hand a negative
// amount to Charge or Payment.
package ledger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
)

// Errors returned by the ledger.
var (
	// ErrNonPositive is returned when a charge or payment amount is zero or
	// negative. A zero-amount entry is almost always a bug in the caller, and a
	// negative one means the caller is trying to apply the sign itself.
	ErrNonPositive = errors.New("ledger: amount must be positive")
	// ErrAlreadyReversed is returned when an entry has already been undone.
	ErrAlreadyReversed = errors.New("ledger: entry already reversed")
	// ErrNotReversible is returned when asked to reverse a reversal.
	ErrNotReversible = errors.New("ledger: a reversal cannot itself be reversed")
	// ErrBadMethod is returned when a payment carries an unrecognized method.
	ErrBadMethod = errors.New("ledger: unrecognized payment method")
	// ErrOverflow is returned when an item breakdown sums past int64 cents.
	// Reported rather than wrapped silently, since a wrapped total is a wrong
	// amount posted to a ledger that cannot be edited afterward.
	ErrOverflow = errors.New("ledger: item total overflows")
)

// Service posts entries and derives balances.
type Service struct {
	store store.EntryStore
	now   func() time.Time
}

// New builds a ledger over the given store.
func New(s store.EntryStore) *Service {
	return &Service{store: s, now: func() time.Time { return time.Now().UTC() }}
}

// WithClock overrides the time source. Tests use it; production does not.
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// Post describes a financial event. Amount is always positive; Kind decides
// which direction it moves the balance.
type Post struct {
	TabID       int64
	Amount      money.Cents
	Memo        string
	ActorUserID int64
	// EffectiveAt is when the event applies. Zero means now.
	EffectiveAt time.Time
	// IdempotencyKey makes a replayed submit safe. Empty means the caller did
	// not supply one and a fresh key is generated -- acceptable for
	// server-initiated posts, never for a user-submitted form.
	IdempotencyKey string
	// Items is the breakdown captured at post time (CHG-01).
	Items []store.EntryItem
	// Method records how the money actually moved. Payments only (PAY-02).
	Method store.PaymentMethod
}

// Charge records an amount the Payee owes, moving the balance negative.
func (s *Service) Charge(ctx context.Context, p Post) (store.Entry, bool, error) {
	return s.post(ctx, p, store.KindCharge, negate)
}

// Payment records money received, moving the balance toward zero or into credit.
//
// PAY-05: a payment beyond what is owed simply carries the balance positive.
// The credit then offsets the next charge automatically, because the balance is
// a sum -- no separate credit ledger or application step exists to drift.
func (s *Service) Payment(ctx context.Context, p Post) (store.Entry, bool, error) {
	if !p.Method.Valid() {
		return store.Entry{}, false, fmt.Errorf("%w: %q", ErrBadMethod, p.Method)
	}
	return s.post(ctx, p, store.KindPayment, identity)
}

// Adjustment corrects a balance outside the normal charge path (CHG-03).
// Unlike Charge and Payment it accepts a signed amount, because a correction
// legitimately moves in either direction, and the caller states which.
func (s *Service) Adjustment(ctx context.Context, p Post) (store.Entry, bool, error) {
	if p.Amount == 0 {
		return store.Entry{}, false, fmt.Errorf("%w: adjustment of zero", ErrNonPositive)
	}
	return s.write(ctx, p, store.KindAdjustment, p.Amount, nil)
}

func negate(c money.Cents) money.Cents   { return -c }
func identity(c money.Cents) money.Cents { return c }

func (s *Service) post(ctx context.Context, p Post, kind store.EntryKind, sign func(money.Cents) money.Cents) (store.Entry, bool, error) {
	if p.Amount <= 0 {
		return store.Entry{}, false, fmt.Errorf("%w: got %s", ErrNonPositive, p.Amount)
	}
	return s.write(ctx, p, kind, sign(p.Amount), nil)
}

func (s *Service) write(ctx context.Context, p Post, kind store.EntryKind, signed money.Cents, reverses *int64) (store.Entry, bool, error) {
	key := p.IdempotencyKey
	if key == "" {
		var err error
		if key, err = NewIdempotencyKey(); err != nil {
			return store.Entry{}, false, err
		}
	}
	effective := p.EffectiveAt
	if effective.IsZero() {
		effective = s.now()
	}

	return s.store.PostEntry(ctx, store.NewEntry{
		TabID:          p.TabID,
		Method:         p.Method,
		Kind:           kind,
		Amount:         signed,
		Memo:           p.Memo,
		EffectiveAt:    effective,
		ActorUserID:    p.ActorUserID,
		IdempotencyKey: key,
		ReversesSeq:    reverses,
		Items:          p.Items,
	})
}

// Reverse undoes an entry by posting its inverse (LEDGER-02). The original
// stays visible and untouched; the correction points back at it.
//
// The database carries a unique index on reverses_seq, so a double-submitted
// undo cannot post two offsetting corrections even if this check is raced.
func (s *Service) Reverse(ctx context.Context, seq int64, actorUserID int64, memo, idempotencyKey string) (store.Entry, bool, error) {
	original, err := s.store.GetEntry(ctx, seq)
	if err != nil {
		return store.Entry{}, false, err
	}
	if original.Kind == store.KindReversal {
		return store.Entry{}, false, ErrNotReversible
	}
	if memo == "" {
		memo = fmt.Sprintf("Reversal of entry %d", seq)
	}

	entry, replayed, err := s.write(ctx, Post{
		TabID:          original.TabID,
		Memo:           memo,
		ActorUserID:    actorUserID,
		IdempotencyKey: idempotencyKey,
		// A reversal is not itself a payment, so it carries no method.
		Method: store.MethodNone,
	}, store.KindReversal, original.Amount.Neg(), &seq)

	// The unique index on reverses_seq surfaces a second undo as a conflict.
	// Report it as ErrAlreadyReversed so the caller can say something useful.
	if err != nil && errors.Is(err, store.ErrConflict) {
		return store.Entry{}, false, ErrAlreadyReversed
	}
	return entry, replayed, err
}

// Balance derives the tab's current balance by summing entries (LEDGER-03).
// Negative means the Payee owes; positive means they hold a credit.
func (s *Service) Balance(ctx context.Context, tabID int64) (money.Cents, error) {
	return s.store.SumEntries(ctx, tabID)
}

// History returns the tab's entries, newest first.
func (s *Service) History(ctx context.Context, tabID int64) ([]store.Entry, error) {
	return s.store.ListEntries(ctx, tabID)
}

// NewIdempotencyKey generates a 256-bit random key using crypto/rand.
func NewIdempotencyKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("ledger: generate idempotency key: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// ItemsFrom converts a tab's current items into the snapshot form stored with
// an entry, so that a later edit to the tab cannot rewrite what was charged.
func ItemsFrom(items []store.TabItem) []store.EntryItem {
	out := make([]store.EntryItem, 0, len(items))
	for i, it := range items {
		out = append(out, store.EntryItem{Position: i, Name: it.Name, Amount: it.Amount})
	}
	return out
}

// SumItems totals an item breakdown (TAB-04). It reports overflow rather than
// silently wrapping.
func SumItems(items []store.EntryItem) (money.Cents, bool) {
	amounts := make([]money.Cents, 0, len(items))
	for _, it := range items {
		amounts = append(amounts, it.Amount)
	}
	return money.Sum(amounts)
}

// ReversedSeqs returns the set of entry sequences that already carry a
// reversal, computed from the entry list rather than a second query. Callers
// use it to hide an undo control that would be refused.
func ReversedSeqs(entries []store.Entry) map[int64]bool {
	reversed := make(map[int64]bool)
	for _, e := range entries {
		if e.ReversesSeq != nil {
			reversed[*e.ReversesSeq] = true
		}
	}
	return reversed
}

// CanUndo reports whether an entry is eligible to be undone: it must not be a
// reversal, and must not already have been reversed.
func CanUndo(e store.Entry, reversed map[int64]bool) bool {
	return e.Kind != store.KindReversal && !reversed[e.Seq]
}
