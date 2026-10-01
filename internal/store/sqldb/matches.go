package sqldb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
)

// Confirmed bank matches (SPEC-BANK-RECONCILE, RECON-04). Migration 0016.

// ConfirmBankMatch records a match and posts its delta in one transaction.
//
// Order matters for correctness under concurrency:
//
//  1. The line row is locked first (FOR UPDATE on MariaDB; SQLite's single
//     writer serializes anyway), so two confirms of one line queue rather than
//     both reading it as open.
//  2. The match INSERT sets active_line_id and active_entry_seq, each UNIQUE:
//     whatever a check above missed, the database refuses a second active
//     match of the line or the payment.
//  3. The delta entry's key is "recon:<match id>", so the entry is tied to
//     exactly one match and cannot be posted twice.
//  4. The line becomes matched.
//
// Any failure rolls all of it back: no match without its delta, no delta
// without its match.
func (d *DB) ConfirmBankMatch(ctx context.Context, m store.NewBankMatch, delta *store.NewEntry) (store.BankMatch, bool, error) {
	var (
		out      store.BankMatch
		replayed bool
		err      error
	)
	for attempt := 0; attempt < 4; attempt++ {
		out, replayed, err = d.confirmBankMatch(ctx, m, delta)
		if !isDeadlock(err) {
			break
		}
	}
	return out, replayed, err
}

func (d *DB) confirmBankMatch(ctx context.Context, m store.NewBankMatch, delta *store.NewEntry) (store.BankMatch, bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return store.BankMatch{}, false, fmt.Errorf("begin confirm match: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		state    string
		importID int64
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT state, import_id FROM bank_lines WHERE id = ?`+d.dialect.lockRows(), m.LineID).
		Scan(&state, &importID); err != nil {
		return store.BankMatch{}, false, translate(err)
	}
	if state != string(store.BankLineOpen) {
		// The same pair confirmed twice (a double submit) is a replay.
		_ = tx.Rollback()
		if existing, err := d.activeMatchForLine(ctx, m.LineID); err == nil && existing.EntrySeq == m.EntrySeq {
			return existing, true, nil
		}
		return store.BankMatch{}, false, store.ErrLineNotOpen
	}

	// The payment: a payment, on the stated tab, not reversed, not itself
	// posted by reconciliation.
	var (
		kind  string
		tabID int64
		key   string
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT e.kind, e.tab_id, e.idempotency_key FROM entries e
		  WHERE e.seq = ? AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_seq = e.seq)`,
		m.EntrySeq).Scan(&kind, &tabID, &key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.BankMatch{}, false, store.ErrNotAPayment
		}
		return store.BankMatch{}, false, translate(err)
	}
	if kind != string(store.KindPayment) || tabID != m.TabID || strings.HasPrefix(key, store.ReconKeyPrefix) {
		return store.BankMatch{}, false, store.ErrNotAPayment
	}

	now := nowText()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO bank_matches (line_id, import_id, entry_seq, tab_id, bank_date, bank_cents,
		        recorded_date, recorded_cents, date_flagged, date_flag_reason, note_in_memo,
		        confirmed_by, confirmed_at, active_line_id, active_entry_seq)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.LineID, importID, m.EntrySeq, m.TabID, m.BankDate, int64(m.BankAmount),
		m.RecordedDate, int64(m.RecordedAmount), boolToInt(m.DateFlagged), m.DateFlagReason,
		boolToInt(m.NoteInMemo), m.ConfirmedBy, now, m.LineID, m.EntrySeq)
	if err != nil {
		if err = translate(err); errors.Is(err, store.ErrConflict) {
			// The line was locked and open, so the conflict is the payment.
			return store.BankMatch{}, false, store.ErrPaymentMatched
		}
		return store.BankMatch{}, false, err
	}
	matchID, err := res.LastInsertId()
	if err != nil {
		return store.BankMatch{}, false, fmt.Errorf("match id: %w", err)
	}

	if delta != nil {
		e := *delta
		e.IdempotencyKey = store.ReconKeyPrefix + ":" + strconv.FormatInt(matchID, 10)
		entryNow, err := validateNewEntry(&e)
		if err != nil {
			return store.BankMatch{}, false, err
		}
		seq, err := insertEntry(ctx, tx, e, entryNow)
		if err != nil {
			return store.BankMatch{}, false, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE bank_matches SET delta_entry_seq = ? WHERE id = ?`, seq, matchID); err != nil {
			return store.BankMatch{}, false, translate(err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE bank_lines SET state = 'matched' WHERE id = ?`, m.LineID); err != nil {
		return store.BankMatch{}, false, translate(err)
	}
	if err := tx.Commit(); err != nil {
		return store.BankMatch{}, false, fmt.Errorf("commit confirm match: %w", err)
	}
	out, err := d.GetBankMatch(ctx, matchID)
	return out, false, err
}

func (d *DB) activeMatchForLine(ctx context.Context, lineID int64) (store.BankMatch, error) {
	return scanBankMatch(d.db.QueryRowContext(ctx, bankMatchSelect+` WHERE m.active_line_id = ?`, lineID))
}

// UndoBankMatch undoes a standing match in one transaction. The UPDATE that
// marks it undone matches only a standing match, so of two concurrent undos
// exactly one proceeds.
func (d *DB) UndoBankMatch(ctx context.Context, matchID, by int64, reversal *store.NewEntry) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin undo match: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var lineID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT line_id FROM bank_matches WHERE id = ?`+d.dialect.lockRows(), matchID).Scan(&lineID); err != nil {
		return translate(err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE bank_matches SET undone_at = ?, undone_by = ?, active_line_id = NULL, active_entry_seq = NULL
		  WHERE id = ? AND undone_at = ''`, nowText(), by, matchID)
	if err != nil {
		return translate(err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("undo match rows: %w", err)
	} else if n == 0 {
		return store.ErrMatchUndone
	}

	if reversal != nil {
		e := *reversal
		e.IdempotencyKey = store.ReconKeyPrefix + ":undo:" + strconv.FormatInt(matchID, 10)
		entryNow, err := validateNewEntry(&e)
		if err != nil {
			return err
		}
		if _, err := insertEntry(ctx, tx, e, entryNow); err != nil {
			// A conflict here is the delta already reversed by another path.
			// Refuse rather than leave the match undone with its money in
			// place: the person sees the error and nothing has changed.
			return err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE bank_lines SET state = 'open' WHERE id = ? AND state = 'matched'`, lineID); err != nil {
		return translate(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit undo match: %w", err)
	}
	return nil
}

const bankMatchSelect = `SELECT m.id, m.line_id, m.import_id, m.entry_seq, m.tab_id, m.delta_entry_seq,
	        m.bank_date, m.bank_cents, m.recorded_date, m.recorded_cents, m.date_flagged,
	        m.date_flag_reason, m.note_in_memo, m.confirmed_by, m.confirmed_at, m.undone_at,
	        m.undone_by, u.display_name, t.name, i.file_name, l.file_row, l.original_text,
	        l.description, l.note, de.amount_cents, de.kind
	   FROM bank_matches m
	   JOIN users u ON u.id = m.confirmed_by
	   JOIN tabs t ON t.id = m.tab_id
	   JOIN bank_imports i ON i.id = m.import_id
	   JOIN bank_lines l ON l.id = m.line_id
	   LEFT JOIN entries de ON de.seq = m.delta_entry_seq`

func scanBankMatch(row interface{ Scan(...any) error }) (store.BankMatch, error) {
	var (
		m                   store.BankMatch
		delta, undoneBy     sql.NullInt64
		bank, rec           int64
		flagged, noteInMemo int
		confirmedAt, undone string
		deltaCents          sql.NullInt64
		deltaKind           sql.NullString
	)
	if err := row.Scan(&m.ID, &m.LineID, &m.ImportID, &m.EntrySeq, &m.TabID, &delta,
		&m.BankDate, &bank, &m.RecordedDate, &rec, &flagged, &m.DateFlagReason, &noteInMemo,
		&m.ConfirmedBy, &confirmedAt, &undone, &undoneBy, &m.ConfirmedByName, &m.TabName,
		&m.FileName, &m.Row, &m.Raw, &m.Description, &m.Note, &deltaCents, &deltaKind); err != nil {
		return store.BankMatch{}, translate(err)
	}
	if delta.Valid {
		v := delta.Int64
		m.DeltaEntrySeq = &v
	}
	m.BankAmount, m.RecordedAmount = money.Cents(bank), money.Cents(rec)
	m.DateFlagged, m.NoteInMemo = flagged != 0, noteInMemo != 0
	m.UndoneBy = undoneBy.Int64
	m.DeltaAmount, m.DeltaKind = money.Cents(deltaCents.Int64), store.EntryKind(deltaKind.String)
	var err error
	if m.ConfirmedAt, err = parseTime(confirmedAt); err != nil {
		return store.BankMatch{}, fmt.Errorf("parse match confirmed_at: %w", err)
	}
	if undone != "" {
		t, err := parseTime(undone)
		if err != nil {
			return store.BankMatch{}, fmt.Errorf("parse match undone_at: %w", err)
		}
		m.UndoneAt = &t
	}
	return m, nil
}

// GetBankMatch reads one match with its joins.
func (d *DB) GetBankMatch(ctx context.Context, id int64) (store.BankMatch, error) {
	return scanBankMatch(d.db.QueryRowContext(ctx, bankMatchSelect+` WHERE m.id = ?`, id))
}

// ListBankMatches returns matches newest first, undone ones included.
func (d *DB) ListBankMatches(ctx context.Context, f store.BankMatchFilter) ([]store.BankMatch, error) {
	q := bankMatchSelect + ` WHERE 1 = 1`
	var args []any
	if f.TabID != 0 {
		q += ` AND m.tab_id = ?`
		args = append(args, f.TabID)
	}
	if f.ImportID != 0 {
		q += ` AND m.import_id = ?`
		args = append(args, f.ImportID)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	q += ` ORDER BY m.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.BankMatch
	for rows.Next() {
		m, err := scanBankMatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, translate(rows.Err())
}

// EntryHasActiveMatch reports whether a payment has a standing match.
func (d *DB) EntryHasActiveMatch(ctx context.Context, seq int64) (bool, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bank_matches WHERE active_entry_seq = ?`, seq).Scan(&n)
	return n > 0, translate(err)
}
