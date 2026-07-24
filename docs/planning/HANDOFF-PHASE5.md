# Handoff — after Phase 5 (notifications), 0.7.1

Written 2026-07-24, to be read cold. Covers what notifications now do, the
security design they were built to, and the one clearly-scoped next task.

The main [HANDOFF.md](HANDOFF.md) still holds the project's shape, the ledger
invariants, and the 0.5/0.6 traps. This file is the Phase 5 delta.

---

## DONE in 0.7.3: per-tab, provider-set reminders

This section described the next task; it was built in 0.7.3 substantially as
written. What shipped, against the plan below: migration `0009` with the
suggested `(tab_id, days, title, body)` table; `ListTabReminders` /
`SetTabReminders` on the store; a Reminders card in the tab page's Setup group;
and `Server.reminderForTab` at the `reminderFor(lead)` call site.

Three things the plan did not anticipate, all of which matter:

1. **A customised tab does not merge with the instance defaults, it replaces
   them.** Otherwise removing a lead time from a tab is impossible -- the
   instance default keeps sending it.
2. **The templates became USER text.** A Provider is an ordinary user, and the
   title reaches a mail Subject, so `notify.ValidTitleTemplate` /
   `ValidBodyTemplate` validate at input as well as at send.
3. **A `<textarea>` submits CRLF.** Refusing control characters without
   normalising that first makes every multi-line message unsaveable in a
   browser. Only a real browser catches this; the Go tests post LF.

The original plan follows, for the reasoning behind the shape.

### The plan as written

The user's stated model: **the Provider configures notifications per tab** — the
reminder lead times and the message text — because the Provider owns the tab's
billing cadence. What shipped in 0.7.0–0.7.1 is **instance-wide defaults**
(`BITT_REMINDER_DAYS`, `BITT_REMINDER_TITLE/BODY[_<d>]`). That is the fallback
layer, not the end state.

**Build per-tab as an override of the instance defaults**, the same shape as fee
and interest settings already use:

1. **Schema** (migration 0009): a `tab_reminders` table, or reminder columns on
   `tabs`. A tab with no per-tab config falls back to `cfg.Reminders`. Suggest a
   small table `(tab_id, days, title, body)` so a tab can have several, mirroring
   the instance list.
2. **Store**: `SetTabReminders` / `ListTabReminders`, and have the scan prefer a
   tab's own reminders over `s.cfg.Reminders`.
3. **Handler + UI**: a "Reminders" card in the tab page's **Setup** group,
   Provider-only (`CanManage`), like the schedule / late-fee / interest cards.
   Reuse the `{tab} {amount} {due} {days} {when} {url}` variable set — the render
   function `Server.reminderMessage` already takes a `config.Reminder`, so point
   it at the per-tab spec instead.
4. **Where the scan reads them**: `runNotifications` in
   `internal/web/handlers_tick.go`, at the `s.reminderFor(lead)` call — swap the
   instance lookup for "this tab's reminders, or the instance default".

The variable rendering, the send-then-claim path, the header safety, and the
event-key/claim scheme all stay as-is; only the *source* of the days and
templates changes from instance config to per-tab rows.

**Do not** drop the instance-wide config — it stays the default for any tab the
Provider has not customised.

---

## What Phase 5 delivered (0.7.0), and where it lives

Reminders reach payees who do not have the app open, by email and ntfy, driven
by an external cron. Four slices, each its own commit:

| Area | File(s) | Note |
|---|---|---|
| Config | `internal/config` | `NotifyConfig`, `Reminder`, all via fail-closed `loader.str` |
| Delivery (the security surface) | `internal/notify` | header safety + SSRF policy, tested in isolation |
| Schema + store | migration `0008`, `store` + `sqlite` | `sent_notifications` claim table, prefs on `users` |
| Tick endpoint + scan | `internal/web/handlers_tick.go` | auth + read-only scan |
| Prefs UI | `internal/web/views/profile.templ` | topic + channel toggles |

**Run it:** set `BITT_TICK_SECRET` (and SMTP and/or `BITT_NTFY_URL`), then
`curl -H "Authorization: Bearer $SECRET" -X POST localhost:8080/internal/tick`.
With no secret set the endpoint 404s — it fails closed and cannot be driven.

---

## The security rules Phase 5 lives under — do not regress these

Full design in [../SECURITY-PHASE5.md](../SECURITY-PHASE5.md). The load-bearing
ones:

- **Off the balance path.** The tick scan is READ-ONLY with respect to the
  ledger — it reads balances and schedules and never posts an entry. If a future
  change makes the scan accrue, an outbound-triggered, cron-reachable endpoint
  becomes a ledger write. Do not.
- **`/internal/tick` fails closed.** No `BITT_TICK_SECRET` → every request
  refused. Secret is compared with `subtle.ConstantTimeCompare`, in an
  `Authorization: Bearer` header, checked before any work, rate-limited.
- **Send-then-claim (at-least-once).** Deliver first; write the `sent_notifications`
  claim only on confirmed success, in its own transaction, NEVER inside a ledger
  transaction. A transient failure re-sends next tick rather than dropping the
  notice; a crash re-sends one harmless duplicate.
- **Re-derive live state before sending.** Nothing owed → no notice, so a
  paid-early or settled tab is never dunned. Keep this in `runNotifications`.
- **Header safety.** `internal/notify` REJECTS (never strips) control characters
  in every header value; all user text (tab names) goes in the body. A hostile
  `{tab}` in a title fails the send closed. Keep user text out of headers.
- **SSRF policy is tuned for a container.** The ntfy host is admin config, so
  LAN/private addresses are ALLOWED (a sidecar or LAN ntfy box is legitimate),
  while loopback and link-local (cloud metadata `169.254.169.254`) are refused,
  checked at dial time (`isAllowedDestination` in `internal/notify/safe.go`).
  This deliberately refines the review's blanket "reject private" — see the
  comment there.

---

## Where notification config lives, and why (0.7.4)

Three layers, and two different precedence orders. Get this wrong and the
symptom is subtle -- a setting that appears to save and then does nothing.

| What | Order |
|---|---|
| Reminders (days + message) | a tab's own -> instance stored -> environment -> built-in 14/7/1 |
| Delivery (SMTP, ntfy URL) | environment -> instance stored |

They run opposite ways deliberately. A reminder message is CONTENT, so the most
specific author wins. A delivery setting is DEPLOYMENT, so the thing under
version control -- the compose file -- wins over a value typed into a form
months ago, and a container brought up with the same environment behaves the
same way whatever is in its volume.

`internal/web/notifyconfig.go` is where all of it resolves. Two traps it already
handles, both of which cost a test to find:

- **A read-only input still posts its value.** Saving the delivery form must not
  copy environment-owned values into the database, or they take over silently
  the day someone unsets the variable. See `postAdminDelivery`'s `pick`.
- **`config.RemindersFromEnv` exists because the built-in default is
  indistinguishable from a deliberate setting.** Without the flag, `len()` on
  `cfg.Reminders` is always 3 and the stored defaults are unreachable.

**Secrets are not in this.** `BITT_SMTP_PASSWORD`, `BITT_NTFY_TOKEN`, and
`BITT_TICK_SECRET` have no columns and no form fields. If a future change wants
them in the database, read the header of migration `0010` first -- the argument
is that a secret there needs a key that has to come from the environment anyway.

---

## Documented follow-ups (not started, not core)

- **Payment-made / payment-missed event notices.** Only pre-due reminders ship.
  These are event-anchored (fire on the ledger event, exempt from the
  stale-state suppression) — a natural addition once per-tab reminders land.
- **Backlog cap / per-recipient failure bounding.** The review's "cap and drain"
  SHOULD-item: after a long cron outage the scan could send a burst. Bound sends
  per tick and stop retrying a hard-failing recipient. `sent_notifications` has
  no attempt/error columns yet; add them when this is built.

---

## State at handoff (as of 0.7.1; see the top of this file for 0.7.3)

- `main` at `fa0ca90`, tree clean. Version 0.7.1. Schema at migration `0008`.
- Running: v0.7.1 on `:8080` over plain HTTP against `data/bitt.db` (real user
  accounts — do not test against it; use a scratch DB on another port).
- Full suite green including `-race`. New: `internal/notify` (delivery + safety),
  `config` reminder loading, tick auth, prefs round-trip.
- **Not visually checked with Playwright:** the profile Notifications section and
  the reminder rendering were verified by round-trip tests, not a browser
  screenshot. Worth a Playwright pass (throwaway port) before calling the UI
  done — per the standing preference, never against `:8080`.
