# Changelog

All notable changes to BitTabby are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/), and the project uses semantic
versioning. Pre-1.0, the minor version tracks the delivered phase.

The version is defined once, in `internal/version`, shown in the app footer and
in the `/healthz` response, and a build stamps in the commit and date.

## [0.7.2] - 2026-07-24 — Running balances and the projected payment schedule

### Added
- **A balance column in the tab history.** Every entry now shows the tab balance
  as it stood immediately after it, so the history reads like a bank statement.
  Rows run newest first, and the top row is the balance in the tab's header by
  construction -- the column is walked back from that figure, so the two cannot
  disagree.
- **A collapsible Payments table on Payoff tabs**, projecting every payment
  still to come: payment date, amount, and the balance left owed after it, down
  to zero. Its folded summary carries the count, the total cost to finish, and
  the projected payoff date. `loan.Project` runs the same per-period arithmetic
  as `loan.Simulate` -- U.S. Rule allocation, interest rounded once per period
  -- and a test pins the two together, so the schedule agrees with what the
  ledger will actually post. Nothing is stored; it is derived on each render
  from the entries that produce the balance, and it is empty whenever there is
  nothing honest to project (no schedule, no expected payment, already settled,
  or a payment too small to ever retire the loan -- that last is the true-up
  banner's business).

### Changed
- **The built-in reminder message now uses every template variable**, so the
  shipped default doubles as the worked example an administrator edits from.
  Title: `{tab}: {amount} due {when}`. Body: `Your {days}-day reminder: a
  payment on the tab "{tab}" is due {when}, on {due}.` / `{amount} is owed.` /
  `{url}`. Overriding it works exactly as before via `BITT_REMINDER_TITLE` /
  `BITT_REMINDER_BODY` and their per-lead-time variants.

## [0.7.1] - 2026-07-24 — Configurable reminder days and messages

### Added
- **Reminder lead times are configurable** via `BITT_REMINDER_DAYS` (e.g.
  "14,7,1"), defaulting to the built-in 14/7/1. Deduped and validated.
- **Reminder messages are configurable templates** with variables filled per
  send: `{tab}`, `{amount}`, `{due}`, `{days}`, `{when}`, `{url}` (a link to the
  tab's payment page, from `BITT_BASE_URL`). Set a default via
  `BITT_REMINDER_TITLE` / `BITT_REMINDER_BODY`, or override one lead time with
  `BITT_REMINDER_TITLE_<d>` / `BITT_REMINDER_BODY_<d>`. Templates are admin
  (env) text; a `{tab}` value with a control character still fails the send
  closed via the sender's header check.

### Note
- These are INSTANCE-WIDE defaults. Per-tab, provider-set reminders are the
  intended next step (see HANDOFF.md) and will override these defaults.

## [0.7.0] - 2026-07-24 — Phase 5: notifications

Payment reminders reach people who do not have the app open, over email and
ntfy, driven by an external cron. Built to the pre-implementation security
design (docs/SECURITY-PHASE5.md); every control there is in place, and the whole
feature stays off the balance path -- a failed or double send can never touch a
ledger.

### Added
- **Email and ntfy delivery** of payment reminders at 14, 7, and 1 days before a
  due date, to the payees on a tab. `internal/notify` is the delivery package
  and the security surface: it rejects (never strips) control characters in
  every header value, keeps all user text -- tab names, memos -- in the message
  body, refuses to follow redirects, and bounds each send with a timeout.
- **`POST /internal/tick`**, the cron entry point. Outside `requireAuth` (a cron
  has no session), authenticated by a shared secret in an `Authorization: Bearer`
  header, constant-time compared, **failing closed when unset**, checked before
  any work, and rate-limited. The scan it runs is read-only with respect to the
  ledger.
- **The sent-notifications claim table** (migration 0008), send-then-claim
  (at-least-once): deliver first, claim only on confirmed success, so a transient
  failure re-sends rather than dropping the notice. Before sending, live state is
  re-derived -- a paid-early or settled tab is never dunned.
- **Notification preferences** on the profile: per-channel email/ntfy toggles and
  an ntfy topic, validated to the same strict charset the sender enforces. The
  section that was deferred from 0.6.0 now that there is delivery behind it.
- **Config**: SMTP settings, an admin-pinned `BITT_NTFY_URL` (users choose only a
  topic -- the SSRF decision), `BITT_TICK_SECRET`, and `BITT_BASE_URL` for links.
  All via `file:`-capable, fail-closed config loading.

### Security notes
- ntfy SSRF policy is tuned for a self-hosted container: the ntfy host is admin
  config, so LAN/private addresses (a sidecar or LAN ntfy box) are allowed, while
  loopback and link-local -- the cloud metadata endpoint -- are refused, checked
  at dial time to defeat DNS rebinding.
- The tick endpoint never provokes ledger accrual, so an outbound-triggered,
  cron-reachable endpoint cannot become a balance-path write.

### Still open (documented for a follow-up)
- Payment-made / payment-missed event notices (only the pre-due reminders ship).
- A backlog cap and per-recipient failure bounding on the scan.

## [0.6.4] - 2026-07-24 — Pre-Phase-5 security design and hardening

Phase 5 (notifications) is the app's first outbound side effect and first reach
to external hosts, so a security design review was run before any code: a
multi-agent threat model (5 reviewers by lens, every finding adversarially
verified, then synthesis). 34 findings raised, 15 refuted, 19 survived. Net risk
is low-to-medium and entirely off the balance path.

### Added
- **`docs/SECURITY-PHASE5.md`** — the pre-implementation security design the
  phase is gated on. Captures the must-fix controls (stale-reminder suppression,
  SSRF containment on ntfy, email/ntfy header injection, `/internal/tick` auth
  and its balance-path hazard, delivery semantics, secret hygiene) and the three
  decisions the user must make first: the ntfy SSRF policy, at-most-once vs
  at-least-once delivery, and the tick-auth mechanism.

### Fixed (in-repo gaps the review confirmed, both correct regardless of Phase 5)
- **The profile email edit admitted control characters.** It checked only for an
  `@`, weaker than the `looksLikeEmail` used at setup and admin creation, so a
  CR/LF could enter the stored login identity — header injection waiting for a
  mail sender to land. Now uses `looksLikeEmail`.
- **An unreadable `file:` secret silently fell back to a blank default.**
  `config` reworked so a `file:`-supplied value that cannot be read fails the
  load instead of degrading to empty (fail-closed, which the Phase 5 secrets and
  the tick endpoint's own fail-closed guard depend on). Added a permission
  warning when a `file:` secret is group/other-readable.

### Tests
- `internal/config` gains tests (0% -> ~69%): fail-closed on an unreadable
  secret file, reading a secret file, defaults, and bad-timezone rejection.
- `internal/auth` session and CSRF managers gain direct unit tests (42% -> ~90%):
  session round-trip and digest-only storage, fail-closed resolution, expiry,
  Revoke vs RevokeOthers, CSRF double-submit and its rejections, DummyVerify.

## [0.6.3] - 2026-07-24 — Avatars in history rows

### Changed
- **The ledger history now shows who recorded each entry with a face**, not only
  a name, completing the avatar treatment across every place a person appears.
  The actor lookup already loaded the full user, so no extra query was added.
- `initials` was hardened to take the first *letter* of each word, skipping
  punctuation, so a removed account renders "R" rather than "(" and CJK names
  keep their first character. Covered by a unit test.

## [0.6.2] - 2026-07-24 — Avatars everywhere a person is named

### Changed
- **A person's picture now shows wherever they are named**, not only in their
  own header: the tab participants list and the admin users list. Uploading a
  picture and seeing it in one place but nowhere else read as half-finished.
  People without a picture keep their initials fallback, so every slot renders
  something.
- The `AvatarImage` component was generalised to `Avatar(id, name, timestamp)`
  from its old `store.User` argument, so anything that names a person can render
  a face; a `UserAvatar` wrapper keeps the common User case tidy.
- `store.Participant` now carries `AvatarUpdatedAt`, populated by the existing
  `ListParticipants` join, so a participant row shows a picture without a
  second query per row.

## [0.6.1] - 2026-07-24 — Settle buttons pay a period, not the whole balance

### Changed
- **The dashboard's primary settle button now offers one period's payment**, not
  the entire outstanding balance. On a Payoff loan that means the scheduled
  installment (e.g. "Pay $505.65") rather than "Settle $21,966.00"; on a
  Services tab it means one period's charge rather than every period that has
  accrued. Paying the period is the ordinary act, so it is the one-tap default;
  clearing a whole loan is the exception and lives behind "Other amount", which
  still prefills the full balance. The same default now prefills the tab page's
  payment field, so a full loan payoff is a deliberate edit rather than an
  accidental tap.
- The button reads "Pay $X" for a period payment and "Settle $X" when that
  amount clears the whole balance, since the two are different acts. When less
  than a period remains it settles what is left rather than overshooting an
  installment into credit. A hand-billed tab with no periodic amount keeps the
  settle-the-balance button, since there is no period to offer.

### Notes
- Verified with Playwright against the layout from the reported screenshot: the
  loan card offers "Pay $505.65" beside "Other amount".

## [0.6.0] - 2026-07-23 — Account profiles

Click your own name in the header to reach `/profile`.

### Added
- **Profile settings**: display name, email, password, and a picture, all
  self-service. Reached by clicking your name in the header, which is where
  people look for them.
- **Avatars.** Upload a PNG, JPEG, or GIF up to 2 MB; it is cropped square,
  scaled to at most 256px, and re-encoded as PNG. **What is stored is never what
  was uploaded** — decoding and re-encoding strips EXIF and GPS, discards data
  appended after the image, and means a file crafted to parse as two formats at
  once survives as neither. The filename is never read or stored, which makes
  path traversal structurally impossible rather than merely handled.
  Accounts without a picture get initials on a colour derived from their id, so
  every avatar slot renders something rather than a broken-image icon.
- **`internal/avatar`**, the whole upload surface in one package: a byte cap
  applied while reading, a magic-byte allowlist that ignores the declared
  content type, and **a pixel-dimension check taken from the header before
  decoding** — the control a size cap cannot provide, since a 30,000 x 30,000
  PNG compresses to a few kilobytes and expands to gigabytes.
- **Rate limiting on the upload route**, the first in the app. Image decoding is
  the only meaningful CPU work a request can trigger here, and an authenticated
  user can ask for it in a loop; ten uploads a minute per account, a fixed
  window rather than a general framework for one endpoint.
- **Migration 0007** adds `users.avatar_png` and `users.avatar_updated_at`. The
  timestamp is the ETag on the avatar route, so a browser revalidates cheaply
  and a changed picture still appears at once.

### Changed
- **Changing your password now signs out every other device**, keeping the one
  making the change. That is usually the point of changing it. It requires the
  current password, refuses a password identical to the current one, and
  confirms how many devices were signed out.
- **Changing your email requires your current password**, because the email is
  the login identity: an unattended session that could silently move the address
  elsewhere is a full account takeover, since recovery would follow the new
  address.

### Fixed
- **`GetSession` did not select `avatar_updated_at`.** It joins `users` with its
  own hand-written column list rather than reusing `userColumns`, so the
  authenticated user on every request carried a zero value and the header always
  fell back to initials even after an upload. Caught by a test; the hazard is now
  recorded in the data model doc, since any future `User` column has the same
  trap waiting.

### Deferred
- **Notification preferences.** They belong on this page but there is no
  delivery to configure yet, and switches that do nothing are worse than no
  switches. They land with Phase 5, where the per-event list is already designed.

## [0.5.2] - 2026-07-23 — Create-form layout, and assets that actually update

### Fixed
- **Static assets were cached for an hour behind a stable URL.** The reasoning
  in `static.go` was that replacing the binary made a long `max-age` safe; it
  does not, because it does nothing to a stylesheet a browser has already
  cached. The 0.5.1 kind-scoped fields therefore did not appear at all for
  anyone who had loaded the app in the previous hour, and the feature looked
  broken rather than stale. Asset URLs now carry a digest of the embedded
  content (`/static/app.css?v=…`), so a change to the stylesheet changes the
  URL. A request carrying the current digest is cached for a year and marked
  immutable; anything else gets 60 seconds, so a stale copy heals quickly.
  The digest hashes content rather than the version constant, since the version
  does not change between development builds but the stylesheet does.
- **`input[type="number"]` and `input[type="date"]` were never styled.** They
  fell outside the selector list and rendered at browser defaults — wrong width,
  no padding, no border radius — beside styled text fields. Visible as soon as
  0.5.1 added two number inputs, but the date input had been wrong since the
  schedule form shipped. A test now walks the rendered forms and fails if any
  input type in use is missing from the stylesheet.
- **The create form was cramped.** Bordered fieldsets nested inside a bordered
  card inside a 26rem column stacked three sets of padding. The kind-scoped
  groups are now plain sections with a small heading, and the form has its own
  34rem width rather than borrowing the login box's.
- **The kind radio sat above its own label text** instead of beside it:
  `.stack label` sets `flex-direction: column`, which won because the new rule
  never declared a direction.

### Notes
- Verified with Playwright against a throwaway instance: the Payoff and Services
  halves show and hide correctly, and the text, number, and date inputs now
  share a width and left edge.

## [0.5.1] - 2026-07-23 — Timezone picker and kind-scoped tab fields

### Added
- **Timezone autocomplete on the setup screen.** The field was free text, which
  asked people to recall an IANA name exactly. It now offers 410 zones through a
  `<datalist>`, so typing "York" finds `America/New_York` — a native `<select>`
  keyboard-searches only from the start of the value and would never find it.
  Common zones are listed first; the rest are alphabetical. The form now
  proposes `America/New_York` rather than `UTC`; `BITT_TIMEZONE` still overrides
  it, and UTC remains the *fallback* when a stored zone fails to load, because a
  broken value should degrade to something neutral rather than to a guess that
  shifts every boundary five hours.
- **`internal/tz`**, holding the embedded zone list. Go exposes no way to
  enumerate the zones inside `time/tzdata`, and reading `/usr/share/zoneinfo` at
  runtime would give a full list in development and an empty one in a scratch
  container — the exact case the embedded tzdata exists for. The list is
  generated and committed, the same arrangement as the vendored htmx asset, and
  every name is filtered through `time.LoadLocation` before being offered.

### Changed
- **The create form shows only the fields belonging to the chosen tab kind.**
  Loan amount, interest, term, and payment appear for Payoff; line items for
  Services. The kind is now a radio group, and the stylesheet hides the
  irrelevant half via `:has()` on the checked radio — no JavaScript, which the
  content-security policy forbids inline, and no round trip. Where `:has()` is
  unsupported every field shows, which is the previous behaviour.
  Nothing kind-scoped is `required`, since a hidden required field blocks
  submission with a message the browser cannot display; validation stays
  server-side, by kind, as it already was.

### Fixed
- **The binary accepted no arguments and silently ignored them**, so
  `bittabby --version` looked like a question and was answered by starting a
  server against `BITT_DB_PATH` — which on one occasion applied a forward-only
  migration to a database that was only meant to be inspected. `--version` and
  `--help` now answer without touching the database, and anything unrecognized
  exits 2 with usage.

### Notes
- The timezone list is a convenience, never the authority. A zone the running
  tzdata can load is accepted whether or not the committed list has caught up,
  and an unknown one is still rejected by `time.LoadLocation`.

## [0.5.0] - 2026-07-23 — Loan terms, scheduled payments, and schedule intervals

A Payoff loan can now state how long it runs and what it costs per period, and
the app works out that payment itself so a lender's figure can be checked
against it. Verified against an issued amortization schedule: $21,852.48 at
5.24% over 48 monthly payments, quoted at $505.65 and computed here at $505.63 —
a two-cent difference that is the lender's own rounding.

### Added
- **Loan term and scheduled payment** on Payoff tabs, each in its own field. The
  Provider enters the payment the lender actually charges, because that is the
  figure that has to be paid; the app computes what the terms imply and shows
  the two side by side rather than overriding one with the other.
- **A suggested payment**, derived by simulating the same arithmetic the ledger
  posts — the same per-period rounding, the same allocation — rather than by the
  closed-form annuity formula. The closed form needs a float, which is forbidden
  in the money path, and disagrees with integer per-period rounding by a few
  cents over a long term. The simulation cannot: it is the same arithmetic.
  Reports the final payment (smaller, as lenders adjust it) and lifetime interest.
- **A true-up**, recasting the required payment against the balance and the
  payments still remaining, and saying when the current payment will not clear
  the loan in time or will clear it early. Differences within five cents are not
  flagged, since that is a lender's rounding rather than a disagreement.
- **`internal/loan`**, a fourth pure package beside money, schedule, and fee.
  No I/O, no clock. Pinned by tests against both the closed-form annuity value
  and an issued lender amortization schedule.
- **Arbitrary schedule intervals** — every N weeks, every N months. "Every three
  weeks" was previously unrepresentable.
- **Maturity date and payment count** on the loan progress panel.
- **Adjustments in the interface** (CHG-03). The Provider can correct what is
  owed in either direction, with a required reason, from Day to day → "Adjust
  what is owed". `ledger.Adjustment` had existed since Phase 1 and was reachable
  from no route, which left a reconciliation with no honest home: the only way
  to reduce a balance was to record a payment that never happened, which claims
  money changed hands, satisfies the period's late-fee window, and on a loan is
  allocated to interest before principal. **A credit is allocated
  principal-first**, deliberately unlike a payment — a payment settles the
  oldest thing owed, while a credit is the Provider deciding part of the debt
  should not exist, and taking it off principal makes the reduction permanent
  and every later interest charge smaller. Credits count toward meeting the
  schedule, so forgiving what was expected clears "Behind"; they are excluded
  from payment totals, since an adjustment is not money.
- **`docs/DATA-MODEL.md`** — the physical schema with a mermaid ER diagram, a
  per-column data dictionary with rules, the database invariants, and the loan
  arithmetic conventions in one place.

### Changed
- **Interest no longer compounds on unpaid interest.** Interest now accrues on
  outstanding principal only; interest a short payment did not cover is owed,
  and is repaid before principal, but does not itself accrue. This is the U.S.
  Rule, which Regulation Z permits for closed-end credit and which consumer
  lenders run. The previous behaviour capitalized it, which meant a borrower who
  missed a payment and caught up would separate permanently from what their bank
  said — with no way back. The old rule was documented but never tested; it is
  now tested in both `internal/loan` and `internal/ledger`.
- **The per-period interest rate is now an exact fraction of a year** rather than
  APR divided by a periods-per-year count, which had no answer for an interval
  like three weeks (52/3 is not an integer). Month-stepping schedules use
  months/12, the 30/360 basis a US installment loan is quoted on, so a plain
  monthly loan is still exactly APR/12. Week-stepping schedules use days/365,
  actual/365. **Weekly and biweekly tabs carrying interest will see a very
  slightly different figure on future periods** (APR × 7/365 rather than APR/52);
  interest already posted is immutable and stands.
- **A Payoff tab's expected payment comes from its own field, not its line
  items.** Items are a Services tab's period charge. One field meaning both "what
  is charged" and "what is expected" is what let a mis-configured loan read as
  already settled. Payoff tabs no longer offer the line-item editor; existing
  items are shown, read-only, rather than deleted.
- **`biweekly` is now `weekly` with an interval of 2.** Both compute period n as
  anchor + 14n, so the rewrite is date-identical: no period key moves and no
  posted cycle can re-bill. Verified against the demo database.

### Fixed
- **The true-up flagged harmless overpayments.** Paying more than the term
  strictly requires is now only reported when it actually shortens the loan.
  A small credit, or a lender's payment rounded up, previously produced a notice
  reading "48 payments rather than 48".
- **Payoff progress and late fees stopped short on an interest-bearing loan.**
  Both capped expectations at the principal, which is less than the loan costs
  once interest is charged — so the schedule stopped expecting payments partway
  through, a borrower paying exactly on time could read as behind, and the last
  months of a term went unfined. Expectations now span the term.

### Migration
`0006_loan_terms` adds `schedule_interval`, `loan_term_periods`, and
`loan_payment_cents`; rewrites `biweekly` schedules; and backfills each Payoff
tab's payment from the sum of its active line items — exactly what the old code
read, so no tab's expectations change on upgrade. **A Payoff tab that was set up
with the loan amount in a line item will backfill that amount as its payment**
and should be corrected in Setup → Loan term and payment.

## [0.4.0] - 2026-07-23 — Payoff tabs and late fees

### Added
- **Payoff loans** (TAB-02, PAYOFF-01/02/03). A loan is a principal charged once
  and drawn down by payments; the schedule describes expected payment dates. A
  loan shows its remaining balance, progress against the original principal, and
  on-track / ahead / behind status, and drops off the active dashboard when
  settled. Payoff tabs post no scheduled charges.
- **Late fees** (FEE-01 through 07). Fixed or percentage, with a grace period and
  an optional cap, per tab. A fee is assessed once per missed period, judged in
  that period's own payment window, and never compounds — a percentage is taken
  on the period charge, not on a balance containing fees. Fees accrue lazily on
  read through a `posted_fees` claim table, exactly once even under concurrent
  reads. A fee can be waived, which records a reversing entry with a reason.
- **Declining-balance interest** on loans (by request, beyond the requirement
  list). Interest accrues each period on the loan's outstanding balance, so it
  falls as the loan is paid down and compounds on unpaid interest. Posted lazily
  like fees, one claim per period.
- **In-app upcoming-payment notice**, shown within two weeks of a due date.
- **Version display**: an `internal/version` package, shown in the footer (with
  the full build string on hover) and in `/healthz`.
- **Logo** in the header, linking home.

### Changed
- Settling from the dashboard now accepts any amount, not only the full balance.
- A tab's name, description, and kind are editable after creation; tabs can be
  archived; participants can be removed. A global administrator may manage the
  settings of any tab (an amendment to AUTH-05, logged), but may not move money
  on a tab they do not participate in.

## [0.3.0] - 2026-07-23 — Recurrence

### Added
- Recurring schedules (weekly, biweekly, monthly-on-day, monthly-last), billed in
  advance or in arrears (SCHED-01/02/05). Due periods post lazily inside the read
  transaction — no background scheduler — and catch up whenever a tab is opened
  (SCHED-03), exactly once even under concurrent reads (SCHED-04).
- Per-period statements computed from ledger entries (CHG-01/04); item changes
  take effect the following period and never alter posted entries (CHG-02).

## [0.2.0] - 2026-07-23 — The settle loop

### Added
- Payments, one-tap settle from the dashboard, payment on another's behalf, undo
  as a reversing entry, and pay-ahead credit (PAY-01 through 05, LEDGER-02/07).
- A second participant and per-tab authorization (TAB-03, AUTH-04/05).

## [0.1.0] - 2026-07-23 — Walking skeleton

### Added
- A single static binary: append-only ledger, integer-cent money, first-run
  admin setup, tabs with line items, balances derived by summing entries, and a
  mobile-first interface (LEDGER, TAB, AUTH, DEPLOY, UI foundations).

[0.4.0]: #040---2026-07-23--payoff-tabs-and-late-fees
[0.3.0]: #030---2026-07-23--recurrence
[0.2.0]: #020---2026-07-23--the-settle-loop
[0.1.0]: #010---2026-07-23--walking-skeleton
