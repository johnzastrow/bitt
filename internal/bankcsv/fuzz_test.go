package bankcsv

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// FuzzRead: any input is either refused with ErrRefused or read, never a
// panic, and every record's raw text comes from the file itself.
func FuzzRead(f *testing.F) {
	if data, err := os.ReadFile("testdata/reference.csv"); err == nil {
		f.Add(data)
	}
	f.Add([]byte("Date;Description;Amount\n01.09.2026;\"a;b\";1.234,56\n"))
	f.Add([]byte("Date,Payee,Debit,Credit\n2026-09-01,A,,50.00\n"))
	f.Add([]byte("\xEF\xBB\xBFa,b\n\"x\ny\",($1.00)\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		file, err := Read(data)
		if err != nil {
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("error not wrapped in ErrRefused: %v", err)
			}
			return
		}
		text := string(data)
		if file.Encoding == "utf-8" {
			for _, r := range file.Records {
				if !strings.Contains(text, r.Raw) {
					t.Fatalf("raw %q not in the input", r.Raw)
				}
			}
		}
		m := Guess(file)
		if m.Validate(len(file.Header)) == nil {
			res := Apply(file, m)
			for _, l := range res.Incoming {
				if l.Amount <= 0 {
					t.Fatalf("incoming line with amount %d", l.Amount)
				}
			}
			_ = Fingerprints(1, res.Incoming)
		}
	})
}
