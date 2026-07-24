package main

import "testing"

// A stray argument must never fall through to starting a server. It did once,
// and the server it started applied a forward-only migration to a database that
// was only meant to be inspected.
func TestRunArgsRefusesUnrecognizedArguments(t *testing.T) {
	for _, arg := range []string{"-version", "--migrate", "serve", "-x", "--db=other.db"} {
		handled, err := runArgs([]string{arg})
		if !handled {
			t.Errorf("runArgs(%q) fell through to starting the server", arg)
		}
		if err == nil {
			t.Errorf("runArgs(%q) reported no error", arg)
		}
	}
}

func TestRunArgsAnswersVersionAndHelp(t *testing.T) {
	for _, arg := range []string{"-v", "--version", "version", "-h", "--help", "help"} {
		handled, err := runArgs([]string{arg})
		if !handled {
			t.Errorf("runArgs(%q) did not handle the argument", arg)
		}
		if err != nil {
			t.Errorf("runArgs(%q) errored: %v", arg, err)
		}
	}
}

// No arguments is the ordinary case and must start the server.
func TestRunArgsStartsWithNoArguments(t *testing.T) {
	handled, err := runArgs(nil)
	if handled || err != nil {
		t.Errorf("runArgs(nil) = (%v, %v), want (false, nil)", handled, err)
	}
}
