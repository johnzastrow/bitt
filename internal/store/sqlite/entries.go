package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
)

const entryColumns = `seq, tab_id, kind, amount_cents, memo, effective_at, created_at, actor_user_id, idempotency_key, reverses_seq`

func scanEntry(row interface{ Scan(...any) error }) (store.Entry, error) {
	var (
		e         store.Entry
		kind      string
		amount    int64
		effective string
		created   string
		reverses  sql.NullInt64
	)
	if err := row.Scan(&e.Seq, &e.TabID, &kind, &amount, &e.Memo,
		&effective, &created, &e.ActorUserID, &e.IdempotencyKey, &reverses); err != nil {
		return store.Entry{}, translate(err)
	}
	e.Kind = store.EntryKind(kind)
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
	if !e.Kind.Valid() {
		return store.Entry{}, false, fmt.Errorf("sqlite: invalid entry kind %q", e.Kind)
	}
	if e.IdempotencyKey == "" {
		return store.Entry{}, false, errors.New("sqlite: entry requires an idempotency key")
	}
	now := time.Now().UTC()
	if e.EffectiveAt.IsZero() {
		e.EffectiveAt = now
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Entry{}, false, fmt.Errorf("sqlite: begin post entry: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO entries
             (tab_id, kind, amount_cents, memo, effective_at, created_at, actor_user_id, idempotency_key, reverses_seq)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TabID, string(e.Kind), int64(e.Amount), e.Memo,
		toText(e.EffectiveAt), toText(now), e.ActorUserID, e.IdempotencyKey, nullInt64(e.ReversesSeq))
	if err != nil {
		translated := translate(err)
		if errors.Is(translated, store.ErrConflict) {
			// Either a replayed idempotency key or a second reversal of the same
			// entry. Resolve which, outside the failed transaction.
			_ = tx.Rollback()
			if existing, lookupErr := d.entryByIdempotencyKey(ctx, e.IdempotencyKey); lookupErr == nil {
				return existing, true, nil
			}
			return store.Entry{}, false, translated
		}
		return store.Entry{}, false, translated
	}

	seq, err := res.LastInsertId()
	if err != nil {
		return store.Entry{}, false, fmt.Errorf("sqlite: entry seq: %w", err)
	}

	for i, item := range e.Items {
		pos := item.Position
		if pos == 0 {
			pos = i
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO entry_items (entry_seq, position, name, amount_cents) VALUES (?, ?, ?, ?)`,
			seq, pos, item.Name, int64(item.Amount)); err != nil {
			return store.Entry{}, false, translate(err)
		}
	}

	if err := tx.Commit(); err != nil {
		return store.Entry{}, false, fmt.Errorf("sqlite: commit post entry: %w", err)
	}

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
	}, false, nil
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
