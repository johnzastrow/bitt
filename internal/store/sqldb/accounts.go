package sqldb

import (
	"context"
	"database/sql"
	"errors"
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
		`SELECT timezone, setup_completed_at, created_at,
		        smtp_host, smtp_port, smtp_username, email_from, ntfy_url
		   FROM instance WHERE id = 1`).
		Scan(&inst.Timezone, &completed, &created,
			&inst.Delivery.SMTPHost, &inst.Delivery.SMTPPort, &inst.Delivery.SMTPUsername,
			&inst.Delivery.EmailFrom, &inst.Delivery.NtfyBaseURL)
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

// userColumns is deliberately without avatar_png. Session resolution reads a
// user row on every authenticated request, and pulling an image through that
// path would be a steady, pointless cost. Only GetAvatar touches the blob.
const userColumns = `id, email, display_name, password_hash, is_admin, created_at, ` +
	`deactivated_at, avatar_updated_at, ntfy_topic, notify_email, notify_ntfy`

func scanUser(row interface{ Scan(...any) error }) (store.User, error) {
	var (
		u           store.User
		isAdmin     int
		created     string
		deactivated sql.NullString
	)
	var notifyEmail, notifyNtfy int
	if err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &isAdmin, &created,
		&deactivated, &u.AvatarUpdatedAt, &u.NtfyTopic, &notifyEmail, &notifyNtfy); err != nil {
		return store.User{}, translate(err)
	}
	u.IsAdmin = isAdmin != 0
	u.NotifyEmail = notifyEmail != 0
	u.NotifyNtfy = notifyNtfy != 0

	var err error
	if u.CreatedAt, err = parseTime(created); err != nil {
		return store.User{}, fmt.Errorf("sqlite: parse user created_at: %w", err)
	}
	if u.DeactivatedAt, err = fromNullText(deactivated); err != nil {
		return store.User{}, fmt.Errorf("sqlite: parse user deactivated_at: %w", err)
	}
	return u, nil
}

// UpdateProfile changes an account's display name and email.
//
// The uniqueness index is on email_folded, so both columns move together or the
// stored address and the one that can be logged in with drift apart.
func (d *DB) UpdateProfile(ctx context.Context, id int64, displayName, email string) (store.User, error) {
	res, err := d.db.ExecContext(ctx,
		`UPDATE users SET display_name = ?, email = ?, email_folded = ? WHERE id = ?`,
		displayName, email, foldEmail(email), id)
	if err != nil {
		// A duplicate address surfaces as ErrConflict through translate, which
		// is what lets the handler say something specific rather than 500.
		return store.User{}, translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return store.User{}, fmt.Errorf("sqlite: update profile: %w", err)
	}
	if n == 0 {
		return store.User{}, store.ErrNotFound
	}
	return d.GetUser(ctx, id)
}

// UpdatePasswordHash stores a new password hash. Verifying the old password and
// producing the new hash both belong to the caller.
func (d *DB) UpdatePasswordHash(ctx context.Context, id int64, hash string) error {
	if hash == "" {
		return fmt.Errorf("sqlite: refusing to store an empty password hash")
	}
	res, err := d.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ? WHERE id = ?`, hash, id)
	if err != nil {
		return translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: update password: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// SetAvatar stores a processed PNG. The bytes are expected to have come from
// internal/avatar; nothing here inspects them.
func (d *DB) SetAvatar(ctx context.Context, id int64, png []byte, at time.Time) error {
	if len(png) == 0 {
		return fmt.Errorf("sqlite: refusing to store an empty avatar")
	}
	res, err := d.db.ExecContext(ctx,
		`UPDATE users SET avatar_png = ?, avatar_updated_at = ? WHERE id = ?`,
		png, toText(at), id)
	if err != nil {
		return translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: set avatar: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ClearAvatar removes an account's picture. Both columns are cleared together,
// so HasAvatar and the stored bytes cannot disagree.
func (d *DB) ClearAvatar(ctx context.Context, id int64) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE users SET avatar_png = NULL, avatar_updated_at = '' WHERE id = ?`, id)
	if err != nil {
		return translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: clear avatar: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// GetAvatar returns the stored PNG and when it was set.
func (d *DB) GetAvatar(ctx context.Context, id int64) ([]byte, string, error) {
	var (
		png []byte
		at  string
	)
	err := d.db.QueryRowContext(ctx,
		`SELECT avatar_png, avatar_updated_at FROM users WHERE id = ?`, id).Scan(&png, &at)
	if err != nil {
		return nil, "", translate(err)
	}
	if len(png) == 0 {
		return nil, "", store.ErrNotFound
	}
	return png, at, nil
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
		notifyEmail int
		notifyNtfy  int
	)
	// This restates the user columns rather than reusing userColumns, because
	// they need the "u." qualifier for the join. Any column added to a User
	// must be added here too: the authenticated user on every request comes
	// from this query, and a field missing here is silently zero everywhere in
	// the interface. That is exactly how the avatar first failed to appear.
	err := d.db.QueryRowContext(ctx,
		`SELECT s.token_hash, s.user_id, s.created_at, s.expires_at, s.last_seen_at,
                u.id, u.email, u.display_name, u.password_hash, u.is_admin, u.created_at,
                u.deactivated_at, u.avatar_updated_at, u.ntfy_topic, u.notify_email, u.notify_ntfy
         FROM sessions s
         JOIN users u ON u.id = s.user_id
         WHERE s.token_hash = ?
           AND s.expires_at > ?
           AND u.deactivated_at IS NULL`,
		tokenHash, nowText()).
		Scan(&s.TokenHash, &s.UserID, &created, &expires, &lastSeen,
			&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &isAdmin, &uCreated,
			&deactivated, &u.AvatarUpdatedAt, &u.NtfyTopic, &notifyEmail, &notifyNtfy)
	if err != nil {
		return store.Session{}, store.User{}, translate(err)
	}
	u.NotifyEmail = notifyEmail != 0
	u.NotifyNtfy = notifyNtfy != 0
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

// DeleteSessionsForUserExcept ends every other session a user holds.
//
// A password change runs this so a device that is no longer trusted loses its
// access. The current session is kept by token hash rather than by recency,
// since "the newest session" is not reliably the one making the request.
func (d *DB) DeleteSessionsForUserExcept(ctx context.Context, userID int64, keepTokenHash string) (int, error) {
	res, err := d.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, userID, keepTokenHash)
	if err != nil {
		return 0, translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: delete other sessions: %w", err)
	}
	return int(n), nil
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

// SetUserActive deactivates or reactivates an account (AUTH-04).
//
// The last-admin guard runs inside the same transaction as the write. Doing it
// as a check-then-act in the handler would let two concurrent requests each
// observe a second active admin and both proceed, leaving the instance with no
// administrator and no way back in.
func (d *DB) SetUserActive(ctx context.Context, id int64, active bool) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin set active: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if !active {
		// Refuse to remove the last administrator. The check and the write share
		// this transaction so they cannot be split by a concurrent one -- but on
		// a server that runs transactions in parallel, "cannot be split" needs a
		// lock to be true. Two people each deactivating a different admin would
		// otherwise each count the other as still active and both proceed,
		// leaving zero.
		//
		// So lock every active admin row, in id order, before deciding. Two
		// concurrent deactivations then acquire the SAME set in the SAME order:
		// one waits for the other rather than deadlocking, and the second one
		// re-reads a set with the first's change already applied. On SQLite the
		// single writer serializes regardless and lockRows() is empty.
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM users WHERE is_admin = 1 AND deactivated_at IS NULL ORDER BY id`+
				d.dialect.lockRows())
		if err != nil {
			return translate(err)
		}
		var targetIsAdmin bool
		var otherAdmins int
		for rows.Next() {
			var adminID int64
			if err := rows.Scan(&adminID); err != nil {
				_ = rows.Close()
				return translate(err)
			}
			if adminID == id {
				targetIsAdmin = true
			} else {
				otherAdmins++
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return translate(err)
		}
		// The cursor must close before the UPDATE below: SQLite's single
		// connection cannot execute a write while a read cursor is open on it.
		_ = rows.Close()

		if targetIsAdmin && otherAdmins == 0 {
			return store.ErrLastAdmin
		}
	}

	var when sql.NullString
	if !active {
		when = sql.NullString{String: nowText(), Valid: true}
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE users SET deactivated_at = ? WHERE id = ?`, when, id)
	if err != nil {
		return translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: set active rows: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}

	// A deactivated account's sessions must stop working immediately rather
	// than lingering until they expire.
	if !active {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
			return translate(err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit set active: %w", err)
	}
	return nil
}

// SetNotifyPrefs replaces a user's delivery preferences. The topic is validated
// by the caller (notify.ValidTopic) before it reaches here.
func (d *DB) SetNotifyPrefs(ctx context.Context, userID int64, ntfyTopic string, email, ntfy bool) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE users SET ntfy_topic = ?, notify_email = ?, notify_ntfy = ? WHERE id = ?`,
		ntfyTopic, boolToInt(email), boolToInt(ntfy), userID)
	if err != nil {
		return translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: set notify prefs: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ClaimSent records a delivered notification, reporting whether this call made
// the claim. The primary key makes it at-most-once per (tab, event, channel);
// a duplicate insert is a conflict, not this caller's claim.
func (d *DB) ClaimSent(ctx context.Context, tabID int64, eventKey, channel string, userID int64) (bool, error) {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO sent_notifications (tab_id, event_key, channel, user_id, sent_at)
		 VALUES (?, ?, ?, ?, ?)`,
		tabID, eventKey, channel, userID, nowText())
	if err != nil {
		if errors.Is(translate(err), store.ErrConflict) {
			return false, nil
		}
		return false, translate(err)
	}
	return true, nil
}

// WasSent reports whether a notification event has already gone out on a channel.
func (d *DB) WasSent(ctx context.Context, tabID int64, eventKey, channel string) (bool, error) {
	var one int
	err := d.db.QueryRowContext(ctx,
		`SELECT 1 FROM sent_notifications WHERE tab_id = ? AND event_key = ? AND channel = ?`,
		tabID, eventKey, channel).Scan(&one)
	if err != nil {
		if errors.Is(translate(err), store.ErrNotFound) {
			return false, nil
		}
		return false, translate(err)
	}
	return true, nil
}

// SetDelivery replaces the instance's non-secret notification settings.
//
// It stores no credentials, and there is nothing here to guard against one
// arriving: the columns for a password and a token do not exist (migration
// 0010). A caller that wants to set a secret has to change the schema first,
// which is the point at which the reasoning there gets re-read.
func (d *DB) SetDelivery(ctx context.Context, s store.Delivery) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE instance
		    SET smtp_host = ?, smtp_port = ?, smtp_username = ?, email_from = ?, ntfy_url = ?
		  WHERE id = 1`,
		s.SMTPHost, s.SMTPPort, s.SMTPUsername, s.EmailFrom, s.NtfyBaseURL)
	return translate(err)
}

// ListInstanceReminders returns the instance-wide default reminders, longest
// lead first -- the order they fire in.
func (d *DB) ListInstanceReminders(ctx context.Context) ([]store.TabReminder, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT days, title, body FROM instance_reminders ORDER BY days DESC`)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.TabReminder
	for rows.Next() {
		var r store.TabReminder
		if err := rows.Scan(&r.Days, &r.Title, &r.Body); err != nil {
			return nil, fmt.Errorf("sqlite: scan instance reminder: %w", err)
		}
		out = append(out, r)
	}
	return out, translate(rows.Err())
}

// SetInstanceReminders replaces the instance-wide defaults in one transaction,
// for the same reason SetTabReminders does: the set is the unit an
// administrator edits, and clearing it must actually clear it.
func (d *DB) SetInstanceReminders(ctx context.Context, rs []store.TabReminder) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin set instance reminders: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM instance_reminders`); err != nil {
		return translate(err)
	}
	for _, r := range rs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO instance_reminders (days, title, body) VALUES (?, ?, ?)`,
			r.Days, r.Title, r.Body); err != nil {
			return translate(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit set instance reminders: %w", err)
	}
	return nil
}
