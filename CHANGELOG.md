# Changelog

All notable changes to BitTabby are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/), and the project uses semantic
versioning. Pre-1.0, the minor version tracks the delivered phase.

The version is defined once, in `internal/version`, shown in the app footer and
in the `/healthz` response, and a build stamps in the commit and date.

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
