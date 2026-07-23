# BitTabby — Current State

**Updated:** 2026-07-23

## Where things stand

Planning complete. No application code written yet.

| Item | State |
|------|-------|
| Repository | Initialized on `main` |
| Scope | Revised — 54 requirements, 5 phases |
| Stack | Go + templ + htmx, SQLite first, MariaDB in Phase 5 |
| Phase 1 | Not started |

## Next action

Begin Phase 1 — walking skeleton. See [ROADMAP.md](ROADMAP.md) for its goal and exit criteria.

## Working agreement

- Build in long stretches; check in at phase boundaries or genuine design forks
- Do not ask permission for routine writes, tests, or commits within an agreed phase
- Atomic commits referencing REQ-IDs

## Relationship to bit-tabby

`bitt` is a fresh start, seeded only from bit-tabby's PROJECT.md and REQUIREMENTS.md. No code, planning artifacts, or completion status carried over. bit-tabby remains on disk at `../bit-tabby` as a reference implementation of the ledger core and auth, but it is not a dependency and nothing here assumes it.
