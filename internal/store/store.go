// Package store defines BitTabby's persistence contract.
//
// DEPLOY-02: every SQL statement lives behind these interfaces. Call sites
// depend on this package rather than on any driver, so adding MariaDB in Phase 5
// means adding an implementation, not editing handlers. Nothing here exposes a
// dialect-specific type, and no method promises SQLite-only behavior such as
// UPDATE ... RETURNING.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
)

// Sentinel errors. Implementations must translate driver errors into these so
// that handlers never inspect a driver-specific error type.
var (
	// ErrNotFound is returned when a lookup matches no row.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict is returned when a uniqueness constraint rejects a write --
	// a duplicate email, or a replayed idempotency key.
	ErrConflict = errors.New("store: conflict")
	// ErrAppendOnly is returned when something attempts to modify or remove a
	// posted ledger entry (LEDGER-01).
	ErrAppendOnly = errors.New("store: entries are append-only")
)

// EntryKind classifies a ledger entry.
type EntryKind string

const (
	// KindCharge increases what the Payee owes.
	KindCharge EntryKind = "charge"
	// KindPayment reduces what the Payee owes.
	KindPayment EntryKind = "payment"
	// KindAdjustment corrects a balance outside the normal charge path.
	KindAdjustment EntryKind = "adjustment"
	// KindFee is a late fee (Phase 4).
	KindFee EntryKind = "fee"
	// KindReversal undoes an earlier entry (LEDGER-02).
	KindReversal EntryKind = "reversal"
)

// Valid reports whether k is a recognized entry kind.
func (k EntryKind) Valid() bool {
	switch k {
	case KindCharge, KindPayment, KindAdjustment, KindFee, KindReversal:
		return true
	}
	return false
}

// TabKind distinguishes the two tab types.
type TabKind string

const (
	// TabServices is a recurring tab with no defined end (TAB-01).
	TabServices TabKind = "services"
	// TabPayoff is a fixed total drawn down by payments (TAB-02, Phase 4).
	TabPayoff TabKind = "payoff"
)

// Valid reports whether k is a recognized tab kind.
func (k TabKind) Valid() bool {
	return k == TabServices || k == TabPayoff
}

// Role is a user's relationship to a specific tab.
type Role string

const (
	// RoleProvider bills on the tab.
	RoleProvider Role = "provider"
	// RolePayee pays on the tab.
	RolePayee Role = "payee"
)

// Instance holds deployment-wide state. Exactly one row exists.
type Instance struct {
	Timezone         string
	SetupCompletedAt *time.Time
	CreatedAt        time.Time
}

// SetupComplete reports whether the first-run screen has already been used
// (AUTH-03). Once true it never returns to false.
func (i Instance) SetupComplete() bool { return i.SetupCompletedAt != nil }

// User is an account. PasswordHash is a full Argon2id PHC string.
type User struct {
	ID            int64
	Email         string
	DisplayName   string
	PasswordHash  string
	IsAdmin       bool
	CreatedAt     time.Time
	DeactivatedAt *time.Time
}

// Active reports whether the account may still authenticate.
func (u User) Active() bool { return u.DeactivatedAt == nil }

// Session is a server-side login record. The raw token is never stored; only
// its SHA-256 digest, so a database read cannot be replayed as a login.
type Session struct {
	TokenHash  string
	UserID     int64
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

// Tab is a running account between a Provider and a Payee. It deliberately has
// no balance field: balances are derived by summing entries (LEDGER-03).
type Tab struct {
	ID          int64
	Name        string
	Kind        TabKind
	Description string
	CreatedBy   int64
	CreatedAt   time.Time
	ArchivedAt  *time.Time
}

// TabItem is a line in a tab's breakdown. Items carry an amount so that cost
// changes are visible (TAB-04), but never a balance of their own (TAB-05).
type TabItem struct {
	ID        int64
	TabID     int64
	Name      string
	Amount    money.Cents
	Position  int
	CreatedAt time.Time
	RemovedAt *time.Time
}

// Participant links a user to a tab in a role.
type Participant struct {
	TabID   int64
	UserID  int64
	Role    Role
	AddedAt time.Time
	// Denormalized for display; populated by ListParticipants.
	DisplayName string
	Email       string
}

// Entry is a posted ledger entry. Once written it is never modified or removed.
type Entry struct {
	Seq            int64
	TabID          int64
	Kind           EntryKind
	Amount         money.Cents
	Memo           string
	EffectiveAt    time.Time
	CreatedAt      time.Time
	ActorUserID    int64
	IdempotencyKey string
	ReversesSeq    *int64
}

// EntryItem is the item breakdown captured at the moment an entry was posted,
// so a later change to the tab's items cannot rewrite history (CHG-01).
type EntryItem struct {
	Position int
	Name     string
	Amount   money.Cents
}

// NewEntry is the input to a ledger write. Callers construct it through the
// ledger package rather than passing it to the store directly.
type NewEntry struct {
	TabID          int64
	Kind           EntryKind
	Amount         money.Cents
	Memo           string
	EffectiveAt    time.Time
	ActorUserID    int64
	IdempotencyKey string
	ReversesSeq    *int64
	Items          []EntryItem
}

// Store is the full persistence contract.
type Store interface {
	InstanceStore
	UserStore
	SessionStore
	TabStore
	EntryStore

	// Migrate brings the schema to the current version. Safe to call on every
	// startup and safe to call concurrently (DEPLOY-01).
	Migrate(ctx context.Context) error
	// Close releases the underlying handles.
	Close() error
}

// InstanceStore covers deployment-wide state.
type InstanceStore interface {
	GetInstance(ctx context.Context) (Instance, error)

	// CompleteSetup creates the first admin and latches setup closed in one
	// transaction (AUTH-03). It returns ErrConflict if setup already completed,
	// which is what makes the lock permanent even under a concurrent request.
	CompleteSetup(ctx context.Context, admin User, timezone string) (User, error)
}

// UserStore covers accounts.
type UserStore interface {
	CreateUser(ctx context.Context, u User) (User, error)
	GetUser(ctx context.Context, id int64) (User, error)
	// GetUserByEmail matches case-insensitively.
	GetUserByEmail(ctx context.Context, email string) (User, error)
	ListUsers(ctx context.Context) ([]User, error)
	CountUsers(ctx context.Context) (int, error)
}

// SessionStore covers login state.
type SessionStore interface {
	CreateSession(ctx context.Context, s Session) error
	// GetSession returns the session and its user. It returns ErrNotFound for a
	// session that is missing, expired, or whose user is deactivated, so that
	// callers cannot accidentally honor one (fail closed).
	GetSession(ctx context.Context, tokenHash string) (Session, User, error)
	TouchSession(ctx context.Context, tokenHash string, at time.Time) error
	DeleteSession(ctx context.Context, tokenHash string) error
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error)
}

// TabStore covers tabs, their items, and their participants.
type TabStore interface {
	CreateTab(ctx context.Context, t Tab, items []TabItem) (Tab, error)
	GetTab(ctx context.Context, id int64) (Tab, error)
	// ListTabsForUser returns only tabs the user participates in. Authorization
	// is enforced by the query rather than filtered afterward (AUTH-05).
	ListTabsForUser(ctx context.Context, userID int64) ([]Tab, error)

	ListItems(ctx context.Context, tabID int64) ([]TabItem, error)
	AddItem(ctx context.Context, item TabItem) (TabItem, error)

	AddParticipant(ctx context.Context, p Participant) error
	ListParticipants(ctx context.Context, tabID int64) ([]Participant, error)
	// ParticipantRole returns the user's role on the tab, or ErrNotFound if the
	// user does not participate. This is the authorization primitive.
	ParticipantRole(ctx context.Context, tabID, userID int64) (Role, error)
}

// EntryStore covers the ledger. It has no update or delete method by design:
// the interface itself offers no way to modify a posted entry (LEDGER-01).
type EntryStore interface {
	// PostEntry appends an entry and its item snapshot atomically. A replayed
	// idempotency key returns the already-posted entry with replayed=true
	// rather than writing a second row.
	PostEntry(ctx context.Context, e NewEntry) (posted Entry, replayed bool, err error)
	GetEntry(ctx context.Context, seq int64) (Entry, error)
	ListEntries(ctx context.Context, tabID int64) ([]Entry, error)
	ListEntryItems(ctx context.Context, entrySeq int64) ([]EntryItem, error)
	// SumEntries derives the tab balance by summation. There is no cached
	// balance column anywhere in the schema (LEDGER-03).
	SumEntries(ctx context.Context, tabID int64) (money.Cents, error)
}
