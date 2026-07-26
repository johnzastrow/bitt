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

	"github.com/johnzastrow/bitt/internal/fee"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
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
	// ErrLastAdmin is returned when deactivating an account would leave the
	// instance with no active administrator.
	ErrLastAdmin = errors.New("store: cannot deactivate the last administrator")
)

// EntryCategory sub-types an entry within its kind. It is empty for ordinary
// entries; interest is a charge carrying CategoryInterest, so it can be told
// apart from loan principal without a new entry kind.
type EntryCategory string

const (
	// CategoryNone is the ordinary, unqualified entry.
	CategoryNone EntryCategory = ""
	// CategoryInterest marks a charge as periodic loan interest (Payoff tabs).
	CategoryInterest EntryCategory = "interest"
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

// Label names the kind for display, in the vocabulary REQUIREMENTS.md and
// PROJECT.md already use.
func (k TabKind) Label() string {
	switch k {
	case TabServices:
		return "Services"
	case TabPayoff:
		return "Payoff"
	default:
		return ""
	}
}

// Describe explains the kind in the one line that distinguishes them.
func (k TabKind) Describe() string {
	switch k {
	case TabServices:
		return "Recurring, with no defined end"
	case TabPayoff:
		return "A fixed total drawn down by payments"
	default:
		return ""
	}
}

// TabKinds lists the selectable kinds in display order.
func TabKinds() []TabKind { return []TabKind{TabServices, TabPayoff} }

// Role is a user's relationship to a specific tab.
type Role string

const (
	// RoleProvider bills on the tab.
	RoleProvider Role = "provider"
	// RolePayee pays on the tab.
	RolePayee Role = "payee"
	// RoleAdmin is a per-tab administrator: a member who can manage the tab's
	// settings, schedule, items, and people, and who transacts on it as a
	// member, without being its Provider (the single biller). It is added when a
	// household wants a second person to help run a tab.
	RoleAdmin Role = "admin"
)

// Valid reports whether r is a role the app assigns.
func (r Role) Valid() bool {
	return r == RoleProvider || r == RolePayee || r == RoleAdmin
}

// PaymentMethod records how money actually moved, since it moves outside
// BitTabby (PAY-02).
type PaymentMethod string

const (
	// MethodNone applies to entries that are not payments.
	MethodNone PaymentMethod = ""
	// MethodCash is cash handed over in person.
	MethodCash PaymentMethod = "cash"
	// MethodTransfer covers bank transfer, Venmo, Zelle, and the like.
	MethodTransfer PaymentMethod = "transfer"
	// MethodOther is anything else, described in the memo.
	MethodOther PaymentMethod = "other"
)

// Valid reports whether m is a recognized payment method.
func (m PaymentMethod) Valid() bool {
	switch m {
	case MethodNone, MethodCash, MethodTransfer, MethodOther:
		return true
	}
	return false
}

// Label renders the method for display.
func (m PaymentMethod) Label() string {
	switch m {
	case MethodCash:
		return "Cash"
	case MethodTransfer:
		return "Transfer"
	case MethodOther:
		return "Other"
	default:
		return ""
	}
}

// PaymentMethods lists the selectable methods in display order.
func PaymentMethods() []PaymentMethod {
	return []PaymentMethod{MethodCash, MethodTransfer, MethodOther}
}

// Instance holds deployment-wide state. Exactly one row exists.
type Instance struct {
	Timezone         string
	SetupCompletedAt *time.Time
	CreatedAt        time.Time
	// Delivery is the notification setup an administrator has entered through
	// the interface. It is the FALLBACK layer: anything the environment
	// specifies wins, and these apply only where the environment is silent.
	//
	// It holds no secrets. The SMTP password, the ntfy token, and the tick
	// secret come from the environment or a file and nowhere else -- see
	// migration 0010 for why that line is drawn here.
	Delivery Delivery
}

// Delivery is the non-secret half of notification configuration: where mail
// goes out through, who it comes from, and which ntfy server is pinned.
type Delivery struct {
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	EmailFrom    string
	NtfyBaseURL  string
}

// Set reports whether any delivery setting has been entered at all.
func (d Delivery) Set() bool {
	return d.SMTPHost != "" || d.SMTPPort != 0 || d.SMTPUsername != "" ||
		d.EmailFrom != "" || d.NtfyBaseURL != ""
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
	// AvatarUpdatedAt is when the picture last changed, or "" for none. It is
	// the ETag the avatar route serves, so a browser can revalidate cheaply.
	AvatarUpdatedAt string
	// NtfyTopic is the user's ntfy topic (the only user-controlled part of an
	// ntfy destination; the server is admin-pinned). Empty means unset.
	NtfyTopic string
	// NotifyEmail and NotifyNtfy are the per-channel delivery toggles.
	NotifyEmail bool
	NotifyNtfy  bool
}

// HasAvatar reports whether the account has an uploaded picture. The image
// itself is not carried on User: it is read only by the route that serves it,
// so that resolving a session does not drag a blob through every request.
func (u User) HasAvatar() bool { return u.AvatarUpdatedAt != "" }

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
	// Schedule is the tab's recurrence (SCHED-01). A zero Kind means the tab is
	// billed only by hand, which stays a legitimate way to run one (CHG-03).
	Schedule schedule.Schedule
	// Fee is the tab's late-fee policy (FEE-01, FEE-02, FEE-06). A zero policy
	// means the tab charges no late fee.
	Fee fee.Policy
	// InterestAPRBp is the annual interest rate in basis points on a Payoff
	// loan (100 bp = 1%). Zero means no interest, matching an interest-free IOU.
	InterestAPRBp int64
	// LoanTermPeriods is how many schedule periods a Payoff loan is meant to
	// run for. Zero means open-ended, which is what a plain IOU with no agreed
	// end is, and is the default.
	LoanTermPeriods int
	// LoanPayment is the payment expected each period on a Payoff loan.
	//
	// The Provider enters this rather than the app deriving it, because it
	// comes off the lender's paperwork and that is the figure that actually has
	// to be paid. The app computes a suggestion beside it from the loan amount,
	// rate, term, and schedule, and reports the drift between the two -- which
	// is the honest arrangement, since a lender accruing per diem splits a
	// payment by the days between deposits and no period-based model can match
	// that to the cent.
	//
	// On a Services tab this is unused; that tab's period charge is the sum of
	// its line items. Keeping them separate is deliberate: on a Payoff tab the
	// loan amount is a charge that posts the principal once, while this is an
	// expectation that posts nothing, and one field meaning both is what made
	// a mis-set loan read as already settled.
	LoanPayment money.Cents
}

// Interest reports whether the tab carries loan interest.
func (t Tab) Interest() bool { return t.InterestAPRBp > 0 }

// Termed reports whether a Payoff loan has an agreed number of periods, which
// is what makes a suggested payment and a maturity date computable.
func (t Tab) Termed() bool { return t.Kind == TabPayoff && t.LoanTermPeriods > 0 }

// PostedFee records that one overdue date has been assessed a late fee, and
// points at the entry that charged it. It is the claim that makes fee
// assessment happen at most once per date (FEE-04).
type PostedFee struct {
	TabID    int64
	Key      string
	EntrySeq int64
	// AssessedFor is the overdue date the fee answers to.
	AssessedFor schedule.Date
	// Base is the overdue amount the fee was computed on, kept so a percentage
	// fee can be shown to have been taken on the charge and not on a balance
	// (FEE-05).
	Base     money.Cents
	PostedAt time.Time
}

// PostedInterest records that one period has accrued interest, and points at
// the interest entry. It is the claim that makes interest accrue at most once
// per period, the declining-balance counterpart of PostedFee.
type PostedInterest struct {
	TabID    int64
	Key      string
	EntrySeq int64
	// AccruedFor is the period date the interest answers to.
	AccruedFor schedule.Date
	// Base is the outstanding balance the interest was computed on, kept so a
	// declining charge can be shown against the balance it was taken on.
	Base     money.Cents
	PostedAt time.Time
}

// PostedPeriod records that one billing cycle has been charged, and points at
// the entry that charged it. It is the claim that makes lazy accrual safe under
// concurrent reads (SCHED-04).
type PostedPeriod struct {
	TabID    int64
	Key      string
	EntrySeq int64
	// Start and End bound the cycle; DueOn is when its charge was owed. All
	// three are captured at post time so a later schedule change cannot rewrite
	// what a cycle was billed under (SCHED-05).
	Start    schedule.Date
	End      schedule.Date
	DueOn    schedule.Date
	PostedAt time.Time
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

// TabReminder is one of a tab's own payment reminders: how many days before a
// due date it fires, and the message it sends.
//
// It is the per-tab override of the instance-wide config.Reminder, and carries
// the same fields for that reason -- the render path takes either. A tab with
// no reminders of its own falls back to the instance list, so this type never
// needs an "inherit" state.
//
// Title and Body are Provider-supplied text that reaches a mail header once
// {tab} and friends are substituted, which is why the handler validates them
// and internal/notify checks again at send time.
type TabReminder struct {
	Days  int
	Title string
	Body  string
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
	// AvatarUpdatedAt lets a participant row show the person's picture without
	// a second query per row. Empty means no picture, exactly as on User.
	AvatarUpdatedAt string
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
	Method         PaymentMethod
	// Category sub-types the entry; CategoryInterest marks loan interest.
	Category EntryCategory
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
	Method         PaymentMethod
	Category       EntryCategory
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

	// SetDelivery replaces the instance's non-secret notification settings. It
	// stores no credentials; those come from the environment (migration 0010).
	SetDelivery(ctx context.Context, d Delivery) error

	// ListInstanceReminders returns the instance-wide default reminders, longest
	// lead first. Empty means none have been set through the interface, and the
	// environment's list -- or the built-in one -- applies.
	ListInstanceReminders(ctx context.Context) ([]TabReminder, error)

	// SetInstanceReminders replaces the instance-wide defaults in one
	// transaction. An empty set clears them, which returns the instance to the
	// environment's list or the built-in default.
	SetInstanceReminders(ctx context.Context, rs []TabReminder) error
}

// UserStore covers accounts.
type UserStore interface {
	CreateUser(ctx context.Context, u User) (User, error)
	GetUser(ctx context.Context, id int64) (User, error)
	// GetUserByEmail matches case-insensitively.
	GetUserByEmail(ctx context.Context, email string) (User, error)
	ListUsers(ctx context.Context) ([]User, error)
	CountUsers(ctx context.Context) (int, error)

	// UpdateProfile changes an account's display name and email.
	//
	// The email is the login identity, so callers must have re-authenticated
	// the account first. Returns ErrConflict if another account already holds
	// the address, case-insensitively.
	UpdateProfile(ctx context.Context, id int64, displayName, email string) (User, error)

	// UpdatePasswordHash replaces an account's password hash. The caller
	// verifies the current password and hashes the new one; this only stores.
	UpdatePasswordHash(ctx context.Context, id int64, hash string) error

	// SetAvatar stores a processed image, replacing any existing one. The bytes
	// must already have been through internal/avatar -- nothing here validates
	// them, and storing an uploader's bytes directly is the mistake this
	// contract is shaped to prevent.
	SetAvatar(ctx context.Context, id int64, png []byte, at time.Time) error

	// ClearAvatar removes an account's picture.
	ClearAvatar(ctx context.Context, id int64) error

	// GetAvatar returns the stored PNG and the timestamp it was set, or
	// ErrNotFound when the account has none. It is the only read that touches
	// the image, which is why it is separate from GetUser.
	GetAvatar(ctx context.Context, id int64) ([]byte, string, error)

	// SetNotifyPrefs replaces a user's delivery preferences.
	SetNotifyPrefs(ctx context.Context, userID int64, ntfyTopic string, email, ntfy bool) error

	// ClaimSent records that one notification event was delivered to one user on
	// one channel, and reports whether this call made the claim (true) or it
	// already existed (false). It is written AFTER a confirmed send, in its own
	// transaction, never inside a ledger transaction -- the at-least-once
	// guarantee of Phase 5 (D2).
	ClaimSent(ctx context.Context, tabID int64, eventKey, channel string, userID int64) (bool, error)

	// WasSent reports whether a notification event has already gone to a user on
	// a channel, so the scan can skip it without attempting delivery.
	WasSent(ctx context.Context, tabID int64, eventKey, channel string) (bool, error)

	// SetUserActive deactivates or reactivates an account (AUTH-04).
	//
	// Deactivating the last active admin must fail with ErrLastAdmin, checked
	// inside the same transaction as the write. A check-then-act in the handler
	// would let two concurrent requests each see a second admin and both
	// proceed, locking everyone out of the instance.
	SetUserActive(ctx context.Context, id int64, active bool) error
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
	// DeleteSessionsForUserExcept ends every session for a user other than the
	// one given, and reports how many it ended.
	//
	// This is what a password change is usually for: revoking a device that is
	// no longer trusted. Keeping the current session is what stops the person
	// making the change from being logged out by their own action.
	DeleteSessionsForUserExcept(ctx context.Context, userID int64, keepTokenHash string) (int, error)
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error)
}

// TabStore covers tabs, their items, and their participants.
type TabStore interface {
	CreateTab(ctx context.Context, t Tab, items []TabItem) (Tab, error)
	GetTab(ctx context.Context, id int64) (Tab, error)
	// ListTabsForUser returns only tabs the user participates in. Authorization
	// is enforced by the query rather than filtered afterward (AUTH-05).
	ListTabsForUser(ctx context.Context, userID int64) ([]Tab, error)
	// ListAllTabs returns every tab, for instance-wide scans (the notification
	// tick). It is not an authorization bypass: only server-internal, non-user
	// code paths call it.
	ListAllTabs(ctx context.Context) ([]Tab, error)

	// UpdateTabDetails changes a tab's name, description, and kind.
	//
	// None of the three is referenced by any entry, so this cannot disturb a
	// balance: switching a tab between Services and Payoff changes how it is
	// presented and measured, never what it has charged. Callers are expected
	// to log the change, since a tab changing kind is the sort of thing that
	// needs explaining later.
	UpdateTabDetails(ctx context.Context, tabID int64, name, description string, kind TabKind) error

	// SetTabArchived retires a tab from the active dashboard, or brings it
	// back. Archiving is not deletion: the entries, the balance, and the
	// history all stay exactly as they were, and an archived tab stops
	// accruing scheduled charges.
	SetTabArchived(ctx context.Context, tabID int64, archived bool) error

	// SetSchedule replaces a tab's recurrence (SCHED-01). A zero schedule
	// clears it, returning the tab to manual billing. It never touches posted
	// periods, so changing a schedule cannot retroactively re-bill.
	SetSchedule(ctx context.Context, tabID int64, s schedule.Schedule) error

	// SetFeePolicy replaces a tab's late-fee policy (FEE-01, FEE-02, FEE-06). A
	// zero policy clears it. It never touches posted fees, so changing the
	// policy cannot retroactively re-assess or unwind a fee already charged.
	SetFeePolicy(ctx context.Context, tabID int64, p fee.Policy) error

	// SetInterestRate replaces a Payoff loan's annual interest rate, in basis
	// points. Zero clears it. It never touches posted interest, so changing the
	// rate affects future periods only.
	SetInterestRate(ctx context.Context, tabID int64, annualBasisPoints int64) error

	// SetLoanTerms replaces a Payoff loan's term and scheduled payment. A term
	// of zero returns the loan to open-ended; a payment of zero clears the
	// expectation, which stops payoff status and late fees from judging it.
	//
	// Like the other setters here it touches no claim table, so a Provider
	// truing the payment up to what the bank actually charges reaches future
	// periods only. Interest and fees already posted stand as recorded --
	// re-deriving them from a corrected payment would rewrite history the
	// ledger deliberately makes immutable.
	SetLoanTerms(ctx context.Context, tabID int64, termPeriods int, payment money.Cents) error

	// ListTabReminders returns a tab's own payment reminders, soonest lead time
	// last (14, 7, 1 -- the order they fire in). An empty result means the tab
	// has not been customised and the instance defaults apply.
	ListTabReminders(ctx context.Context, tabID int64) ([]TabReminder, error)

	// SetTabReminders replaces a tab's reminders with the given set, in one
	// transaction. An empty set clears them, returning the tab to the instance
	// defaults -- which is how a Provider un-customises a tab, and why this
	// replaces rather than merges.
	//
	// It touches no claim table. sent_notifications is keyed on
	// (tab, event, channel) and the event key carries the due date and the lead
	// time, not the message text, so editing a reminder can never make an
	// already-delivered notice send again.
	SetTabReminders(ctx context.Context, tabID int64, rs []TabReminder) error

	ListItems(ctx context.Context, tabID int64) ([]TabItem, error)
	// ListItemHistory returns every item the tab has ever carried, superseded
	// and retired ones included. Catching up a tab that has been left alone
	// needs to know what each period's items were at the time, not what they
	// are now (CHG-02).
	ListItemHistory(ctx context.Context, tabID int64) ([]TabItem, error)
	GetItem(ctx context.Context, itemID int64) (TabItem, error)
	AddItem(ctx context.Context, item TabItem) (TabItem, error)

	// UpdateItem changes a line item's name or amount (CHG-02).
	//
	// Implementations supersede rather than overwrite: the existing row is
	// marked removed and a replacement takes its position. Posted entries are
	// unaffected either way, because each carries its own item snapshot -- but
	// superseding also keeps the tab's own record of what it used to charge,
	// instead of quietly losing it.
	UpdateItem(ctx context.Context, itemID int64, name string, amount money.Cents) (TabItem, error)
	// RemoveItem retires a line item from future periods, leaving every period
	// already posted exactly as it was.
	RemoveItem(ctx context.Context, itemID int64) error

	AddParticipant(ctx context.Context, p Participant) error
	RemoveParticipant(ctx context.Context, tabID, userID int64) error
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

	// PostPeriodEntry appends a scheduled charge and claims its billing cycle
	// in the same transaction (SCHED-03).
	//
	// The claim is what enforces "exactly once" (SCHED-04). Implementations
	// must write the entry and the claim together, so that losing the race on
	// the claim also rolls back the entry -- a claim written afterward, or in a
	// separate transaction, leaves a window in which two readers each post a
	// charge. A cycle already claimed returns the entry that claimed it with
	// replayed=true, exactly as a replayed idempotency key does.
	PostPeriodEntry(ctx context.Context, p PostedPeriod, e NewEntry) (posted Entry, replayed bool, err error)
	// ListPostedPeriods returns a tab's charged cycles, newest first.
	ListPostedPeriods(ctx context.Context, tabID int64) ([]PostedPeriod, error)

	// PostFeeEntry appends a late-fee entry and claims the overdue date it
	// answers to, in the same transaction (FEE-03, FEE-04).
	//
	// The claim is what enforces "at most one fee per date". Like
	// PostPeriodEntry, the entry and the claim share a transaction, so losing
	// the race on the claim rolls back the fee. A date already assessed returns
	// the entry that assessed it with replayed=true.
	PostFeeEntry(ctx context.Context, f PostedFee, e NewEntry) (posted Entry, replayed bool, err error)
	// ListPostedFees returns a tab's assessed fees, newest first.
	ListPostedFees(ctx context.Context, tabID int64) ([]PostedFee, error)

	// PostInterestEntry appends a periodic interest charge and claims the
	// period it accrued for, in one transaction. The declining-balance
	// counterpart of PostFeeEntry: same exactly-once guarantee, same replay
	// semantics.
	PostInterestEntry(ctx context.Context, in PostedInterest, e NewEntry) (posted Entry, replayed bool, err error)
	// ListPostedInterest returns a tab's accrued interest periods, newest first.
	ListPostedInterest(ctx context.Context, tabID int64) ([]PostedInterest, error)

	GetEntry(ctx context.Context, seq int64) (Entry, error)
	ListEntries(ctx context.Context, tabID int64) ([]Entry, error)
	ListEntryItems(ctx context.Context, entrySeq int64) ([]EntryItem, error)
	// ListEntryItemsForTab returns every entry's item snapshot for one tab,
	// keyed by entry sequence. A statement page needs the breakdown of many
	// entries at once, and one query is preferable to one per period (CHG-04).
	ListEntryItemsForTab(ctx context.Context, tabID int64) (map[int64][]EntryItem, error)
	// SumEntries derives the tab balance by summation. There is no cached
	// balance column anywhere in the schema (LEDGER-03).
	SumEntries(ctx context.Context, tabID int64) (money.Cents, error)
}
