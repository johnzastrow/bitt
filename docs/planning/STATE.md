# BitTabby — Current State

**Updated:** 2026-07-23
**Paused for a reboot after Phase 2.** See [HANDOFF.md](HANDOFF.md) to pick this up cold.

## Where things stand

**Phases 1 and 2 complete and verified.** 30 of 54 v1 requirements delivered.

| Item | State |
|------|-------|
| Repository | `main`, five commits, working tree clean |
| Scope | 54 requirements, 5 phases |
| Stack | Go 1.26 + templ + htmx 2.0.4 (vendored), SQLite |
| Phase 1 | Complete — walking skeleton |
| Phase 2 | Complete — the settle loop |
| Phase 3 | Not started — recurrence |

## What works today

The product is usable. Two people, one tab, money tracked correctly.

```
make build && ./bittabby         # :8080, see .env.example for configuration
```

Run it, complete first-run setup, and you can: add a second person, create a
tab with line items, post charges, attach the other person as payee, settle in
one tap plus one confirmation from the dashboard, record a payment on someone
else's behalf, undo any of it as a reversing entry, and pay ahead to build a
credit that offsets the next charge.

## Requirements delivered

| Phase | Requirements |
|-------|--------------|
| 1 | LEDGER-01, 03, 04, 05, 06; TAB-01, 04, 05; CHG-03; AUTH-01, 02, 03; UI-02; DEPLOY-01, 02, 04 |
| 2 | LEDGER-02, 07; TAB-03, 06; PAY-01, 02, 03, 04, 05; AUTH-04, 05; UI-01, 03, 04 |

## Verification performed

- Full suite green, including under `-race`
- Coverage: money 96%, ledger 84%, sqlite 72%, web 68%, auth 44%
- Live binary walked end to end over HTTP for both phases: setup, admin adds a
  second person, tab with items, charge, attach payee, payee settles in two
  taps, double-tap posts once, undo reverses, second undo refused
- Append-only triggers confirmed against the running database (UPDATE and
  DELETE both abort, balance unchanged)
- Static link confirmed (`not a dynamic executable`); restart clean; database
  files mode 0600

## Next action

**Phase 3 — recurrence.** 8 requirements: SCHED-01 through 05, CHG-01, 02, 04.
Tabs charge themselves on a schedule, and both parties can see what changed.

The load-bearing decision is already made and recorded: **due periods post
lazily inside the transaction that reads the tab**, so there is no background
scheduler, no downtime catch-up logic, and no DST tick handling. A unique
constraint on `(tab, period)` makes concurrent reads incapable of
double-posting.

Start with a table-driven test suite for the period arithmetic — all four
schedule kinds, month-end anchors, DST transitions, leap years — before any UI
work. That is where this phase's risk lives.

See [ROADMAP.md](ROADMAP.md) for the full exit criteria.

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
