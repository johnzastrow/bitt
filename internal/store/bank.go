package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/johnzastrow/bitt/internal/bankcsv"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/reconcile"
)

// Bank reconciliation storage (SPEC-BANK-RECONCILE). These tables sit beside
// the ledger and never inside it: nothing here changes an entry.

// BankFormat is a saved column mapping for one export layout, recognised by
// the signature of its header row.
type BankFormat struct {
	ID        int64
	Name      string
	Signature string
	// HeaderText is the header row as it appeared, for display.
	HeaderText string
	Mapping    bankcsv.Mapping
	CreatedBy  int64
	CreatedAt  time.Time
}

// BankImport is one uploaded file and what happened to its rows.
type BankImport struct {
	ID           int64
	FormatID     int64
	FormatName   string // read only
	FileName     string
	FileSHA256   string
	UploadedBy   int64
	UploaderName string // read only
	UploadedAt   time.Time

	// Kept is lines stored; Duplicates, lines already imported from another
	// file (or earlier in a race); Outgoing, money out or zero, counted and
	// dropped; Ignored, lines stored as ignored by a rule; Refused, rows that
	// could not be read.
	Kept       int
	Duplicates int
	Outgoing   int
	Ignored    int
	Refused    int

	// FirstDate and LastDate bound the file's incoming lines, "YYYY-MM-DD", or
	// "" when there were none.
	FirstDate string
	LastDate  string
	// RefusedDetail is the first few refusal reasons, one per line.
	RefusedDetail string
}

// BankLineState is where a bank line stands.
type BankLineState string

const (
	BankLineOpen     BankLineState = "open"
	BankLineMatched  BankLineState = "matched"
	BankLineRecorded BankLineState = "recorded"
	BankLineIgnored  BankLineState = "ignored"
)

// BankLine is one incoming line of an import, kept forever.
type BankLine struct {
	ID       int64
	ImportID int64
	// Row is the row number in the file (header = 1); Raw the row's original
	// text, so a match can always be shown against what the bank exported.
	Row          int
	Raw          string
	Account      string
	PostedOn     string // "YYYY-MM-DD"
	Amount       money.Cents
	Description  string
	Note         string // verbatim
	Reference    string
	Fingerprint  string
	State        BankLineState
	IgnoreReason string
}

// BankStore covers bank statement imports.
type BankStore interface {
	// BankFormatBySignature finds the saved mapping for a header, or
	// ErrNotFound.
	BankFormatBySignature(ctx context.Context, signature string) (BankFormat, error)
	GetBankFormat(ctx context.Context, id int64) (BankFormat, error)
	// CreateBankFormat saves a mapping. ErrConflict when the signature is
	// already saved (two people mapping the same new layout at once).
	CreateBankFormat(ctx context.Context, f BankFormat) (BankFormat, error)
	ListBankFormats(ctx context.Context) ([]BankFormat, error)

	// SaveBankImport stores an import and its lines in one transaction. A line
	// whose fingerprint is already stored is skipped and counted in
	// Duplicates; every other line is stored and counted in Kept, or in
	// Ignored when its State is BankLineIgnored. The caller fills Outgoing,
	// Refused and the rest; the returned import has every count.
	SaveBankImport(ctx context.Context, imp BankImport, lines []BankLine) (BankImport, error)
	GetBankImport(ctx context.Context, id int64) (BankImport, error)
	// ListBankImports returns the most recent imports first.
	ListBankImports(ctx context.Context, limit int) ([]BankImport, error)
	// ListBankLines returns an import's stored lines in file order.
	ListBankLines(ctx context.Context, importID int64) ([]BankLine, error)

	// GetReconcileSettings returns the setup controls and who last changed
	// them (UpdatedBy 0 and a zero time when nobody has).
	GetReconcileSettings(ctx context.Context) (ReconcileSettings, error)
	// SetReconcileSettings replaces the setup controls. The caller validates;
	// the schema refuses out-of-range values regardless.
	SetReconcileSettings(ctx context.Context, s reconcile.Settings, by int64) error

	ListIgnoreRules(ctx context.Context) ([]IgnoreRule, error)
	// AddIgnoreRule saves a rule and, in the same transaction, marks the
	// layout's still-open lines that match it as ignored with the rule as the
	// reason, returning how many. ErrConflict for a rule already saved.
	AddIgnoreRule(ctx context.Context, r IgnoreRule) (IgnoreRule, int, error)
	// DeleteIgnoreRule removes a rule; lines it ignored stay ignored.
	DeleteIgnoreRule(ctx context.Context, id int64) error

	// ListOpenBankLines returns every line still open, with its layout, for
	// suggestions.
	ListOpenBankLines(ctx context.Context) ([]OpenBankLine, error)
	// GetBankLine reads one line, in any state, with its layout and file.
	GetBankLine(ctx context.Context, id int64) (OpenBankLine, error)
	// ListPaymentCandidates returns every unreversed payment entry, on any
	// tab, effective in [from, to), with its tab's name and participants.
	ListPaymentCandidates(ctx context.Context, from, to time.Time) ([]PaymentCandidate, error)

	// ConfirmBankMatch records a match and posts its delta entry, if any, in
	// one transaction (RECON-04). Callers go through the ledger, which builds
	// the delta; the delta's idempotency key is set here, from the new match's
	// id. The line must be open and the payment an unreversed, unmatched,
	// non-reconciliation payment on m.TabID.
	//
	// Confirming the same pair again returns the standing match with
	// replayed=true and posts nothing. ErrLineNotOpen and ErrPaymentMatched
	// report the line or payment taken by something else.
	ConfirmBankMatch(ctx context.Context, m NewBankMatch, delta *NewEntry) (BankMatch, bool, error)
	// UndoBankMatch marks a standing match undone, posts the reversal of its
	// delta (built by the ledger; nil when there was none), and reopens the
	// line, in one transaction. ErrMatchUndone when it was already undone.
	UndoBankMatch(ctx context.Context, matchID, by int64, reversal *NewEntry) error
	GetBankMatch(ctx context.Context, id int64) (BankMatch, error)
	// EntryHasActiveMatch reports whether a payment has a standing match.
	EntryHasActiveMatch(ctx context.Context, seq int64) (bool, error)
	// ListBankMatches returns matches newest first, undone ones included,
	// filtered by tab or import when the id is non-zero.
	ListBankMatches(ctx context.Context, f BankMatchFilter) ([]BankMatch, error)
}

// Reconciliation errors.
var (
	ErrLineNotOpen    = errors.New("store: the bank line is not open")
	ErrPaymentMatched = errors.New("store: the payment is already matched")
	ErrNotAPayment    = errors.New("store: the entry is not an open payment on that tab")
	ErrMatchUndone    = errors.New("store: the match is already undone")
)

// ReconKeyPrefix starts the idempotency key of every entry reconciliation
// posts. Such entries are not offered for matching and are undone from the
// Reconciliation screen, not the tab.
const ReconKeyPrefix = "recon"

// IsReconciliation reports whether an entry was posted by reconciliation.
func IsReconciliation(e Entry) bool { return strings.HasPrefix(e.IdempotencyKey, ReconKeyPrefix+":") }

// NewBankMatch is what confirming records.
type NewBankMatch struct {
	LineID         int64
	EntrySeq       int64
	TabID          int64
	BankDate       string // YYYY-MM-DD
	BankAmount     money.Cents
	RecordedDate   string // YYYY-MM-DD, in the instance timezone
	RecordedAmount money.Cents
	DateFlagged    bool
	DateFlagReason string
	NoteInMemo     bool
	ConfirmedBy    int64
}

// BankMatch is a confirmed match with what it points at.
type BankMatch struct {
	NewBankMatch
	ID            int64
	ImportID      int64
	DeltaEntrySeq *int64
	ConfirmedAt   time.Time
	UndoneAt      *time.Time
	UndoneBy      int64

	// Joined for display.
	ConfirmedByName string
	TabName         string
	FileName        string
	Row             int
	Raw             string
	Description     string
	Note            string
	DeltaAmount     money.Cents // signed as posted; 0 when none
	DeltaKind       EntryKind
}

// Active reports whether the match stands.
func (m BankMatch) Active() bool { return m.UndoneAt == nil }

// BankMatchFilter narrows ListBankMatches.
type BankMatchFilter struct {
	TabID    int64
	ImportID int64
	Limit    int
}

// ReconcileSettings is the stored setup with who changed it last.
type ReconcileSettings struct {
	reconcile.Settings
	UpdatedBy     int64
	UpdatedByName string
	UpdatedAt     time.Time
}

// IgnoreRule marks a layout's lines whose description contains Contains
// (ignoring case) as not BitTabby's, at import.
type IgnoreRule struct {
	ID         int64
	FormatID   int64
	FormatName string // read only
	Contains   string
	CreatedBy  int64
	CreatedAt  time.Time
}

// Matches reports whether a description falls under the rule.
func (r IgnoreRule) Matches(description string) bool {
	return strings.Contains(strings.ToLower(description), strings.ToLower(r.Contains))
}

// Reason is what an ignored line records.
func (r IgnoreRule) Reason() string {
	return "description contains \"" + r.Contains + "\""
}

// OpenBankLine is an open line with the layout it came through.
type OpenBankLine struct {
	BankLine
	FormatID int64
	FileName string
}

// PaymentCandidate is a payment that a bank line might be.
type PaymentCandidate struct {
	Seq         int64
	TabID       int64
	TabName     string
	Amount      money.Cents
	Method      PaymentMethod
	EffectiveAt time.Time
	Memo        string
	ActorUserID int64
	ActorName   string
	// Participants are the tab's people's display names.
	Participants []string
}
