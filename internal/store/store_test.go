package store

import "testing"

// The enum helpers gate what the app will accept and how it labels things, so a
// silent change to one is a behaviour change. These pin the recognized values
// and their display strings.

func TestEntryKindValid(t *testing.T) {
	for _, k := range []EntryKind{KindCharge, KindPayment, KindAdjustment, KindFee, KindReversal} {
		if !k.Valid() {
			t.Errorf("EntryKind %q should be valid", k)
		}
	}
	for _, k := range []EntryKind{"", "bogus", "Charge", "refund"} {
		if k.Valid() {
			t.Errorf("EntryKind %q should be invalid", k)
		}
	}
}

func TestTabKind(t *testing.T) {
	if !TabServices.Valid() || !TabPayoff.Valid() {
		t.Error("the two real tab kinds must be valid")
	}
	for _, k := range []TabKind{"", "loan", "Services"} {
		if k.Valid() {
			t.Errorf("TabKind %q should be invalid", k)
		}
	}
	if TabServices.Label() != "Services" || TabPayoff.Label() != "Payoff" {
		t.Errorf("labels wrong: %q / %q", TabServices.Label(), TabPayoff.Label())
	}
	if TabKind("bogus").Label() != "" || TabKind("bogus").Describe() != "" {
		t.Error("an unknown kind should have empty label and description")
	}
	if TabServices.Describe() == "" || TabPayoff.Describe() == "" {
		t.Error("the real kinds must describe themselves")
	}
	kinds := TabKinds()
	if len(kinds) != 2 || kinds[0] != TabServices || kinds[1] != TabPayoff {
		t.Errorf("TabKinds() = %v, want [services payoff] in that order", kinds)
	}
}

func TestRoleValid(t *testing.T) {
	for _, r := range []Role{RoleProvider, RolePayee, RoleAdmin} {
		if !r.Valid() {
			t.Errorf("Role %q should be valid", r)
		}
	}
	for _, r := range []Role{"", "owner", "Provider", "superuser"} {
		if r.Valid() {
			t.Errorf("Role %q should be invalid", r)
		}
	}
}
