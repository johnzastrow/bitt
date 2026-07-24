package web

import (
	"context"

	"github.com/johnzastrow/bitt/internal/config"
	"github.com/johnzastrow/bitt/internal/notify"
	"github.com/johnzastrow/bitt/internal/store"
)

// Notification configuration resolves at three levels, and the rule is the same
// at each: the more specific setting wins, and the environment beats the
// database.
//
//	reminders:  a tab's own -> the instance's stored defaults -> the environment
//	            (or the built-in 14/7/1 when the environment is silent)
//	delivery:   the environment -> the instance's stored settings
//
// Delivery runs the other way round from reminders on purpose. A reminder
// message is content, and the most specific author should win. A delivery
// setting is deployment, and the deployment -- the compose file, the unit file,
// the thing under version control -- should win over a value someone typed into
// a form months ago. It also keeps a container reproducible: bring it up with
// the same environment and it behaves the same way, whatever is in the volume.
//
// Secrets never come from the database at all: the SMTP password, the ntfy
// token, and the tick secret are environment-or-file only, and there are no
// columns for them (migration 0010).

// notifyConfig resolves the effective delivery configuration.
//
// A read failure falls back to the environment alone rather than failing the
// request. The caller is either rendering a page or running the tick, and the
// honest degradation for both is "deliver with what the deployment configured",
// never "deliver with a half-populated config".
func (s *Server) notifyConfig(ctx context.Context) config.NotifyConfig {
	cfg := s.cfg.Notify

	inst, err := s.store.GetInstance(ctx)
	if err != nil {
		s.log.Error("resolve delivery settings", "error", err)
		return cfg
	}
	d := inst.Delivery

	// Each field independently: an operator who sets only BITT_SMTP_HOST in the
	// environment should not have the stored From address ignored as well.
	if cfg.SMTPHost == "" {
		cfg.SMTPHost = d.SMTPHost
		// The port belongs to the host that was chosen. Taking a stored port
		// beside an environment host would aim the deployment's mail at a port
		// nobody deploying it can see.
		if d.SMTPPort > 0 {
			cfg.SMTPPort = d.SMTPPort
		}
	}
	if cfg.SMTPUsername == "" {
		cfg.SMTPUsername = d.SMTPUsername
	}
	if cfg.EmailFrom == "" {
		cfg.EmailFrom = d.EmailFrom
	}
	if cfg.NtfyBaseURL == "" {
		cfg.NtfyBaseURL = d.NtfyBaseURL
	}
	return cfg
}

// notifierFor returns a Notifier delivering under the effective configuration.
//
// The Notifier is rebuilt per use rather than held from startup, because a
// setting changed through the interface has to take effect without a restart.
// It shares the original's HTTP client, so the connection pool and its
// SSRF-checking dialer survive.
func (s *Server) notifierFor(ctx context.Context) *notify.Notifier {
	if s.notifier == nil {
		return nil
	}
	return s.notifier.With(s.notifyConfig(ctx))
}

// instanceReminders resolves the instance-wide default reminder set: the
// environment when it specified one, the stored defaults otherwise, and the
// built-in 14/7/1 when neither did.
func (s *Server) instanceReminders(ctx context.Context) []config.Reminder {
	if s.cfg.RemindersFromEnv {
		return s.cfg.Reminders
	}
	stored, err := s.store.ListInstanceReminders(ctx)
	if err != nil {
		s.log.Error("list instance reminders", "error", err)
		return s.cfg.Reminders
	}
	if len(stored) == 0 {
		return s.cfg.Reminders
	}
	out := make([]config.Reminder, 0, len(stored))
	for _, r := range stored {
		out = append(out, config.Reminder{Days: r.Days, Title: r.Title, Body: r.Body})
	}
	return out
}

// defaultReminders is instanceReminders in the shape the tab page displays.
// Converting here keeps the view layer clear of the config package.
func (s *Server) defaultReminders(ctx context.Context) []store.TabReminder {
	rs := s.instanceReminders(ctx)
	out := make([]store.TabReminder, 0, len(rs))
	for _, r := range rs {
		out = append(out, store.TabReminder{Days: r.Days, Title: r.Title, Body: r.Body})
	}
	return out
}

// notifyReady reports whether this instance can deliver anything at all, under
// the effective configuration rather than whatever was set at startup.
func (s *Server) notifyReady(ctx context.Context) bool {
	n := s.notifierFor(ctx)
	return n != nil && n.Enabled()
}
