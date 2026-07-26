package sqldb

import (
	"context"
	"testing"

	"github.com/johnzastrow/bitt/internal/store"
)

// A participant may be attached with the admin role, and it round-trips. This
// also proves migration 0011 widened the role CHECK on whichever backend is
// under test -- before it, the insert would be rejected by the constraint.
func TestParticipantAdminRoleRoundTrips(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	provider := mustUser(t, db, "p@example.com")
	admin := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, provider.ID)

	if err := db.AddParticipant(ctx, store.Participant{
		TabID: tab.ID, UserID: admin.ID, Role: store.RoleAdmin,
	}); err != nil {
		t.Fatalf("add admin participant: %v", err)
	}

	role, err := db.ParticipantRole(ctx, tab.ID, admin.ID)
	if err != nil {
		t.Fatalf("participant role: %v", err)
	}
	if role != store.RoleAdmin {
		t.Errorf("role = %q, want %q", role, store.RoleAdmin)
	}

	// The tab now lists both people, and both roles are represented.
	ps, err := db.ListParticipants(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list participants: %v", err)
	}
	roles := map[store.Role]bool{}
	for _, p := range ps {
		roles[p.Role] = true
	}
	if !roles[store.RoleProvider] || !roles[store.RoleAdmin] {
		t.Errorf("roles present = %v, want provider and admin", roles)
	}
}
