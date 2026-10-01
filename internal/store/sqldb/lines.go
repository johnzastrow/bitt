package sqldb

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/johnzastrow/bitt/internal/store"
)

// Bank line actions (RECON-05): record a line as a payment, mark it not
// BitTabby's, and undo either. Every action writes a bank_line_events row with
// who and when, in the same transaction as the change. Migration 0017.

// lockOpenLine locks a line and checks its state.
func (d *DB) lockLine(ctx context.Context, tx *sql.Tx, lineID int64) (string, *int64, error) {
	var (
		state string
		rec   sql.NullInt64
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT state, recorded_entry_seq FROM bank_lines WHERE id = ?`+d.dialect.lockRows(), lineID).
		Scan(&state, &rec); err != nil {
		return "", nil, translate(err)
	}
	if rec.Valid {
		v := rec.Int64
		return state, &v, nil
	}
	return state, nil, nil
}

func lineEvent(ctx context.Context, tx *sql.Tx, lineID int64, action string, seq *int64, note string, by int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO bank_line_events (line_id, action, entry_seq, note, by_user_id, at) VALUES (?, ?, ?, ?, ?, ?)`,
		lineID, action, nullInt64(seq), note, by, nowText())
	return translate(err)
}

// RecordBankLine posts the payment and marks the line recorded together.
func (d *DB) RecordBankLine(ctx context.Context, lineID, by int64, e store.NewEntry) (store.Entry, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Entry{}, fmt.Errorf("begin record line: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	state, _, err := d.lockLine(ctx, tx, lineID)
	if err != nil {
		return store.Entry{}, err
	}
	if state != string(store.BankLineOpen) {
		return store.Entry{}, store.ErrLineNotOpen
	}
	entryNow, err := validateNewEntry(&e)
	if err != nil {
		return store.Entry{}, err
	}
	seq, err := insertEntry(ctx, tx, e, entryNow)
	if err != nil {
		return store.Entry{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE bank_lines SET state = 'recorded', recorded_entry_seq = ? WHERE id = ?`, seq, lineID); err != nil {
		return store.Entry{}, translate(err)
	}
	if err := lineEvent(ctx, tx, lineID, "recorded", &seq, "", by); err != nil {
		return store.Entry{}, err
	}
	if err := tx.Commit(); err != nil {
		return store.Entry{}, fmt.Errorf("commit record line: %w", err)
	}
	return builtEntry(e, seq, entryNow), nil
}

// UnrecordBankLine reverses the recorded payment and reopens the line.
func (d *DB) UnrecordBankLine(ctx context.Context, lineID, by int64, reversal store.NewEntry, note string) (store.Entry, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Entry{}, fmt.Errorf("begin unrecord line: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	state, rec, err := d.lockLine(ctx, tx, lineID)
	if err != nil {
		return store.Entry{}, err
	}
	if state != string(store.BankLineRecorded) || rec == nil || reversal.ReversesSeq == nil || *reversal.ReversesSeq != *rec {
		return store.Entry{}, store.ErrLineNotOpen
	}
	entryNow, err := validateNewEntry(&reversal)
	if err != nil {
		return store.Entry{}, err
	}
	seq, err := insertEntry(ctx, tx, reversal, entryNow)
	if err != nil {
		return store.Entry{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE bank_lines SET state = 'open', recorded_entry_seq = NULL WHERE id = ?`, lineID); err != nil {
		return store.Entry{}, translate(err)
	}
	if err := lineEvent(ctx, tx, lineID, "unrecorded", rec, note, by); err != nil {
		return store.Entry{}, err
	}
	if err := tx.Commit(); err != nil {
		return store.Entry{}, fmt.Errorf("commit unrecord line: %w", err)
	}
	return builtEntry(reversal, seq, entryNow), nil
}

// IgnoreBankLine marks an open line not BitTabby's.
func (d *DB) IgnoreBankLine(ctx context.Context, lineID, by int64, note string) error {
	return d.lineTransition(ctx, lineID, by, store.BankLineOpen, store.BankLineIgnored,
		"Not BitTabby", note, "ignored")
}

// UnignoreBankLine opens an ignored line again, whatever ignored it.
func (d *DB) UnignoreBankLine(ctx context.Context, lineID, by int64) error {
	return d.lineTransition(ctx, lineID, by, store.BankLineIgnored, store.BankLineOpen, "", "", "unignored")
}

func (d *DB) lineTransition(ctx context.Context, lineID, by int64, from, to store.BankLineState,
	reason, note, action string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin line %s: %w", action, err)
	}
	defer func() { _ = tx.Rollback() }()
	state, _, err := d.lockLine(ctx, tx, lineID)
	if err != nil {
		return err
	}
	if state != string(from) {
		return store.ErrLineNotOpen
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE bank_lines SET state = ?, ignore_reason = ?, ignore_note = ? WHERE id = ?`,
		string(to), reason, note, lineID); err != nil {
		return translate(err)
	}
	if err := lineEvent(ctx, tx, lineID, action, nil, note, by); err != nil {
		return err
	}
	return tx.Commit()
}

// ListBankLineEvents returns what was done to a line, oldest first.
func (d *DB) ListBankLineEvents(ctx context.Context, lineID int64) ([]store.BankLineEvent, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT e.id, e.line_id, e.action, e.entry_seq, e.note, e.by_user_id, u.display_name, e.at
		   FROM bank_line_events e JOIN users u ON u.id = e.by_user_id
		  WHERE e.line_id = ? ORDER BY e.id`, lineID)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.BankLineEvent
	for rows.Next() {
		var (
			ev  store.BankLineEvent
			seq sql.NullInt64
			at  string
		)
		if err := rows.Scan(&ev.ID, &ev.LineID, &ev.Action, &seq, &ev.Note, &ev.By, &ev.ByName, &at); err != nil {
			return nil, translate(err)
		}
		if seq.Valid {
			v := seq.Int64
			ev.EntrySeq = &v
		}
		if ev.At, err = parseTime(at); err != nil {
			return nil, fmt.Errorf("parse line event at: %w", err)
		}
		out = append(out, ev)
	}
	return out, translate(rows.Err())
}

// ListRecordedLinesForTab returns the lines currently recorded on a tab.
func (d *DB) ListRecordedLinesForTab(ctx context.Context, tabID int64) ([]store.OpenBankLine, error) {
	rows, err := d.db.QueryContext(ctx, bankLineSelect+`
		  JOIN entries re ON re.seq = l.recorded_entry_seq
		 WHERE l.state = 'recorded' AND re.tab_id = ?
		 ORDER BY l.posted_on DESC, l.id DESC`, tabID)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.OpenBankLine
	for rows.Next() {
		l, err := scanOpenBankLine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, translate(rows.Err())
}
