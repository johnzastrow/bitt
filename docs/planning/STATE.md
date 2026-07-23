# BitTabby — Current State

**Updated:** 2026-07-23

## Where things stand

**Phase 1 complete and verified.** 16 of 54 v1 requirements delivered.

| Item | State |
|------|-------|
| Repository | `main`, three commits |
| Scope | 54 requirements, 5 phases |
| Stack | Go 1.26 + templ + htmx (htmx arrives Phase 2), SQLite |
| Phase 1 | Complete -- LEDGER-01/03/04/05/06, TAB-01/04/05, CHG-03, AUTH-01/02/03, UI-02, DEPLOY-01/02/04 |
| Phase 2 | Not started |

## What works today

Run the binary, open it in a browser, complete first-run setup, create a tab
with line items, post charges, and see a balance derived from the ledger. The
binary is static, 11 MB, and needs nothing beside it.

```
make build && ./bittabby         # :8080, see .env.example for configuration
```

## Verification performed

- Full suite green, including under `-race`
- Live binary walked end to end over HTTP: setup, login, tab creation with
  items, two charges, dashboard balance
- Append-only triggers confirmed by attempting UPDATE and DELETE directly
  against the running database -- both aborted, balance unchanged
- Static link confirmed (`not a dynamic executable`), restart confirmed clean

## Next action

**Phase 2 -- the settle loop.** This is the phase that makes BitTabby a
product: a second participant, payments, one-tap settle, undo, credits, and
per-tab authorization. See [ROADMAP.md](ROADMAP.md).

Two items must land in Phase 2 and cannot slip:
- **AUTH-05** per-tab authorization, because Phase 2 introduces the second
  participant. The store enforces it already via participant joins; the
  requirement completes when a Payee exists to authorize against.
- **LEDGER-07** idempotency keys. The schema, unique constraint, and replay
  handling are already in place from Phase 1; what remains is emitting a
  per-render key into every form.

## Working agreement

- Build in long stretches; check in at phase boundaries or genuine design forks
- No permission needed for routine writes, tests, or commits within a phase
- Atomic commits referencing REQ-IDs

## Relationship to bit-tabby

`bitt` is a fresh start, seeded only from bit-tabby's PROJECT.md and
REQUIREMENTS.md. No code, planning artifacts, or completion status carried
over. bit-tabby remains on disk at `../bit-tabby` as a reference
implementation of the ledger core and auth, but it is not a dependency and
nothing here assumes it.
