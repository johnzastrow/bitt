package web

import (
	"net/smtp"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/johnzastrow/bitt/internal/config"
	"github.com/johnzastrow/bitt/internal/notify"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// TestTickScanDeliversOnceThenIsIdempotent exercises the whole reminder scan the
// way the cron drives it: a tab that owes money, whose next due date sits on a
// reminder lead time, with a payee who wants email. It uses an injected mailer
// so no server is contacted, and asserts the send-then-claim guarantee -- one
// delivery, and a second tick that sends nothing because the claim was recorded.
func TestTickScanDeliversOnceThenIsIdempotent(t *testing.T) {
	h := newHarnessWithTick(t, "the-cron-secret")
	h.completeSetup() // user 1 is the provider
	ctx := t.Context()

	payee := h.addUser("payee@example.com", "Payee Person", false)
	if err := h.db.SetNotifyPrefs(ctx, payee.ID, "", true, false); err != nil {
		t.Fatalf("set notify prefs: %v", err)
	}
	// Instance email delivery, so the email channel is available.
	if err := h.db.SetDelivery(ctx, store.Delivery{
		SMTPHost: "mail.example", SMTPPort: 587, EmailFrom: "BitTabby <bitt@example.com>",
	}); err != nil {
		t.Fatalf("set delivery: %v", err)
	}

	// Capture what the scan sends instead of contacting a server.
	var mu sync.Mutex
	var sent [][]byte
	h.srv().notifier = h.srv().notifier.WithMailer(func(_ string, _ smtp.Auth, _ string, to []string, msg []byte) error {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, msg)
		return nil
	})

	// A weekly tab anchored one week out: its next unpaid due is 7 days away, so
	// the built-in 7-day reminder fires.
	today := instanceToday(t)
	tabID, _ := h.createScheduledTab(today.AddDays(7), schedule.Weekly, schedule.InAdvance)

	if err := h.db.AddParticipant(ctx, store.Participant{
		TabID: tabID, UserID: payee.ID, Role: store.RolePayee,
	}); err != nil {
		t.Fatalf("add payee: %v", err)
	}

	// Something must be owed, or a reminder is pointless and the scan skips it.
	if resp, b := h.post(tabPath(tabID)+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tabPath(tabID))},
		"amount":     {"100.00"},
		"memo":       {"Service"},
	}); resp.StatusCode != 200 {
		t.Fatalf("charge returned %d: %s", resp.StatusCode, truncate(b))
	}

	// First tick: exactly one email, to the payee, naming the tab.
	if resp := h.postRaw(t, "/internal/tick", "Bearer the-cron-secret"); resp.StatusCode != 200 {
		t.Fatalf("tick returned %d", resp.StatusCode)
	}
	mu.Lock()
	n := len(sent)
	var first string
	if n > 0 {
		first = string(sent[0])
	}
	mu.Unlock()
	if n != 1 {
		t.Fatalf("first tick sent %d emails, want exactly 1", n)
	}
	if !strings.Contains(first, "To: payee@example.com") {
		t.Errorf("email not addressed to the payee:\n%s", first)
	}
	if !strings.Contains(first, "Lawn service") {
		t.Errorf("email does not name the tab:\n%s", first)
	}

	// The claim was recorded, so a second tick in the same window sends nothing.
	if resp := h.postRaw(t, "/internal/tick", "Bearer the-cron-secret"); resp.StatusCode != 200 {
		t.Fatalf("second tick returned %d", resp.StatusCode)
	}
	mu.Lock()
	after := len(sent)
	mu.Unlock()
	if after != 1 {
		t.Errorf("second tick sent again: total %d, want 1 (send-then-claim must be idempotent)", after)
	}
}

// TestTickScanSkipsAPaidTab is the defect the security review named: a tab whose
// balance is settled must produce no dunning notice even if its due date lands
// on a reminder lead, because the scan re-derives live state before sending.
func TestTickScanSkipsAPaidTab(t *testing.T) {
	h := newHarnessWithTick(t, "sec")
	h.completeSetup()
	ctx := t.Context()

	payee := h.addUser("payee@example.com", "Payee", false)
	_ = h.db.SetNotifyPrefs(ctx, payee.ID, "", true, false)
	_ = h.db.SetDelivery(ctx, store.Delivery{SMTPHost: "mail.example", SMTPPort: 587, EmailFrom: "bitt@example.com"})

	var sent int
	var mu sync.Mutex
	h.srv().notifier = h.srv().notifier.WithMailer(func(string, smtp.Auth, string, []string, []byte) error {
		mu.Lock()
		sent++
		mu.Unlock()
		return nil
	})

	today := instanceToday(t)
	tabID, _ := h.createScheduledTab(today.AddDays(7), schedule.Weekly, schedule.InAdvance)
	_ = h.db.AddParticipant(ctx, store.Participant{TabID: tabID, UserID: payee.ID, Role: store.RolePayee})

	// Nothing charged: the balance is zero (nothing owed), so no reminder is due.
	if resp := h.postRaw(t, "/internal/tick", "Bearer sec"); resp.StatusCode != 200 {
		t.Fatalf("tick returned %d", resp.StatusCode)
	}
	mu.Lock()
	got := sent
	mu.Unlock()
	if got != 0 {
		t.Errorf("a tab with nothing owed produced %d reminders, want 0", got)
	}
}

// channelsFor returns exactly the channels a user has turned on AND that can
// currently deliver to them. The matrix matters: a preference for a channel that
// is not configured, or configured but missing the user's coordinate, yields
// nothing on that channel rather than a failed send later.
func TestChannelsFor(t *testing.T) {
	email := notify.New(config.NotifyConfig{SMTPHost: "mail.example", SMTPPort: 587, EmailFrom: "a@b.com"})
	both := notify.New(config.NotifyConfig{SMTPHost: "mail.example", SMTPPort: 587, EmailFrom: "a@b.com", NtfyBaseURL: "https://ntfy.example"})

	cases := []struct {
		name string
		u    store.User
		n    *notify.Notifier
		r    notify.Recipient
		want []notify.Channel
	}{
		{
			name: "wants email, email configured, has address",
			u:    store.User{NotifyEmail: true},
			n:    email,
			r:    notify.Recipient{Email: "p@example.com"},
			want: []notify.Channel{notify.ChannelEmail},
		},
		{
			name: "wants email but no address -> nothing",
			u:    store.User{NotifyEmail: true},
			n:    email,
			r:    notify.Recipient{},
			want: nil,
		},
		{
			name: "wants ntfy but only email is configured -> nothing",
			u:    store.User{NotifyNtfy: true},
			n:    email,
			r:    notify.Recipient{Topic: "bitt-x"},
			want: nil,
		},
		{
			name: "wants both, both configured, both coordinates present",
			u:    store.User{NotifyEmail: true, NotifyNtfy: true},
			n:    both,
			r:    notify.Recipient{Email: "p@example.com", Topic: "bitt-x"},
			want: []notify.Channel{notify.ChannelEmail, notify.ChannelNtfy},
		},
		{
			name: "wants nothing -> nothing",
			u:    store.User{},
			n:    both,
			r:    notify.Recipient{Email: "p@example.com", Topic: "bitt-x"},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := channelsFor(tc.u, tc.n, tc.r)
			if len(got) != len(tc.want) {
				t.Fatalf("channelsFor = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("channel[%d] = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
