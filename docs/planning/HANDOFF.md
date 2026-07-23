# BitTabby — Handoff

**Written:** 2026-07-23. Updated at the end of Phase 3.

This is written to be read cold, by someone (or some session) with no memory of
the work. It covers what exists, why it is shaped this way, what to do next, and
the traps.

---

## Getting running

```bash
cd /home/jcz/Github/bitt
go install github.com/a-h/templ/cmd/templ@latest   # only if `templ` is missing
make check                                          # generate, fmt, vet, test
make build && ./bittabby                            # http://localhost:8080
```

No background processes and no scheduler: recurrence posts inside the read
path, so nothing needs to have been running for balances to be right.

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

Phases 1, 2, and 3 are complete and verified. 38 of 54 requirements.

| Commit | What |
|---|---|
| `a49ce5d` | Baseline: the original bit-tabby docs, preserved |
| `a54f5f7` | Scope revision, 79 requirements to 54, reordered |
| `030a84e` | Phase 1 — walking skeleton |
| `ec5655c` | Phase 1 status |
| `9ef145e` | Phase 2 — the settle loop |
| `529f00f` | Phase 2 status and a cold-start handoff |
| (this one) | Phase 3 — recurrence |

Full suite green including `-race`. Coverage: money 96%, ledger 92%,
schedule 87%, web 67%, sqlite 66%, auth 44%.

---

## Architecture, and why

```
cmd/bittabby/          main, config load, graceful shutdown, embedded tzdata
internal/money/        Cents (int64). No float anywhere in the money path
internal/schedule/     pure period arithmetic. No database, no clock, no I/O
internal/store/        persistence CONTRACT (interfaces only, no SQL)
internal/store/sqlite/ the only implementation, plus embedded migrations
internal/ledger/       the ONLY write boundary for financial entries;
                       also accrual (accrual.go) and statements (statement.go)
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

## Phase 3 landed. Here is what it actually did

**`internal/schedule` is pure calendar arithmetic.** No database, no clock of its
own, no instants except at one boundary. Everything is civil dates: a period has
come due when its due *date* has arrived in the instance timezone. Nothing adds a
duration to a timestamp, so a 23- or 25-hour day cannot move a boundary.

**Every period is computed from the anchor, never chained off its predecessor.**
That is what makes a day-31 anchor land on February 28th and come back to March
31st instead of sticking at the 28th forever (SCHED-02).

**Billing timing is the Provider's choice per tab**, in advance or in arrears.
A retainer is owed up front and metered work is owed after the fact, and one
convention could not honestly serve both. The period *key* is always the cycle's
start date, never the posting date, so changing the rule cannot re-bill a cycle.

**Accrual runs in the read path**, in `Server.accrueTab`, called from the
dashboard and from the tab page. That is the entire scheduler. If a read path is
ever added that does not call it, tabs reached only that way silently stop
billing.

**`posted_periods` is the whole concurrency story.** The claim and the entry
share one transaction, and the claim carries a `(tab_id, period_key)` primary
key. A reader that loses the race has its entry rolled back with the claim.
Nothing reads-then-writes, so there is no window to lose. Verified live: twenty
simultaneous reads of an overdue tab produced exactly one charge per cycle.

**Item changes supersede rather than overwrite.** `UpdateItem` marks the old row
removed and inserts a replacement at the same position. Catch-up then bills each
cycle for the items the tab carried *at that cycle's due date*, via
`ledger.ItemsAsOf` — so a tab left alone for six months does not bill six months
at today's prices. Cycles that came due before the tab existed clamp to the
tab's creation time, otherwise a backdated anchor posts a run of zeroes.

**Two gotchas worth keeping.** `time.Date` resolves a non-existent local midnight
*backwards*: asking for 2026-09-06 00:00 in America/Santiago returns 2026-09-05
23:00, the previous calendar day. `Date.Time` walks forward to fix that; do not
remove the loop. And the binary embeds `time/tzdata`, because a static binary in
a scratch container has no zoneinfo to read and every period boundary depends on
`time.LoadLocation`.

## Authorization, which changed in Phase 3

Read this before touching any tab route.

There are now **two ways to reach a tab**, and they do not grant the same
things. `tabAccess` in `internal/web/handlers_tabs.go` is the whole model:

- **Participation** is the ordinary way, and is what AUTH-05 describes.
- **The global administrator role** is a deliberate second way, added so a tab
  whose Provider has left the household can be renamed, archived, or repaired
  without someone editing the database by hand.

`CanManage()` covers a tab's own settings -- name, kind, schedule, items,
people. Its Provider may, and so may an administrator, on any tab.

`CanTransact()` covers moving money -- charges, payments, undo -- and is
**membership only**. An administrator looking after the instance is not a party
to what two other people owe each other. This is not a nicety: during
development, `authorizeTab` was widened for administrators and the payment
handlers kept calling it without a second check, so an administrator could post
a payment to a stranger's tab. A test caught it. Do not let a new money-moving
route call `authorizeTab` and stop there.

Every administrative access to a non-member tab is logged at WARN. That log line
is what makes the exception auditable rather than invisible, and REQUIREMENTS.md
carries the amendment to AUTH-05 in its own words.

For everyone else nothing changed, including the 404-not-403 response that keeps
tab ids unenumerable.

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

**A charge can appear without anyone posting one.** Reading a tab bills it, so a
balance moves on load. The page says so rather than letting the number move
silently; that note is not decoration.

**Deactivating the last admin is guarded inside the write transaction**, not in
the handler. A check-then-act in the handler lets two concurrent requests both
see a spare admin and both proceed, locking everyone out of the instance. There
is a concurrency test for this. Do not move the check up a layer.

**Foreign tabs answer 404, not 403**, so tab ids cannot be enumerated. Same for
admin routes to non-admins. This is deliberate; do not "fix" it to 403. A
participant who merely lacks the Provider role gets a message instead, because
they can already see the tab and a 404 would tell them nothing true.

**Archiving a tab must stop it accruing.** The guard is in `Server.accrueTab`.
Without it, archiving would only hide a tab from the dashboard while it quietly
kept billing.

**`errors.Join(errBadInput, ...)` put the sentinel's own text on the screen.**
User-facing validation messages now use the `badInput` string type, which
matches `errBadInput` under `errors.Is` while its `Error()` stays clean.

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

**TAB-02 (Payoff tabs) came forward from Phase 4** in Phase 3. The schema had
permitted `payoff` since `0001_initial`; only the UI was withholding it, and a
tab that could not state what kind it was made the whole page harder to read.
Phase 4 still owns PAYOFF-01 to 03 -- progress against the original total,
on-track status, and auto-settling at zero.

**Settling for a partial or larger amount** and **editing a tab's name,
description, and kind** were not in Phase 3's requirement list. Both came from
using the thing. The ledger had always accepted any positive payment; only the
dashboard's hidden amount field was forcing the whole balance.

---

## Phase 4 landed. Payoff, fees, interest

**Payoff is Model A.** Principal charged once, payments draw it down, the schedule
is expected *payment* dates (not charges). A Payoff tab posts NO period charges —
`Accrue` branches on `tab.Kind == TabPayoff`. Progress and status come from
`ledger.ComputePayoff`, which splits payments into interest-then-principal so
progress reflects principal retired.

**Fees and interest reuse the accrual pattern exactly.** Each has a claim table
(`posted_fees`, `posted_interest`) with a `(tab, period)` PK and append-only
triggers, and a `PostFeeEntry` / `PostInterestEntry` that writes the entry and the
claim in one transaction — same exactly-once-under-concurrency guarantee as
`PostPeriodEntry`. If you add another accrual type, copy this shape; do not invent
a new one.

**Fees are per-period-windowed, not cumulative.** Each installment is judged on
payments in its own window `(prev deadline, this deadline]`, so a paid period is
never dragged down by an earlier miss ("pay May, no May fee even while behind").
This was a real correction mid-build — the cumulative model fined every month
after the first miss. See `paidWindow` in `fees.go`.

**Interest is a charge sub-typed by a `category` column, NOT a new entry kind.**
The `entries.kind` CHECK from 0001 cannot gain a value without rebuilding the
table, and the migration harness holds `foreign_keys` on inside its transaction
(a no-op there), so the standard SQLite rebuild recipe is unavailable. So
interest posts `kind='charge', category='interest'`. Principal is derived as
non-interest charges. If you ever need a genuinely new kind, this constraint is
why it is hard — budget for a harness change.

**Interest accrues on the loan balance, excluding fees, and compounds on unpaid
interest.** `loanBalanceThrough` is deliberately fees-excluded. Compounding is
correct for a loan and is exactly what a fee must never do — the two are
different on purpose.

**Version lives in `internal/version`** (a constant `Number`, ldflags inject
commit/date). Shown in the footer and healthz. Bump `Number` on every functional
change per semver; add a CHANGELOG entry. The Makefile injects provenance only.

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
