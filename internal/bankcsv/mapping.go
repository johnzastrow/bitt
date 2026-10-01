package bankcsv

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
)

// None marks an unmapped optional column.
const None = -1

// MaxRowBytes bounds one row's original text. A bank row is a few hundred
// bytes; anything near this is not a transaction, and the bound keeps every
// stored cell within a MariaDB TEXT column.
const MaxRowBytes = 16 << 10

// Mapping says how to read one layout. Columns are 0-based indexes into the
// header; the header signature guarantees the same layout every time it is
// applied.
type Mapping struct {
	Date       int
	DateLayout string // a DateLayout key

	// Amount is one signed column, or None when the layout has separate
	// Debit and Credit columns instead.
	Amount int
	Debit  int
	Credit int
	// IncomingNegative is true when this export writes money received as a
	// negative amount (the signed-column case only).
	IncomingNegative bool
	// DecimalComma reads "1.234,56" rather than "1,234.56".
	DecimalComma bool

	Description int
	Account     int // optional
	Note        int // optional
	Reference   int // optional
}

// DateLayout is one of the date formats a person may choose from.
type DateLayout struct {
	Key   string // stored
	Label string // shown
	go1   string // Go layout
}

// DateLayouts are the choices, most common first. Go's numeric layout
// elements accept an optional leading zero, so "9/1/2026" and "09/01/2026"
// both read under M/D/YYYY.
var DateLayouts = []DateLayout{
	{"m/d/yyyy", "M/D/YYYY (9/1/2026)", "1/2/2006"},
	{"d/m/yyyy", "D/M/YYYY (1/9/2026)", "2/1/2006"},
	{"yyyy-mm-dd", "YYYY-MM-DD (2026-09-01)", "2006-01-02"},
	{"m-d-yyyy", "M-D-YYYY (9-1-2026)", "1-2-2006"},
	{"d.m.yyyy", "D.M.YYYY (1.9.2026)", "2.1.2006"},
	{"m/d/yy", "M/D/YY (9/1/26)", "1/2/06"},
	{"d/m/yy", "D/M/YY (1/9/26)", "2/1/06"},
}

func dateLayout(key string) (DateLayout, bool) {
	for _, l := range DateLayouts {
		if l.Key == key {
			return l, true
		}
	}
	return DateLayout{}, false
}

// Validate checks a mapping against a header of ncols columns.
func (m Mapping) Validate(ncols int) error {
	in := func(c int) bool { return c >= 0 && c < ncols }
	opt := func(c int) bool { return c == None || in(c) }

	if !in(m.Date) {
		return errors.New("choose the date column")
	}
	if _, ok := dateLayout(m.DateLayout); !ok {
		return errors.New("choose the date layout")
	}
	if !in(m.Description) {
		return errors.New("choose the description column")
	}
	switch {
	case m.Amount != None:
		if !in(m.Amount) {
			return errors.New("choose the amount column")
		}
		if m.Debit != None || m.Credit != None {
			return errors.New("use one amount column, or separate debit and credit columns, not both")
		}
	default:
		if !in(m.Debit) || !in(m.Credit) || m.Debit == m.Credit {
			return errors.New("choose the amount column, or two different debit and credit columns")
		}
	}
	if !opt(m.Account) || !opt(m.Note) || !opt(m.Reference) {
		return errors.New("an optional column is out of range")
	}
	return nil
}

// Line is one incoming line under a mapping.
type Line struct {
	Row         int
	Raw         string
	Account     string
	PostedOn    time.Time // a date, at UTC midnight
	Amount      money.Cents
	Description string
	Note        string
	Reference   string
}

// Refusal is a data row that could not be read.
type Refusal struct {
	Row    int
	Reason string
}

// Result is a file read under a mapping.
type Result struct {
	// Incoming are the lines of money received, in file order.
	Incoming []Line
	// Outgoing counts lines of money paid out, or of zero; they are not kept
	// (spec section 4).
	Outgoing int
	// Refused are rows that could not be read, with why.
	Refused []Refusal
}

// Apply reads every record of f under m. A mapping that does not fit the
// file's header refuses every row rather than reading the wrong cells.
func Apply(f File, m Mapping) Result {
	var res Result
	if err := m.Validate(len(f.Header)); err != nil {
		for _, rec := range f.Records {
			res.Refused = append(res.Refused, Refusal{rec.Row, "the layout does not fit: " + err.Error()})
		}
		return res
	}
	for _, rec := range f.Records {
		if rec.Problem != "" {
			res.Refused = append(res.Refused, Refusal{rec.Row, rec.Problem})
			continue
		}
		if len(rec.Raw) > MaxRowBytes {
			res.Refused = append(res.Refused, Refusal{rec.Row, "is longer than 16 KB"})
			continue
		}
		line, incoming, err := readLine(rec, m)
		switch {
		case err != nil:
			res.Refused = append(res.Refused, Refusal{rec.Row, err.Error()})
		case incoming:
			res.Incoming = append(res.Incoming, line)
		default:
			res.Outgoing++
		}
	}
	return res
}

func readLine(rec Record, m Mapping) (Line, bool, error) {
	cell := func(c int) string {
		if c == None {
			return ""
		}
		return rec.Fields[c]
	}

	l := Line{
		Row:         rec.Row,
		Raw:         rec.Raw,
		Account:     strings.TrimSpace(cell(m.Account)),
		Description: strings.TrimSpace(cell(m.Description)),
		// The note is kept verbatim, line breaks included (spec section 2).
		Note:      cell(m.Note),
		Reference: strings.TrimSpace(cell(m.Reference)),
	}

	d, err := ParseDate(cell(m.Date), m.DateLayout)
	if err != nil {
		return Line{}, false, err
	}
	l.PostedOn = d

	var signed money.Cents
	if m.Amount != None {
		signed, err = ParseAmount(cell(m.Amount), m.DecimalComma)
		if err != nil {
			return Line{}, false, err
		}
		if m.IncomingNegative {
			signed = -signed
		}
	} else {
		signed, err = debitCredit(cell(m.Debit), cell(m.Credit), m.DecimalComma)
		if err != nil {
			return Line{}, false, err
		}
	}
	if signed <= 0 {
		return Line{}, false, nil
	}
	l.Amount = signed
	return l, true, nil
}

// debitCredit reads a two-column layout: exactly one of the two holds a value.
// Either may carry a sign or parentheses; the column, not the sign, says which
// way the money went.
func debitCredit(debit, credit string, decimalComma bool) (money.Cents, error) {
	debit, credit = strings.TrimSpace(debit), strings.TrimSpace(credit)
	switch {
	case debit != "" && credit != "":
		d, err1 := ParseAmount(debit, decimalComma)
		c, err2 := ParseAmount(credit, decimalComma)
		if err1 == nil && err2 == nil && d == 0 {
			return abs(c), nil
		}
		if err1 == nil && err2 == nil && c == 0 {
			return -abs(d), nil
		}
		return 0, errors.New("has both a debit and a credit")
	case credit != "":
		c, err := ParseAmount(credit, decimalComma)
		return abs(c), err
	case debit != "":
		d, err := ParseAmount(debit, decimalComma)
		return -abs(d), err
	}
	return 0, errors.New("has no amount")
}

func abs(c money.Cents) money.Cents {
	if c < 0 {
		return -c
	}
	return c
}

// ParseDate reads a date cell under a layout key. A time after the date
// ("9/1/2026 12:00:00 AM") is ignored. Years outside 1990..2100 are refused,
// which catches a D/M file read as M/D only sometimes, but catches a two-digit
// year read as four digits always.
func ParseDate(s, layoutKey string) (time.Time, error) {
	l, ok := dateLayout(layoutKey)
	if !ok {
		return time.Time{}, fmt.Errorf("unknown date layout %q", layoutKey)
	}
	field := strings.Fields(s)
	if len(field) == 0 {
		return time.Time{}, errors.New("has no date")
	}
	d, err := time.Parse(l.go1, field[0])
	if err != nil {
		return time.Time{}, fmt.Errorf("date %q is not %s", field[0], strings.Fields(l.Label)[0])
	}
	if d.Year() < 1990 || d.Year() > 2100 {
		return time.Time{}, fmt.Errorf("date %q is out of range", field[0])
	}
	return d, nil
}

// ParseAmount reads an amount cell to signed cents.
//
// Accepted: a leading "+" or "-", or parentheses for negative ("($336.41)");
// a currency symbol ($, €, £); thousands separators; at most two decimal
// places. With decimalComma the roles of "." and "," swap. Anything else --
// three decimals, two signs, letters -- is refused rather than guessed.
func ParseAmount(s string, decimalComma bool) (money.Cents, error) {
	orig := s
	s = strings.TrimSpace(strings.ReplaceAll(s, " ", " "))
	if s == "" {
		return 0, errors.New("has no amount")
	}
	neg := false
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		neg = true
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		if neg {
			return 0, fmt.Errorf("amount %q has two signs", orig)
		}
		neg = s[0] == '-'
		s = strings.TrimSpace(s[1:])
	}
	for _, sym := range []string{"$", "€", "£"} {
		s = strings.TrimPrefix(s, sym)
	}
	// "-$5" and "$-5" are both seen in the wild.
	if strings.HasPrefix(s, "-") && !neg {
		neg = true
		s = s[1:]
	}
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "")

	if decimalComma {
		s = strings.ReplaceAll(s, ".", "")
		s = strings.ReplaceAll(s, ",", ".")
	} else {
		s = strings.ReplaceAll(s, ",", "")
	}
	if s == "" || strings.ContainsAny(s, "+-$€£()") {
		return 0, fmt.Errorf("amount %q is not a number", orig)
	}
	c, err := money.Parse(s)
	if err != nil {
		if errors.Is(err, money.ErrTooManyDecimals) {
			return 0, fmt.Errorf("amount %q has more than two decimal places", orig)
		}
		return 0, fmt.Errorf("amount %q is not a number", orig)
	}
	if neg {
		c = -c
	}
	return c, nil
}

// NormaliseDescription is the description as the fingerprint sees it:
// lower-cased, with runs of whitespace (line breaks included) as one space.
func NormaliseDescription(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// Fingerprints computes each line's duplicate key (spec section 5): layout,
// account, date, amount, normalised description, reference, and the line's
// occurrence number among identical lines in this file -- so two genuine $50
// transfers on one day stay two lines, while the same two lines in an
// overlapping file are recognised. The account is included because one export
// can cover several accounts.
func Fingerprints(formatID int64, lines []Line) []string {
	seen := make(map[string]int, len(lines))
	out := make([]string, len(lines))
	for i, l := range lines {
		key := strings.Join([]string{
			strconv.FormatInt(formatID, 10),
			strings.ToLower(l.Account),
			l.PostedOn.Format("2006-01-02"),
			strconv.FormatInt(int64(l.Amount), 10),
			NormaliseDescription(l.Description),
			strings.ToLower(l.Reference),
		}, "\x1f")
		seen[key]++
		sum := sha256.Sum256([]byte(key + "\x1f" + strconv.Itoa(seen[key])))
		out[i] = hex.EncodeToString(sum[:])
	}
	return out
}

// Guess proposes a mapping from the header names and the first rows, for the
// mapping screen to start from. A person confirms it; nothing is saved on a
// guess.
func Guess(f File) Mapping {
	m := Mapping{
		Date: None, Amount: None, Debit: None, Credit: None,
		Description: None, Account: None, Note: None, Reference: None,
		DecimalComma: f.Separator == ';',
	}
	names := map[*int][]string{
		&m.Date:        {"date", "posted date", "posting date", "transaction date", "booking date", "post date"},
		&m.Amount:      {"amount", "transaction amount", "amount ($)"},
		&m.Debit:       {"debit", "debits", "withdrawal", "withdrawals", "money out", "paid out"},
		&m.Credit:      {"credit", "credits", "deposit", "deposits", "money in", "paid in"},
		&m.Description: {"description", "details", "narrative", "payee", "transaction description", "memo", "name"},
		&m.Account:     {"account", "account name", "account number"},
		&m.Note:        {"note", "notes"},
		&m.Reference:   {"check #", "check number", "check", "cheque number", "reference", "ref", "reference number"},
	}
	// Earlier names in each list win, so "description" beats "memo" when a
	// file has both.
	for ptr, want := range names {
		best := len(want)
		for i, h := range f.Header {
			h = strings.ToLower(strings.TrimSpace(h))
			for rank, w := range want {
				if h == w && rank < best {
					best, *ptr = rank, i
				}
			}
		}
	}
	if m.Amount != None && m.Debit != None && m.Credit != None {
		m.Debit, m.Credit = None, None
	}
	if m.Amount == None && (m.Debit == None || m.Credit == None) {
		m.Debit, m.Credit = None, None
	}

	m.DateLayout = DateLayouts[0].Key
	if m.Date != None {
		for _, l := range DateLayouts {
			if datesFit(f, m.Date, l.Key) {
				m.DateLayout = l.Key
				break
			}
		}
	}
	return m
}

// datesFit reports whether the first rows' dates all read under a layout.
func datesFit(f File, col int, key string) bool {
	n := 0
	for _, r := range f.Records {
		if r.Fields == nil {
			continue
		}
		if _, err := ParseDate(r.Fields[col], key); err != nil {
			return false
		}
		if n++; n == 20 {
			break
		}
	}
	return n > 0
}

// PreviewRow is one row as a mapping reads it, for the mapping screen.
type PreviewRow struct {
	Row       int
	Line      Line   // set unless Outcome is "refused"
	Outcome   string // "in", "out" or "refused"
	Reason    string // why, when refused
	RawFields []string
}

// Preview reads the first n data rows of f under m, saying what happens to
// each: kept as incoming, dropped as outgoing, or refused and why.
func Preview(f File, m Mapping, n int) []PreviewRow {
	var out []PreviewRow
	verr := m.Validate(len(f.Header))
	for _, rec := range f.Records {
		if len(out) == n {
			break
		}
		p := PreviewRow{Row: rec.Row, RawFields: rec.Fields}
		switch {
		case verr != nil:
			p.Outcome, p.Reason = "refused", "the layout does not fit: "+verr.Error()
		case rec.Problem != "":
			p.Outcome, p.Reason = "refused", rec.Problem
		case len(rec.Raw) > MaxRowBytes:
			p.Outcome, p.Reason = "refused", "is longer than 16 KB"
		default:
			line, incoming, err := readLine(rec, m)
			switch {
			case err != nil:
				p.Outcome, p.Reason = "refused", err.Error()
			case incoming:
				p.Outcome, p.Line = "in", line
			default:
				p.Outcome = "out"
			}
		}
		out = append(out, p)
	}
	return out
}
