// Package bankcsv reads a bank's CSV export for reconciliation
// (SPEC-BANK-RECONCILE, RECON-02).
//
// It is pure: bytes in, typed lines out, no database and no clock. Reading is
// split in two so a layout the instance has never seen can be shown to a
// person before anything is kept:
//
//   - Read decodes and splits the file: encoding, separator, header, records,
//     and the original text of every record.
//   - Apply turns records into typed lines under a Mapping (which column is
//     the date, the amount, and so on), keeping only incoming money.
//
// Amounts go straight from text to integer cents through internal/money; no
// floating point is involved anywhere (LEDGER-04). No cell is ever evaluated.
package bankcsv

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Limits on an upload (spec section 4).
const (
	MaxBytes = 5 << 20
	MaxRows  = 10_000
)

// ErrRefused wraps every reason a whole file is refused, so callers can show
// the message and know it is the uploader's to fix.
var ErrRefused = errors.New("bankcsv: file refused")

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrRefused}, args...)...)
}

// File is a decoded, split export.
type File struct {
	// Header is the first record, as written.
	Header []string
	// Signature identifies the layout: the header normalised (trimmed,
	// lower-cased) and hashed. A saved mapping is found by it.
	Signature string
	// Separator is ',' or ';'.
	Separator rune
	// Encoding is "utf-8" or "windows-1252".
	Encoding string
	// Records are the data records, header excluded, in file order.
	Records []Record
}

// Record is one data record of the file.
type Record struct {
	// Row is the record's position with the header as row 1, which is the row
	// a spreadsheet shows. A quoted field spanning two lines is still one row.
	Row int
	// Raw is the record's original text, exactly as exported (decoded to
	// UTF-8), without its line ending.
	Raw string
	// Fields are the parsed cells. Nil when the record was malformed.
	Fields []string
	// Problem says why the record cannot be used, when it cannot.
	Problem string
}

// Read decodes and splits an export.
//
// Accepted: UTF-8 (with or without a byte-order mark) or Windows-1252; comma or
// semicolon separated; RFC 4180 quoting including line breaks inside a quoted
// field. LazyQuotes stays off, so a stray quote refuses the file rather than
// being guessed at. A record with the wrong number of fields is kept as a
// Record with a Problem, so one bad row does not refuse a whole statement.
func Read(data []byte) (File, error) {
	if len(data) > MaxBytes {
		return File{}, refuse("the file is larger than 5 MB")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return File{}, refuse("the file is empty")
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return File{}, refuse("the file contains binary data; export it as CSV")
	}

	var f File
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	text := string(data)
	f.Encoding = "utf-8"
	if !utf8.ValidString(text) {
		decoded, err := decodeWindows1252(data)
		if err != nil {
			return File{}, err
		}
		text, f.Encoding = decoded, "windows-1252"
	}

	f.Separator = detectSeparator(text)

	r := csv.NewReader(strings.NewReader(text))
	r.Comma = f.Separator
	r.LazyQuotes = false
	r.FieldsPerRecord = 0 // the header sets the count; others must match

	var prev int64
	row := 0
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		off := r.InputOffset()
		raw := strings.TrimRight(text[prev:off], "\r\n")
		// encoding/csv skips blank lines; the text between records then
		// starts with them, which is not part of this record.
		raw = strings.TrimLeft(raw, "\r\n")
		prev = off

		var pe *csv.ParseError
		if err != nil && !(errors.As(err, &pe) && errors.Is(pe.Err, csv.ErrFieldCount)) {
			if errors.As(err, &pe) {
				return File{}, refuse("the file is not valid CSV near line %d: %v", pe.StartLine, pe.Err)
			}
			return File{}, refuse("the file is not valid CSV: %v", err)
		}

		row++
		if row == 1 {
			// The header is stored with a saved layout; bound it like a row.
			if len(raw) > 16<<10 {
				return File{}, refuse("the header row is longer than 16 KB")
			}
			f.Header = rec
			continue
		}
		if len(f.Records) >= MaxRows {
			return File{}, refuse("the file has more than %d rows", MaxRows)
		}
		r := Record{Row: row, Raw: raw}
		if err != nil {
			r.Problem = fmt.Sprintf("has %d fields, the header has %d", len(rec), len(f.Header))
		} else {
			r.Fields = rec
		}
		f.Records = append(f.Records, r)
	}

	if len(f.Header) < 2 {
		return File{}, refuse("the first row is not a header with at least two columns")
	}
	f.Signature = Signature(f.Header)
	return f, nil
}

// Signature hashes a header row, normalised, into a layout identifier.
func Signature(header []string) string {
	norm := make([]string, len(header))
	for i, h := range header {
		norm[i] = strings.ToLower(strings.TrimSpace(h))
	}
	sum := sha256.Sum256([]byte(strings.Join(norm, "\x1f")))
	return hex.EncodeToString(sum[:])
}

// detectSeparator picks ';' only when the first line has more semicolons than
// commas outside quotes; comma is the default.
func detectSeparator(text string) rune {
	commas, semis := 0, 0
	inQuote := false
	for _, c := range text {
		switch {
		case c == '"':
			inQuote = !inQuote
		case inQuote:
		case c == '\n':
			if semis > commas {
				return ';'
			}
			return ','
		case c == ',':
			commas++
		case c == ';':
			semis++
		}
	}
	if semis > commas {
		return ';'
	}
	return ','
}

// windows1252High maps 0x80..0x9F. Zero marks the five bytes the code page
// leaves undefined.
var windows1252High = [32]rune{
	0x20AC, 0, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0, 0x017D, 0,
	0, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0, 0x017E, 0x0178,
}

// decodeWindows1252 decodes a file that is not valid UTF-8. Every byte outside
// 0x80..0x9F is the same code point (Latin-1); the five undefined bytes mean
// the file is in some other encoding, and it is refused rather than guessed.
func decodeWindows1252(data []byte) (string, error) {
	var b strings.Builder
	b.Grow(len(data) + len(data)/8)
	for _, c := range data {
		switch {
		case c < 0x80 || c >= 0xA0:
			b.WriteRune(rune(c))
		default:
			r := windows1252High[c-0x80]
			if r == 0 {
				return "", refuse("the file is neither UTF-8 nor Windows-1252")
			}
			b.WriteRune(r)
		}
	}
	return b.String(), nil
}
