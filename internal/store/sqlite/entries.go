package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

const (
	entryColumns = `seq, tab_id, kind, amount_cents, memo, effective_at, created_at, actor_user_id, idempotency_key, reverses_seq, method, category`
	// same list qualified for joins.
	entryColumnsE = `e.seq, e.tab_id, e.kind, e.amount_cents, e.memo, e.effective_at, e.created_at, e.actor_user_id, e.idempotency_key, e.reverses_seq, e.method, e.category`
)

func scanEntry(row interface{ Scan(...any) error }) (store.Entry, error) {
	var (
		e         store.Entry
		kind      string
		amount    int64
		effective string
		created   string
		reverses  sql.NullInt64
		method    string
		category  string
	)
	if err := row.Scan(&e.Seq, &e.TabID, &kind, &amount, &e.Memo,
		&effective, &created, &e.ActorUserID, &e.IdempotencyKey, &reverses, &method, &category); err != nil {
		return store.Entry{}, translate(err)
	}
	e.Kind = store.EntryKind(kind)
	e.Method = store.PaymentMethod(method)
	e.Category = store.EntryCategory(category)
	e.Amount = money.Cents(amount)

	var err error
	if e.EffectiveAt, err = parseTime(effective); err != nil {
		return store.Entry{}, fmt.Errorf("sqlite: parse entry effective_at: %w", err)
	}
	if e.CreatedAt, err = parseTime(created); err != nil {
		return store.Entry{}, fmt.Errorf("sqlite: parse entry created_at: %w", err)
	}
	if reverses.Valid {
		v := reverses.Int64
		e.ReversesSeq = &v
	}
	return e, nil
}

// PostEntry appends an entry and its item snapshot in one transaction.
//
// LEDGER-01: this is the only write path to the entries table. There is no
// update or delete counterpart anywhere in the package.
//
// LEDGER-06: seq is assigned by the database via AUTOINCREMENT, so ordering
// never depends on a client clock.
//
// Idempotency: a replayed key returns the entry already posted under that key,
// with replayed=true, instead of writing a second row. The uniqueness check is
// the database constraint rather than a prior SELECT, so two concurrent
// requests carrying the same key cannot both pass a check and then both insert.
func (d *DB) PostEntry(ctx context.Context, e store.NewEntry) (store.Entry, bool, error) {
	now, err := validateNewEntry(&e)
	if err != nil {
		return store.Entry{}, false, err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Entry{}, false, fmt.Errorf("sqlite: begin post entry: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	seq, err := insertEntry(ctx, tx, e, now)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Either a replayed idempotency key or a second reversal of the same
			// entry. Resolve which, outside the failed transaction.
			_ = tx.Rollback()
			if existing, lookupErr := d.entryByIdempotencyKey(ctx, e.IdempotencyKey); lookupErr == nil {
				return existing, true, nil
			}
		}
		return store.Entry{}, false, err
	}

	if err := tx.Commit(); err != nil {
		return store.Entry{}, false, fmt.Errorf("sqlite: commit post entry: %w", err)
	}
	return builtEntry(e, seq, now), false, nil
}

// PostPeriodEntry appends a scheduled charge and claims its billing cycle in
// one transaction (SCHED-03).
//
// SCHED-04: the claim and the entry share a transaction, and the claim carries
// the (tab_id, period_key) primary key. Two concurrent reads of an overdue tab
// therefore cannot both post: whichever loses the race on the claim has its
// entry rolled back with it. There is no check-then-write anywhere in this
// path, so there is no window between the two to lose.
//
// A cycle already claimed returns the entry that claimed it with replayed=true,
// matching how a replayed idempotency key behaves. The caller cannot tell
// whether it lost a race or is simply reading a tab that was already current,
// and does not need to.
func (d *DB) PostPeriodEntry(ctx context.Context, p store.PostedPeriod, e store.NewEntry) (store.Entry, bool, error) {
	if p.Key == "" {
		return store.Entry{}, false, errors.New("sqlite: period requires a key")
	}
	if p.TabID != e.TabID {
		return store.Entry{}, false, fmt.Errorf("sqlite: period tab %d does not match entry tab %d", p.TabID, e.TabID)
	}
	now, err := validateNewEntry(&e)
	if err != nil {
		return store.Entry{}, false, err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Entry{}, false, fmt.Errorf("sqlite: begin post period: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	seq, err := insertEntry(ctx, tx, e, now)
	if err == nil {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO posted_periods
                 (tab_id, period_key, entry_seq, period_start, period_end, due_on, posted_at)
             VALUES (?, ?, ?, ?, ?, ?, ?)`,
			p.TabID, p.Key, seq, p.Start.String(), p.End.String(), p.DueOn.String(), toText(now))
		err = translate(err)
	}
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			// The cycle was claimed by someone else between this read and this
			// write, or the entry's key was replayed. Either way the charge
			// exists; return it rather than reporting a failure.
			_ = tx.Rollback()
			if existing, lookupErr := d.entryForPeriod(ctx, p.TabID, p.Key); lookupErr == nil {
				return existing, true, nil
			}
			if existing, lookupErr := d.entryByIdempotencyKey(ctx, e.IdempotencyKey); lookupErr == nil {
				return existing, true, nil
			}
		}
		return store.Entry{}, false, err
	}

	if err := tx.Commit(); err != nil {
		return store.Entry{}, false, fmt.Errorf("sqlite: commit post period: %w", err)
	}
	return builtEntry(e, seq, now), false, nil
}

// PostFeeEntry appends a late-fee entry and claims the overdue date it answers
// to, in one transaction (FEE-03, FEE-04).
//
// This is PostPeriodEntry's sibling: the fee entry and the claim share a
// transaction, and the claim carries the (tab_id, fee_key) primary key, so an
// overdue date can be assessed at most once however many readers pass over it
// at once. A date already assessed returns the entry that assessed it with
// replayed=true.
func (d *DB) PostFeeEntry(ctx context.Context, f store.PostedFee, e store.NewEntry) (store.Entry, bool, error) {
	if f.Key == "" {
		return store.Entry{}, false, errors.New("sqlite: fee requires a key")
	}
	if f.TabID != e.TabID {
		return store.Entry{}, false, fmt.Errorf("sqlite: fee tab %d does not match entry tab %d", f.TabID, e.TabID)
	}
	if e.Kind != store.KindFee {
		return store.Entry{}, false, fmt.Errorf("sqlite: PostFeeEntry requires a fee entry, got %q", e.Kind)
	}
	now, err := validateNewEntry(&e)
	if err != nil {
		return store.Entry{}, false, err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Entry{}, false, fmt.Errorf("sqlite: begin post fee: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	seq, err := insertEntry(ctx, tx, e, now)
	if err == nil {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO posted_fees
                 (tab_id, fee_key, entry_seq, assessed_for, base_cents, posted_at)
             VALUES (?, ?, ?, ?, ?, ?)`,
			f.TabID, f.Key, seq, f.AssessedFor.String(), int64(f.Base), toText(now))
		err = translate(err)
	}
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			_ = tx.Rollback()
			if existing, lookupErr := d.entryForFee(ctx, f.TabID, f.Key); lookupErr == nil {
				return existing, true, nil
			}
			if existing, lookupErr := d.entryByIdempotencyKey(ctx, e.IdempotencyKey); lookupErr == nil {
				return existing, true, nil
			}
		}
		return store.Entry{}, false, err
	}

	if err := tx.Commit(); err != nil {
		return store.Entry{}, false, fmt.Errorf("sqlite: commit post fee: %w", err)
	}
	return builtEntry(e, seq, now), false, nil
}

func (d *DB) entryForFee(ctx context.Context, tabID int64, key string) (store.Entry, error) {
	return scanEntry(d.db.QueryRowContext(ctx,
		`SELECT `+entryColumnsE+`
         FROM entries e
         JOIN posted_fees f ON f.entry_seq = e.seq
         WHERE f.tab_id = ? AND f.fee_key = ?`, tabID, key))
}

// ListPostedFees returns a tab's assessed fees, newest first (FEE-03).
func (d *DB) ListPostedFees(ctx context.Context, tabID int64) ([]store.PostedFee, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT tab_id, fee_key, entry_seq, assessed_for, base_cents, posted_at
         FROM posted_fees
         WHERE tab_id = ?
         ORDER BY assessed_for DESC, fee_key DESC`, tabID)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.PostedFee
	for rows.Next() {
		var (
			f        store.PostedFee
			assessed string
			base     int64
			posted   string
		)
		if err := rows.Scan(&f.TabID, &f.Key, &f.EntrySeq, &assessed, &base, &posted); err != nil {
			return nil, translate(err)
		}
		f.Base = money.Cents(base)
		if f.AssessedFor, err = schedule.ParseDate(assessed); err != nil {
			return nil, fmt.Errorf("sqlite: parse fee assessed_for: %w", err)
		}
		if f.PostedAt, err = parseTime(posted); err != nil {
			return nil, fmt.Errorf("sqlite: parse fee posted_at: %w", err)
		}
		out = append(out, f)
	}
	return out, translate(rows.Err())
}

// PostInterestEntry appends a periodic interest charge and claims the period it
// accrued for, in one transaction. The declining-balance counterpart of
// PostFeeEntry, with the same exactly-once and replay behaviour.
func (d *DB) PostInterestEntry(ctx context.Context, in store.PostedInterest, e store.NewEntry) (store.Entry, bool, error) {
	if in.Key == "" {
		return store.Entry{}, false, errors.New("sqlite: interest requires a key")
	}
	if in.TabID != e.TabID {
		return store.Entry{}, false, fmt.Errorf("sqlite: interest tab %d does not match entry tab %d", in.TabID, e.TabID)
	}
	if e.Category != store.CategoryInterest {
		return store.Entry{}, false, fmt.Errorf("sqlite: PostInterestEntry requires an interest entry, got category %q", e.Category)
	}
	now, err := validateNewEntry(&e)
	if err != nil {
		return store.Entry{}, false, err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Entry{}, false, fmt.Errorf("sqlite: begin post interest: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	seq, err := insertEntry(ctx, tx, e, now)
	if err == nil {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO posted_interest
                 (tab_id, period_key, entry_seq, accrued_for, base_cents, posted_at)
             VALUES (?, ?, ?, ?, ?, ?)`,
			in.TabID, in.Key, seq, in.AccruedFor.String(), int64(in.Base), toText(now))
		err = translate(err)
	}
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			_ = tx.Rollback()
			if existing, lookupErr := d.entryForInterest(ctx, in.TabID, in.Key); lookupErr == nil {
				return existing, true, nil
			}
			if existing, lookupErr := d.entryByIdempotencyKey(ctx, e.IdempotencyKey); lookupErr == nil {
				return existing, true, nil
			}
		}
		return store.Entry{}, false, err
	}

	if err := tx.Commit(); err != nil {
		return store.Entry{}, false, fmt.Errorf("sqlite: commit post interest: %w", err)
	}
	return builtEntry(e, seq, now), false, nil
}

func (d *DB) entryForInterest(ctx context.Context, tabID int64, key string) (store.Entry, error) {
	return scanEntry(d.db.QueryRowContext(ctx,
		`SELECT `+entryColumnsE+`
         FROM entries e
         JOIN posted_interest i ON i.entry_seq = e.seq
         WHERE i.tab_id = ? AND i.period_key = ?`, tabID, key))
}

// ListPostedInterest returns a tab's accrued interest periods, newest first.
func (d *DB) ListPostedInterest(ctx context.Context, tabID int64) ([]store.PostedInterest, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT tab_id, period_key, entry_seq, accrued_for, base_cents, posted_at
         FROM posted_interest
         WHERE tab_id = ?
         ORDER BY accrued_for DESC, period_key DESC`, tabID)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.PostedInterest
	for rows.Next() {
		var (
			in      store.PostedInterest
			accrued string
			base    int64
			posted  string
		)
		if err := rows.Scan(&in.TabID, &in.Key, &in.EntrySeq, &accrued, &base, &posted); err != nil {
			return nil, translate(err)
		}
		in.Base = money.Cents(base)
		if in.AccruedFor, err = schedule.ParseDate(accrued); err != nil {
			return nil, fmt.Errorf("sqlite: parse interest accrued_for: %w", err)
		}
		if in.PostedAt, err = parseTime(posted); err != nil {
			return nil, fmt.Errorf("sqlite: parse interest posted_at: %w", err)
		}
		out = append(out, in)
	}
	return out, translate(rows.Err())
}

// validateNewEntry checks the invariants shared by every write path and fills
// in the timestamps, returning the creation time.
func validateNewEntry(e *store.NewEntry) (time.Time, error) {
	if !e.Kind.Valid() {
		return time.Time{}, fmt.Errorf("sqlite: invalid entry kind %q", e.Kind)
	}
	if !e.Method.Valid() {
		return time.Time{}, fmt.Errorf("sqlite: invalid payment method %q", e.Method)
	}
	if e.IdempotencyKey == "" {
		return time.Time{}, errors.New("sqlite: entry requires an idempotency key")
	}
	now := time.Now().UTC()
	if e.EffectiveAt.IsZero() {
		e.EffectiveAt = now
	}
	return now, nil
}

// insertEntry writes an entry and its item snapshot inside an open
// transaction, returning the assigned sequence. Errors arrive already
// translated to the package's sentinels.
func insertEntry(ctx context.Context, tx *sql.Tx, e store.NewEntry, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO entries
             (tab_id, kind, amount_cents, memo, effective_at, created_at, actor_user_id, idempotency_key, reverses_seq, method, category)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TabID, string(e.Kind), int64(e.Amount), e.Memo,
		toText(e.EffectiveAt), toText(now), e.ActorUserID, e.IdempotencyKey,
		nullInt64(e.ReversesSeq), string(e.Method), string(e.Category))
	if err != nil {
		return 0, translate(err)
	}

	seq, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("sqlite: entry seq: %w", err)
	}

	for i, item := range e.Items {
		pos := item.Position
		if pos == 0 {
			pos = i
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO entry_items (entry_seq, position, name, amount_cents) VALUES (?, ?, ?, ?)`,
			seq, pos, item.Name, int64(item.Amount)); err != nil {
			return 0, translate(err)
		}
	}
	return seq, nil
}

// builtEntry assembles the posted entry from what was written, so callers get
// the row back without a second read.
func builtEntry(e store.NewEntry, seq int64, now time.Time) store.Entry {
	return store.Entry{
		Seq:            seq,
		TabID:          e.TabID,
		Kind:           e.Kind,
		Amount:         e.Amount,
		Memo:           e.Memo,
		EffectiveAt:    e.EffectiveAt,
		CreatedAt:      now,
		ActorUserID:    e.ActorUserID,
		IdempotencyKey: e.IdempotencyKey,
		ReversesSeq:    e.ReversesSeq,
		Method:         e.Method,
		Category:       e.Category,
	}
}

// entryForPeriod returns the entry that claimed a billing cycle.
func (d *DB) entryForPeriod(ctx context.Context, tabID int64, key string) (store.Entry, error) {
	return scanEntry(d.db.QueryRowContext(ctx,
		`SELECT `+entryColumnsE+`
         FROM entries e
         JOIN posted_periods p ON p.entry_seq = e.seq
         WHERE p.tab_id = ? AND p.period_key = ?`, tabID, key))
}

// ListPostedPeriods returns a tab's charged cycles, newest first (CHG-04).
func (d *DB) ListPostedPeriods(ctx context.Context, tabID int64) ([]store.PostedPeriod, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT tab_id, period_key, entry_seq, period_start, period_end, due_on, posted_at
         FROM posted_periods
         WHERE tab_id = ?
         ORDER BY due_on DESC, period_key DESC`, tabID)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.PostedPeriod
	for rows.Next() {
		var (
			p                       store.PostedPeriod
			start, end, due, posted string
		)
		if err := rows.Scan(&p.TabID, &p.Key, &p.EntrySeq, &start, &end, &due, &posted); err != nil {
			return nil, translate(err)
		}
		if p.Start, err = schedule.ParseDate(start); err != nil {
			return nil, fmt.Errorf("sqlite: parse period start: %w", err)
		}
		if p.End, err = schedule.ParseDate(end); err != nil {
			return nil, fmt.Errorf("sqlite: parse period end: %w", err)
		}
		if p.DueOn, err = schedule.ParseDate(due); err != nil {
			return nil, fmt.Errorf("sqlite: parse period due date: %w", err)
		}
		if p.PostedAt, err = parseTime(posted); err != nil {
			return nil, fmt.Errorf("sqlite: parse period posted_at: %w", err)
		}
		out = append(out, p)
	}
	return out, translate(rows.Err())
}

func (d *DB) entryByIdempotencyKey(ctx context.Context, key string) (store.Entry, error) {
	return scanEntry(d.db.QueryRowContext(ctx,
		`SELECT `+entryColumns+` FROM entries WHERE idempotency_key = ?`, key))
}

// GetEntry looks up a single entry by sequence.
func (d *DB) GetEntry(ctx context.Context, seq int64) (store.Entry, error) {
	return scanEntry(d.db.QueryRowContext(ctx,
		`SELECT `+entryColumns+` FROM entries WHERE seq = ?`, seq))
}

// ListEntries returns a tab's entries in authoritative order, newest first.
func (d *DB) ListEntries(ctx context.Context, tabID int64) ([]store.Entry, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+entryColumns+` FROM entries WHERE tab_id = ? ORDER BY seq DESC`, tabID)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, translate(rows.Err())
}

// ListEntryItems returns the item breakdown captured when the entry posted.
func (d *DB) ListEntryItems(ctx context.Context, entrySeq int64) ([]store.EntryItem, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT position, name, amount_cents FROM entry_items WHERE entry_seq = ? ORDER BY position`,
		entrySeq)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.EntryItem
	for rows.Next() {
		var (
			it     store.EntryItem
			amount int64
		)
		if err := rows.Scan(&it.Position, &it.Name, &amount); err != nil {
			return nil, translate(err)
		}
		it.Amount = money.Cents(amount)
		out = append(out, it)
	}
	return out, translate(rows.Err())
}

// ListEntryItemsForTab returns every entry's item snapshot for one tab, keyed
// by entry sequence (CHG-04). A statement page needs many breakdowns at once,
// and this keeps that one query rather than one per period.
func (d *DB) ListEntryItemsForTab(ctx context.Context, tabID int64) (map[int64][]store.EntryItem, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT i.entry_seq, i.position, i.name, i.amount_cents
         FROM entry_items i
         JOIN entries e ON e.seq = i.entry_seq
         WHERE e.tab_id = ?
         ORDER BY i.entry_seq, i.position`, tabID)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[int64][]store.EntryItem)
	for rows.Next() {
		var (
			seq    int64
			it     store.EntryItem
			amount int64
		)
		if err := rows.Scan(&seq, &it.Position, &it.Name, &amount); err != nil {
			return nil, translate(err)
		}
		it.Amount = money.Cents(amount)
		out[seq] = append(out[seq], it)
	}
	return out, translate(rows.Err())
}

// SumEntries derives the tab balance (LEDGER-03).
//
// The sum is computed in SQL over integer cents. There is no cached balance
// column to fall out of step with the entries, because no such column exists.
func (d *DB) SumEntries(ctx context.Context, tabID int64) (money.Cents, error) {
	var total sql.NullInt64
	err := d.db.QueryRowContext(ctx,
		`SELECT SUM(amount_cents) FROM entries WHERE tab_id = ?`, tabID).Scan(&total)
	if err != nil {
		return 0, translate(err)
	}
	if !total.Valid {
		return 0, nil // no entries yet
	}
	return money.Cents(total.Int64), nil
}

func nullInt64(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

var _ store.EntryStore = (*DB)(nil)
var _ store.Store = (*DB)(nil)
