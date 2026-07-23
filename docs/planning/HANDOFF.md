# BitTabby — Handoff

**Written:** 2026-07-23, pausing after Phase 2 for a reboot.

This is written to be read cold, by someone (or some session) with no memory of
the work. It covers what exists, why it is shaped this way, what to do next, and
the traps.

---

## Restarting after the reboot

```bash
cd /home/jcz/Github/bitt
go install github.com/a-h/templ/cmd/templ@latest   # only if `templ` is missing
make check                                          # generate, fmt, vet, test
make build && ./bittabby                            # http://localhost:8080
```

Nothing is left running. No background processes, no open ports, no
uncommitted work. `git status` should be clean at commit `9ef145e`.

The demo database from the earlier walkthrough is at `data/bitt.db`
(gitignored). Delete that file to get the first-run setup screen back.

**One environment caveat:** `templ` is a code generator installed to
`$(go env GOPATH)/bin`. The generated `*_templ.go` files **are committed**, so
the project builds without it. You only need `templ` if you edit a `.templ`
file. `make generate` runs it; every other make target depends on that.

---

## What this project is

A self-hosted, mobile-first web app for tracking money owed between people who
trust each other. Providers bill, Payees pay, and the app keeps one
authoritative running balance per tab. It never touches money — it records
payments made elsewhere.

**Core value: the running balance is always correct.** Everything else is
negotiable. That single commitment drives most of the architecture.

Read [../PROJECT.md](../PROJECT.md) for the full reasoning and
[../REQUIREMENTS.md](../REQUIREMENTS.md) for the 54 v1 requirements with stable
IDs. [ROADMAP.md](ROADMAP.md) has per-phase goals and exit criteria.

---

## The single most important piece of context

This project is a **deliberate rewrite of a predecessor that stalled**.
`../bit-tabby` scoped v1 at 79 requirements across 7 phases and spent its first
three phases on ledger core, OIDC-capable auth, and three database backends —
without ever producing a screen where a person could look at a tab.

The failure was sequencing, not engineering.

So `bitt` runs under one governing rule: **a usable application exists after
Phase 1, and every later phase adds to something you can already open and use.**
Before accepting any new requirement or refactor, check it against that rule.

These were cut or deferred deliberately. Watch for them creeping back:
offline writes and sync, invoice entities, notifications, PostgreSQL, OIDC,
theme switching, multi-currency, and **any background scheduler process**.

---

## Where things stand

Phases 1 and 2 are complete and verified. 30 of 54 requirements.

| Commit | What |
|---|---|
| `a49ce5d` | Baseline: the original bit-tabby docs, preserved |
| `a54f5f7` | Scope revision, 79 requirements to 54, reordered |
| `030a84e` | Phase 1 — walking skeleton |
| `ec5655c` | Phase 1 status |
| `9ef145e` | Phase 2 — the settle loop |

Full suite green including `-race`. Coverage: money 96%, ledger 84%,
sqlite 72%, web 68%, auth 44%.

---

## Architecture, and why

```
cmd/bittabby/          main, config load, graceful shutdown
internal/money/        Cents (int64). No float anywhere in the money path
internal/store/        persistence CONTRACT (interfaces only, no SQL)
internal/store/sqlite/ the only implementation, plus embedded migrations
internal/ledger/       the ONLY write boundary for financial entries
internal/auth/         Argon2id, sessions, CSRF
internal/web/          routes, handlers, templ views, embedded static assets
```

**`internal/ledger` is the only thing that writes entries.** Not a convention —
`store.EntryStore` exposes no update or delete method at all, and SQLite abort
triggers reject `UPDATE`/`DELETE` against `entries` outright. Three layers:
interface shape, single write path, database triggers.

**Balances are derived by summing entries.** There is no balance column, and a
test introspects the schema to keep it that way. A cache is the single most
likely way "the balance is always correct" fails.

**Sign convention:** a tab balance is **negative when the Payee owes**, positive
when they hold a credit. Callers pass positive amounts to `Charge` and
`Payment`; the ledger applies the sign. Never hand a negative amount to either.

**Credits need no machinery.** Paying ahead carries the balance positive, and
that offsets the next charge because the balance is a sum. There is no credit
ledger and no application step to drift.

**Idempotency is everywhere already.** Every rendered form carries a fresh
random key; the database has a unique constraint; a replay returns the original
entry with `replayed=true` rather than posting again. This is what makes a
double-tapped button safe, and it is also what keeps offline capability
additive if it is ever built.

---

## Phase 3 is next: recurrence

8 requirements: SCHED-01 through 05, CHG-01, CHG-02, CHG-04.

**The design decision is already made and must not be relitigated:** due periods
post **lazily, inside the transaction that reads the tab**. There is no
background scheduler, no timer goroutine, no cron. Catch-up after downtime is
inherent — whatever periods are due get posted the moment anyone looks. A unique
constraint on `(tab, period)` makes concurrent reads incapable of
double-posting.

A scheduler process is exactly what made this expensive in the predecessor. If
Phase 3 starts growing a timer, stop and re-read this paragraph.

**Where the risk actually is: date arithmetic.** Build a table-driven test suite
first, before any UI:

- All four schedule kinds: weekly, every two weeks, monthly on day N, monthly on
  last day
- Month-end anchors: a day-31 anchor must land correctly in February and return
  to the 31st in March
- DST transitions in the instance timezone
- Leap years
- A tab left alone six months must post six months of periods, each exactly once
- Concurrent reads of an overdue tab must produce one entry per period

**Suggested shape:** a new `internal/schedule` package holding pure period
arithmetic with no database dependency, tested exhaustively on its own, then
wired into the tab read path. Keeping the arithmetic pure is what makes it
cheap to test the nasty cases.

**Schema you will need:** a `posted_periods` table with a unique constraint on
`(tab_id, period_key)`, plus schedule columns on `tabs` (anchor date, kind,
interval). Migration `0003_schedules.sql`. Follow the pattern in
`0001_initial.sql` — the header comments there explain the portability rules
that keep MariaDB viable in Phase 5.

Note that `entry_items` already snapshots the item breakdown at post time, so
CHG-01 is largely wiring rather than new persistence.

---

## Traps and things that will bite you

**`ListTabsForUser` returns newest-first.** A test helper indexed into it
positionally and silently operated on the wrong tab. Match by name or id.

**`pkill -f 'bitt/bittabby'` kills your own shell**, because the pattern matches
the shell's own command line. Find the process by port instead:
`ss -ltnpH "sport = :8080" | grep -oP 'pid=\K[0-9]+'`.

**`curl -X POST` with `-L` forces POST across the 303 redirect**, landing on a
route that does not exist and returning an empty body. Use `-d` without `-X`;
curl then correctly switches to GET on the redirect.

**SQLite creates database files honoring the umask**, which is world-readable.
`restrictPermissions` chmods them to 0600 on open and again after migration
(the WAL and shm sidecars only appear on first write). Do not remove it.

**`go:embed` cannot reach outside its own package directory.** Migrations live
at `internal/store/sqlite/migrations/` for exactly this reason.

**Deactivating the last admin is guarded inside the write transaction**, not in
the handler. A check-then-act in the handler lets two concurrent requests both
see a spare admin and both proceed, locking everyone out of the instance. There
is a concurrency test for this. Do not move the check up a layer.

**Foreign tabs answer 404, not 403**, so tab ids cannot be enumerated. Same for
admin routes to non-admins. This is deliberate; do not "fix" it to 403.

---

## Deliberate deviations worth knowing

**Idempotency keys landed in Phase 1** even though LEDGER-07 is a Phase 2
requirement, because PROJECT.md commits to idempotent writes from the first
commit and retrofitting them across an existing write path is the expensive
version. Phase 2 only had to emit keys into forms.

**htmx was skipped in Phase 1** and added in Phase 2, when one-tap settle
actually needed it. It is vendored at `internal/web/static/htmx.min.js` with its
SHA-256 recorded in `static/VENDOR.md` — not fetched at build time, so the CSP
can forbid every external origin.

**UI-04 (design tokens) was implemented in Phase 1** though scheduled for
Phase 2; it was free to do while writing the first stylesheet.

---

## Open items, none blocking

- **License is unset.** README says so. Pick one before publishing.
- **`internal/config` and `internal/store` have no tests.** Both are largely
  declarations; the behavior is covered through the packages that use them.
- **`auth` coverage is 44%**, mostly because `DummyVerify` and some session
  paths are not directly exercised. The security-relevant paths — hashing,
  salting, malformed-hash rejection, fail-closed sessions — are covered.
- **Planning docs live in `docs/planning/`**, not the GSD-conventional
  `.planning/`. If you run GSD tooling that expects the latter, it will not find
  these. Moving them is a `git mv` plus link fixes.
- **The demo runs used `BITT_SECURE_COOKIES=false`** for plain HTTP. Production
  defaults to `true`; the server logs a warning when it is off.
