# BitTabby — Current State

**Updated:** 2026-07-24

## Where things stand

**Phases 1–5 complete; Phase 6 (ship) is 4 of 5.** 53 of 54 v1 requirements
delivered. Only UI-05 (the PWA) remains.

| Item | State |
|------|-------|
| Repository | `main`, working tree clean |
| Version | 0.8.1 |
| Scope | 54 requirements, 6 phases |
| Stack | Go 1.26 + templ + htmx 2.0.4 (vendored); SQLite or MariaDB |
| Phase 1 | Complete — walking skeleton |
| Phase 2 | Complete — the settle loop |
| Phase 3 | Complete — recurrence |
| Phase 4 | Complete — payoff tabs, late fees, interest |
| Phase 5 | Complete — notifications (email/ntfy, per-tab and instance settings) |
| Phase 6 | In progress — Docker + deploy + backup/restore + MariaDB shipped; PWA remains |

## What works today

The product is usable. Two people, one tab, money tracked correctly, and now
tabs that bill themselves.

```
make build && ./bittabby         # :8080, see .env.example for configuration
```

Run it, complete first-run setup, and you can: add a second person, create a tab
with line items, give it a schedule, post charges by hand as well, attach the
other person as payee, settle in one tap plus one confirmation from the
dashboard, pay a partial or larger amount instead, record a payment on someone
else's behalf, undo any of it as a reversing entry, pay ahead to build a credit
that offsets the next charge, and read a per-period statement showing what each
cycle covered and what has been paid against it.

Tabs are Services or Payoff, stated at the top of the tab, and both kinds are
editable. A tab's name, description, and kind can be changed after creation,
people can be detached as well as attached, and a tab can be archived -- which
stops it billing and drops it down the dashboard without touching a single
entry.

## Requirements delivered

| Phase | Requirements |
|-------|--------------|
| 1 | LEDGER-01, 03, 04, 05, 06; TAB-01, 04, 05; CHG-03; AUTH-01, 02, 03; UI-02; DEPLOY-01, 02, 04 |
| 2 | LEDGER-02, 07; TAB-03, 06; PAY-01, 02, 03, 04, 05; AUTH-04, 05; UI-01, 03, 04 |
| 3 | SCHED-01, 02, 03, 04, 05; CHG-01, 02, 04; TAB-02 (pulled forward) |
| 4 | TAB-02; PAYOFF-01, 02, 03; FEE-01…07 |
| 5 | Notifications (added by request; email/ntfy reminders, per-tab + instance config) |
| 6 | DEPLOY-03 (MariaDB), 05 (Docker), 06 (secrets), 07 (backup/restore) — **UI-05 (PWA) outstanding** |

## Verification performed

- Full suite green, including under `-race`
- Coverage: fee 96%, money 96%, ledger ~90%, schedule 87%, sqlite ~73%, web ~71%, auth 44%
- Migration `0003_schedules` applied to an existing Phase 2 database without
  incident, and the demo tab kept its balance
- Live binary walked end to end over HTTP: a tab anchored ten weeks back posted
  eleven cycles on first read for the correct total, a refresh posted nothing
  more, statements rendered with due dates and breakdowns, and a payment settled
  the oldest cycle first
- **Twenty simultaneous reads of an overdue tab produced 21 claims, 21 entries,
  21 distinct period keys, and exactly the right balance** — one charge per
  cycle under real parallel load (SCHED-04)
- Changing an item's amount left the posted cycle's entry and its snapshot
  untouched, and superseded the item row rather than overwriting it
- `posted_periods`, `posted_fees`, and `posted_interest` UPDATE/DELETE all abort
- Migration 0005 applied cleanly to the existing demo database (through 0004)
- Live: a $5,000 loan at 6% accrued $25 then $23.88 interest on the declining
  balance; a waived fee added $25 back and did not re-assess; a fully paid loan
  read settled and left the active dashboard; version shows v0.4.0 in footer and healthz
- The administrator exception to AUTH-05 is covered from both sides: an admin can
  rename a tab they are not on, and **cannot** post a payment to it. That second
  test caught a real hole during development and now guards it.

## Next action

**UI-05 — the PWA.** The one requirement left in v1: a web-app manifest, a
service worker that caches the app shell, and home-screen icons, so the app
installs to a phone and opens to a cached shell (data operations still require a
connection). Self-contained; no backend risk.

**Start from [HANDOFF-PHASE6.md](HANDOFF-PHASE6.md)** — it leads with UI-05, the
concrete steps, and the app-specific traps (CSP, SW scope, cache versioning off
the existing `AssetVersion` digest, and the shell-only-not-offline-data
constraint).

After that, v1 is feature-complete and the work is release polish: a milestone
audit, a tagged `v1.0.0`, and the first published image via the release workflow.

## Working agreement

- Build in long stretches; check in at phase boundaries or genuine design forks
- No permission needed for routine writes, tests, or commits within a phase
- Atomic commits referencing REQ-IDs
- `make check` before committing

## Relationship to bit-tabby

`bitt` is a fresh start, seeded only from bit-tabby's PROJECT.md and
REQUIREMENTS.md. No code, planning artifacts, or completion status carried
over. bit-tabby remains on disk at `../bit-tabby` as a reference
implementation of the ledger core and auth, but it is not a dependency and
nothing here assumes it.
