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

## Phase 3 — Recurrence  -- NEXT

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

---

## Phase 4 — Payoff tabs and late fees

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

---

## Phase 5 — Ship

**Goal:** someone other than the author can deploy it.

5 requirements: UI-05; DEPLOY-03, 05, 06, 07

**Exit criteria**
- MariaDB selectable by configuration, with the full test suite green against both backends
- Docker image published via GitHub, running as non-root
- Secrets read from environment or file; none present in the image or the repository
- App installs to a phone home screen and opens to a cached shell
- Backup and restore documented, and the restore path actually exercised once

**Risk:** MariaDB is where SQLite-only assumptions surface. DEPLOY-02 in Phase 1 is what keeps this from becoming a rewrite — if this phase requires touching call sites, the repository interface leaked.

---

## Deliberately not in v1

Offline writes and sync, invoice entities, notifications, PostgreSQL, OIDC, theme switching, reporting and exports, search, messaging, receipt attachments, external integrations. See ../PROJECT.md for the reasoning on each.
