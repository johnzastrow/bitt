# BitTabby — v1 Roadmap

**Governing rule:** a usable application exists after Phase 1. Every later phase adds to something a person can already open and use.

Requirement text lives in [REQUIREMENTS.md](../REQUIREMENTS.md); reasoning lives in [PROJECT.md](../PROJECT.md). This file holds only what those two do not: each phase's goal, its exit criteria, and the risk it carries.

---

## Phase 1 — Walking skeleton  -- COMPLETE

**Goal:** a single binary you can run, log into, and see a tab with a correct balance.

16 requirements: LEDGER-01, 03, 04, 05, 06; TAB-01, 04, 05; CHG-03; AUTH-01, 02, 03; UI-02; DEPLOY-01, 02, 04

**Exit criteria**
- `go run .` starts a server against a fresh SQLite file, migrations applied automatically
- First-run setup screen creates the admin, then refuses to run again
- Admin logs in, creates a Services tab with line items, posts a one-off charge
- The tab shows a balance derived by summing entries, in integer cents
- Attempting an UPDATE or DELETE against the entries table aborts
- The binary runs with no external files present
- Usable on a phone-sized viewport

**Risk:** the temptation to build the schedule engine here. Charges in Phase 1 are posted by hand. Recurrence is Phase 3.

---

## Phase 2 — The settle loop  -- COMPLETE

**Goal:** the product. Two people, one tab, payments recorded and undone, balance always right.

14 requirements: LEDGER-02, 07; TAB-03, 06; PAY-01, 02, 03, 04, 05; AUTH-04, 05; UI-01, 03, 04

**Exit criteria**
- Admin adds a second user; Provider attaches them as Payee and the tab appears on their dashboard
- Payee settles in one tap plus one confirmation, all fields prefilled, with immediate visual feedback
- Provider records a payment on the Payee's behalf, attributed correctly
- Undo produces a reversing entry; the original remains visible
- Paying ahead produces a credit that offsets the next charge
- A user cannot read or write a tab they do not participate in, verified by test
- A double-submitted settle produces exactly one entry
- Dashboard shows tab cards with balance and status, styled from design tokens

**Risk:** AUTH-05 must land here, not later — this is the phase that introduces a second participant. LEDGER-07 must land here too; retrofitting idempotency across an existing write path is the expensive version.

---

## Phase 3 — Recurrence  -- COMPLETE

**Goal:** tabs charge themselves on a schedule, and both parties can see what changed.

8 requirements: SCHED-01, 02, 03, 04, 05; CHG-01, 02, 04

**Exit criteria**
- All four schedule kinds compute correct period boundaries, including a day-31 anchor across February and back
- Opening a tab posts every period that has come due, and posts each exactly once
- Concurrent reads of an overdue tab produce one entry per period, verified under parallel load
- Changing an item amount takes effect next period and leaves posted entries untouched
- A period statement shows the charge, its item breakdown, the due date, and payments applied
- Opening a tab left alone for six months posts six months of periods correctly

**Risk:** date arithmetic. This phase deserves a table-driven test suite covering DST transitions, month-end anchors, and leap years before the UI work begins.

**How it landed.** The arithmetic went into `internal/schedule` as pure civil-date
functions with no database and no clock, and the test suite was written before
any UI. That ordering paid for itself immediately: it caught that `time.Date`
resolves a non-existent local midnight *backwards* to 23:00 the previous day, so
on a zone that shifts at midnight a period's charge would have carried the wrong
date. Billing timing became a per-tab Provider choice rather than a convention --
in advance or in arrears -- since a retainer and metered work genuinely differ.

Three things arrived here that were not in the phase's list, all in response to
using it: a settle can now be for any amount rather than only the whole balance;
a tab's name, description, and kind are editable after creation, and a tab can
be archived; and TAB-02 came forward from Phase 4, because a Payoff tab was
already schema-legal and only the UI was withholding it. Phase 4 still owns
PAYOFF-01 to 03 -- progress against the original total, on-track status, and
auto-settling at zero.

**AUTH-05 was amended** to let a global administrator manage the settings of any
tab, so a tab whose Provider has left can be repaired. Administrators still
cannot move money on a tab they do not belong to, and the access is logged.

---

## Phase 4 — Payoff tabs and late fees  -- COMPLETE

**Goal:** loans that track against a schedule, and overdue periods that cost something.

11 requirements: TAB-02; PAYOFF-01, 02, 03; FEE-01, 02, 03, 04, 05, 06, 07

**Exit criteria**
- A Payoff tab shows remaining balance, progress against the original total, and on-track / ahead / behind status
- A Payoff tab hitting zero is marked settled and leaves the active dashboard
- A fixed fee posts once the grace period elapses on an unpaid period, and only once
- A percentage fee is computed on the overdue period charge, never on a balance containing fees
- Fee rounding to whole cents is deterministic and covered by tests
- Accrued fees stop at the configured cap
- A waiver posts a reversing entry carrying its reason

**Risk:** fee assessment reuses the Phase 3 lazy path. If it grows its own posting mechanism instead, that is a signal to stop and reconcile the two.

**How it landed.** Payoff follows the "Model A" decision: the principal is charged
once, payments draw it down, and the schedule describes expected *payments* (hard
dates), not charges -- so a Payoff tab posts no period charges (the Phase 3
behaviour was corrected for it). Late fees assess per period on a shortfall
against the expected installment, judged in each period's own window so a paid
period is never dragged down by an earlier miss; they never compound and a cap
bounds them; a waiver is a reversal carrying a reason. All of it accrues lazily
through the same claim-table mechanism as scheduled charges (posted_fees,
posted_interest), one claim per period, exactly-once under concurrent reads.

Two things arrived beyond the requirement list, both by request. **Declining-
balance interest** on Payoff loans, accrued per period on the outstanding balance
(it compounds on unpaid interest -- which a loan is meant to do and a fee never
is), posted as a charge sub-typed `interest` via a new column, because the
entries.kind CHECK cannot gain a value without a table rebuild the migration
harness cannot do safely. And an **in-app upcoming-payment notice** shown within
two weeks of a due date -- the pure-computation half of the reminder; the pushed
email/ntfy half is Phase 5. Version display (a `version` package, shown in the
footer and healthz) and a header logo also landed here.

---

## Phase 5 — Notifications  -- COMPLETE (0.7.0)

**Goal:** payment requests and event notices reach people who do not have the app open.

Not in the original requirement list; added when the user asked for reminders.
Email and ntfy delivery of: a payment request two weeks before a due date, a
reminder one week and one day before (all configurable), and notices on a
payment made and a payment missed, to all parties on a tab.

**Delivered (0.7.0):** email + ntfy reminders at 14/7/1 days before due, driven
by an authenticated `/internal/tick` cron endpoint (shared secret, constant-time,
fail-closed, read-only scan), send-then-claim via the `sent_notifications` claim
table, per-user channel + topic preferences. Built to `docs/SECURITY-PHASE5.md`.

**Completed (0.7.3):** per-tab, Provider-set reminders -- the lead times and the
message text, on a Reminders card in each tab's Setup group (migration `0009`,
`tab_reminders`). The instance-wide env config is the fallback for any tab the
Provider has not customised. This was the phase's stated model; the env-only
version was the fallback layer, not the end state.

**Completed (0.7.4):** an admin Notifications screen (`/admin/notifications`,
migration `0010`) for the non-secret instance config -- SMTP server/port/user/
from, the ntfy URL, and the default reminder set. Secrets stay environment-only
by design and are reported as set/not-set, never displayed. The environment wins
over stored delivery settings so a container stays reproducible.

Follow-up (not core): payment-made/missed event notices, a backlog cap.

**Exit criteria (draft)**
- Delivery is driven by an external cron hitting an authenticated `/internal/tick`
  endpoint -- no timer inside the binary, keeping the Out-of-Scope rule (a
  background scheduler is what stalled the predecessor)
- Sends are idempotent via a sent-notifications claim table: a re-run of the same
  hour sends nothing twice, a missed hour sends late rather than never
- Per-user, per-event delivery preferences; secrets (SMTP, ntfy) via env or file

**Risk:** this is the first outbound side effect in the app. It must stay off the
balance path entirely -- a failed or double send can never affect a ledger.

---

## Phase 6 — Ship  -- IN PROGRESS (3 of 5, 0.8.0)

**Goal:** someone other than the author can deploy it.

5 requirements: UI-05; DEPLOY-03, 05, 06, 07

**Delivered (0.8.0):** DEPLOY-05, 06, 07. A ~25MB distroless image running as
uid 65532, `compose.yaml` with a reminder sidecar, CI and a ghcr.io release
workflow with provenance attestation, `bittabby --healthcheck`, and
[../DEPLOY.md](../DEPLOY.md). The restore path was exercised: the data volume
was destroyed outright and the data came back.

**Remaining:** DEPLOY-03 (MariaDB) and UI-05 (PWA). MariaDB is the one with
real risk; the PWA is self-contained.

**Exit criteria**
- MariaDB selectable by configuration, with the full test suite green against both backends
- ~~Docker image published via GitHub, running as non-root~~ (0.8.0)
- ~~Secrets read from environment or file; none present in the image or the repository~~ (0.8.0)
- App installs to a phone home screen and opens to a cached shell
- ~~Backup and restore documented, and the restore path actually exercised once~~ (0.8.0)

**Risk:** MariaDB is where SQLite-only assumptions surface. DEPLOY-02 in Phase 1 is what keeps this from becoming a rewrite — if this phase requires touching call sites, the repository interface leaked.

---

## Deliberately not in v1

Offline writes and sync, invoice entities, notifications, PostgreSQL, OIDC, theme switching, reporting and exports, search, messaging, receipt attachments, external integrations. See ../PROJECT.md for the reasoning on each.
