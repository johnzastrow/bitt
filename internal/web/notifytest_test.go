package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/johnzastrow/bitt/internal/store"
)

// The admin "send a test notification" action. A real end-to-end send cannot be
// exercised here: the notifier dials through an SSRF-guarded client that refuses
// loopback, so a test ntfy server on 127.0.0.1 is (correctly) unreachable. That
// guard and real delivery are covered in the notify package. These tests pin the
// handler's decisions -- fail closed when nothing is configured, gate the button
// on a configured channel, and stay admin-only.

func TestNotifyTestReportsNoChannel(t *testing.T) {
	h := newHarness(t) // no delivery configured
	h.completeSetup()

	_, body := h.post("/admin/notifications/test", url.Values{
		"csrf_token": {h.csrfToken("/admin/notifications")},
	})
	if !strings.Contains(body, "No delivery channel is configured") {
		t.Errorf("test with nothing configured did not report it: %s", truncate(body))
	}
}

func TestNotifyTestButtonGatedOnConfig(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	const action = `action="/admin/notifications/test"`

	_, body := h.get("/admin/notifications")
	if strings.Contains(body, action) {
		t.Error("the test button shows before any channel is configured")
	}

	// A stored ntfy URL enables a channel without a restart.
	if err := h.db.SetDelivery(t.Context(), store.Delivery{NtfyBaseURL: "https://ntfy.example"}); err != nil {
		t.Fatalf("set delivery: %v", err)
	}
	_, body = h.get("/admin/notifications")
	if !strings.Contains(body, action) {
		t.Error("the test button is missing after a channel was configured")
	}
}

func TestNotifyTestRequiresAdmin(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	// Configure a channel so the only thing standing between a non-admin and a
	// send is the authorization check.
	if err := h.db.SetDelivery(t.Context(), store.Delivery{NtfyBaseURL: "https://ntfy.example"}); err != nil {
		t.Fatalf("set delivery: %v", err)
	}
	h.addUser("plain@example.com", "Plain", false)
	h.loginAs("plain@example.com", "a-long-enough-password")

	resp, body := h.post("/admin/notifications/test", url.Values{
		"csrf_token": {h.csrfToken("/")},
	})
	if resp.StatusCode == 200 && strings.Contains(body, "Test sent") {
		t.Errorf("a non-admin triggered a test send: %s", truncate(body))
	}
}
