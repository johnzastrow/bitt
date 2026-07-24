package views

import "testing"

func TestInitials(t *testing.T) {
	cases := map[string]string{
		"Jane Provider": "JP",
		"jane":          "J",
		"Alex Q Public": "AQ",
		"(removed)":     "R",
		"--":            "?",
		"":              "?",
		"  spaced  out": "SO",
		"李 Wang":        "李W",
	}
	for in, want := range cases {
		if got := initials(in); got != want {
			t.Errorf("initials(%q) = %q, want %q", in, got, want)
		}
	}
}
