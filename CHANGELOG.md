# Changelog

All notable changes to BitTabby are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/), and the project uses semantic
versioning. Pre-1.0, the minor version tracks the delivered phase.

The version is defined once, in `internal/version`, shown in the app footer and
in the `/healthz` response, and a build stamps in the commit and date.

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
