package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/johnzastrow/bitt/internal/store"
)

// foldEmail produces the case-insensitive lookup key. Folding happens in Go
// rather than via a database collation so the behavior is identical on MariaDB.
func foldEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ---------------------------------------------------------------------------
// Instance
// ---------------------------------------------------------------------------

// GetInstance reads the single instance row.
func (d *DB) GetInstance(ctx context.Context) (store.Instance, error) {
	var (
		inst      store.Instance
		completed sql.NullString
		created   string
	)
	err := d.db.QueryRowContext(ctx,
		`SELECT timezone, setup_completed_at, created_at FROM instance WHERE id = 1`).
		Scan(&inst.Timezone, &completed, &created)
	if err != nil {
		return store.Instance{}, translate(err)
	}

	if inst.SetupCompletedAt, err = fromNullText(completed); err != nil {
		return store.Instance{}, fmt.Errorf("sqlite: parse setup_completed_at: %w", err)
	}
	if inst.CreatedAt, err = parseTime(created); err != nil {
		return store.Instance{}, fmt.Errorf("sqlite: parse instance created_at: %w", err)
	}
	return inst, nil
}

// CompleteSetup creates the first admin and latches setup closed atomically.
//
// AUTH-03: the UPDATE carries `WHERE setup_completed_at IS NULL`, so if two
// requests race, the second one's update matches zero rows and the whole
// transaction rolls back. The lock is therefore enforced by the database, not
// by a check-then-act in the handler.
func (d *DB) CompleteSetup(ctx context.Context, admin store.User, timezone string) (store.User, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return store.User{}, fmt.Errorf("sqlite: begin setup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE instance SET setup_completed_at = ?, timezone = ?
         WHERE id = 1 AND setup_completed_at IS NULL`,
		nowText(), timezone)
	if err != nil {
		return store.User{}, translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return store.User{}, fmt.Errorf("sqlite: setup rows affected: %w", err)
	}
	if n == 0 {
		return store.User{}, fmt.Errorf("%w: setup already completed", store.ErrConflict)
	}

	// Belt and braces: refuse if any account already exists, so setup cannot
	// mint a second admin even if the latch were somehow cleared.
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return store.User{}, translate(err)
	}
	if count > 0 {
		return store.User{}, fmt.Errorf("%w: accounts already exist", store.ErrConflict)
	}

	admin.IsAdmin = true
	created, err := insertUser(ctx, tx, admin)
	if err != nil {
		return store.User{}, err
	}
	if err := tx.Commit(); err != nil {
		return store.User{}, fmt.Errorf("sqlite: commit setup: %w", err)
	}
	return created, nil
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertUser(ctx context.Context, ex execer, u store.User) (store.User, error) {
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	res, err := ex.ExecContext(ctx,
		`INSERT INTO users (email, email_folded, display_name, password_hash, is_admin, created_at, deactivated_at)
         VALUES (?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(u.Email), foldEmail(u.Email), u.DisplayName, u.PasswordHash,
		boolToInt(u.IsAdmin), toText(u.CreatedAt), toNullText(u.DeactivatedAt))
	if err != nil {
		return store.User{}, translate(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return store.User{}, fmt.Errorf("sqlite: user id: %w", err)
	}
	u.ID = id
	return u, nil
}

// CreateUser inserts an account.
func (d *DB) CreateUser(ctx context.Context, u store.User) (store.User, error) {
	return insertUser(ctx, d.db, u)
}

const userColumns = `id, email, display_name, password_hash, is_admin, created_at, deactivated_at`

func scanUser(row interface{ Scan(...any) error }) (store.User, error) {
	var (
		u           store.User
		isAdmin     int
		created     string
		deactivated sql.NullString
	)
	if err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &isAdmin, &created, &deactivated); err != nil {
		return store.User{}, translate(err)
	}
	u.IsAdmin = isAdmin != 0

	var err error
	if u.CreatedAt, err = parseTime(created); err != nil {
		return store.User{}, fmt.Errorf("sqlite: parse user created_at: %w", err)
	}
	if u.DeactivatedAt, err = fromNullText(deactivated); err != nil {
		return store.User{}, fmt.Errorf("sqlite: parse user deactivated_at: %w", err)
	}
	return u, nil
}

// GetUser looks up an account by id.
func (d *DB) GetUser(ctx context.Context, id int64) (store.User, error) {
	return scanUser(d.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// GetUserByEmail looks up an account case-insensitively.
func (d *DB) GetUserByEmail(ctx context.Context, email string) (store.User, error) {
	return scanUser(d.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE email_folded = ?`, foldEmail(email)))
}

// ListUsers returns all accounts, oldest first.
func (d *DB) ListUsers(ctx context.Context) ([]store.User, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+userColumns+` FROM users ORDER BY id`)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, translate(rows.Err())
}

// CountUsers returns the number of accounts.
func (d *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, translate(err)
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// CreateSession stores a session keyed by the token's digest.
func (d *DB) CreateSession(ctx context.Context, s store.Session) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at, last_seen_at)
         VALUES (?, ?, ?, ?, ?)`,
		s.TokenHash, s.UserID, toText(s.CreatedAt), toText(s.ExpiresAt), toText(s.LastSeenAt))
	return translate(err)
}

// GetSession returns the session and its user.
//
// It fails closed: an expired session or a deactivated user yields ErrNotFound
// rather than a valid-looking result the caller might honor.
func (d *DB) GetSession(ctx context.Context, tokenHash string) (store.Session, store.User, error) {
	var (
		s           store.Session
		u           store.User
		created     string
		expires     string
		lastSeen    string
		isAdmin     int
		uCreated    string
		deactivated sql.NullString
	)
	err := d.db.QueryRowContext(ctx,
		`SELECT s.token_hash, s.user_id, s.created_at, s.expires_at, s.last_seen_at,
                u.id, u.email, u.display_name, u.password_hash, u.is_admin, u.created_at, u.deactivated_at
         FROM sessions s
         JOIN users u ON u.id = s.user_id
         WHERE s.token_hash = ?
           AND s.expires_at > ?
           AND u.deactivated_at IS NULL`,
		tokenHash, nowText()).
		Scan(&s.TokenHash, &s.UserID, &created, &expires, &lastSeen,
			&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &isAdmin, &uCreated, &deactivated)
	if err != nil {
		return store.Session{}, store.User{}, translate(err)
	}
	u.IsAdmin = isAdmin != 0

	for _, p := range []struct {
		src string
		dst *time.Time
	}{{created, &s.CreatedAt}, {expires, &s.ExpiresAt}, {lastSeen, &s.LastSeenAt}, {uCreated, &u.CreatedAt}} {
		t, err := parseTime(p.src)
		if err != nil {
			return store.Session{}, store.User{}, fmt.Errorf("sqlite: parse session time: %w", err)
		}
		*p.dst = t
	}
	if u.DeactivatedAt, err = fromNullText(deactivated); err != nil {
		return store.Session{}, store.User{}, err
	}
	return s, u, nil
}

// TouchSession records activity so idle sessions can be distinguished.
func (d *DB) TouchSession(ctx context.Context, tokenHash string, at time.Time) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, toText(at), tokenHash)
	return translate(err)
}

// DeleteSession logs a session out (AUTH-02).
func (d *DB) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := d.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return translate(err)
}

// DeleteExpiredSessions prunes the table.
func (d *DB) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := d.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, toText(now))
	if err != nil {
		return 0, translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: prune sessions: %w", err)
	}
	return n, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ensure the interface stays satisfied at compile time
var _ interface {
	store.InstanceStore
	store.UserStore
	store.SessionStore
} = (*DB)(nil)
