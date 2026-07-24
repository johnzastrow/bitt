# BitTabby — Handoff

**Written:** 2026-07-23. Updated through 0.6.3 and the Phase 5 pre-implementation
security review (2026-07-24).

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
[../DATA-MODEL.md](../DATA-MODEL.md) is the physical schema with an ER diagram,
a per-column data dictionary, the invariants, and the loan arithmetic rules.

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

Phases 1 through 4 are complete and verified, and a run of self-contained 0.5
and 0.6 improvements has landed on top. **Phase 5 (notifications) has not
started** — it is gated behind a security design review (see the end of this
doc). The roadmap is **six phases**: Phase 5 is notifications, Phase 6 is ship.

| Commit / version | What |
|---|---|
| `030a84e` | Phase 1 — walking skeleton |
| `9ef145e` | Phase 2 — the settle loop |
| `8a6d25e` | Phase 3 — recurrence |
| `8d3e989` | Phase 4 — payoff tabs, late fees, interest; versioning; logo |
| `de8bc11` (0.5.0) | Loan terms, scheduled payments, intervals, U.S. Rule interest, adjustments |
| `7aff25a` (0.5.1) | Timezone autocomplete, kind-scoped tab fields, `--version` arg handling |
| `927e66d` (0.5.2) | Create-form layout fix; content-hashed asset URLs |
| `227ab27` (0.6.0) | Account profiles — name, email, password, avatar |
| `b20d5b4` (0.6.1) | Settle buttons pay a period, not the whole balance |
| `c4ae6ab` (0.6.2) | Avatars wherever a person is named |
| `2917713` (0.6.3) | Avatars in the ledger history |

Full suite green including `-race`. Coverage: version 100, tz 96, fee 96, money
90, ledger 89, loan 89, avatar 88, schedule 87, web 72, sqlite 67, auth 42.
Zero-coverage packages: `config` and `store` (declarations, exercised through
their users) and `web/views` (templates). Schema is at migration `0007`.

**Running now:** v0.6.3 on `:8080` over plain HTTP against `data/bitt.db`, which
now holds the user's real accounts. To restart from cold:
`make build && BITT_SECURE_COOKIES=false ./bittabby`. Do NOT point a test run at
that database or that port — spin a throwaway on another port with its own
`BITT_DB_PATH` (see "Testing the UI" below).

### The 0.5 / 0.6 series in brief

Each of these has its own CHANGELOG entry with the reasoning; the traps worth
carrying into new work:

- **Loan arithmetic (0.5.0)** — interest is the **U.S. Rule**, on outstanding
  principal only, and does NOT compound on unpaid interest. `ledger.AllocateLoan`
  is the single source of truth for the payment/credit split. A **credit is
  allocated principal-first**, deliberately unlike a payment. The `internal/loan`
  package is pinned against a real lender's quoted schedule to two cents. See the
  "0.5.0 landed" section below.
- **Adjustments (0.5.0)** — `POST /tabs/{id}/adjustments`, Provider-only, is how
  a balance is corrected without faking a payment. `ledger.Adjustment` had
  existed unused since Phase 1.
- **Asset caching (0.5.2)** — `/static/*` URLs carry a content digest
  (`app.css?v=…` from `web.AssetVersion()`), because a stable URL with a long
  `max-age` hid a shipped CSS change for an hour. Any new asset reference must go
  through `Page.asset()` / `AssetURL`, never a bare `/static/` path.
- **Profiles & avatars (0.6.0–0.6.3)** — `internal/avatar` is the whole upload
  surface and the ONLY file upload in the app; study it before adding another
  (magic bytes, header-dimension check before decode, decode-and-re-encode,
  per-user rate limiter). Avatars render from the `Avatar(id, name, timestamp)`
  templ component everywhere a person is named.
- **`--version` / `--help` (0.5.1)** — the binary used to ignore argv and start a
  server for any flag, which once migrated a live database. `runArgs` now refuses
  unknown arguments. Configuration is still env-only; there are no real flags.
- **Timezone picker (0.5.1)** — `internal/tz` holds the embedded IANA list;
  default seed is `America/New_York`, UTC is the load-failure fallback.

### Testing the UI (standing preference)

Use **Playwright**, never the Chrome extension, and never against `:8080` or the
real database. Pattern: `BITT_DB_PATH=<scratch>/x.db BITT_ADDR=:8099
BITT_SECURE_COOKIES=false ./bittabby &`, complete setup over curl or Playwright,
then drive headless Chromium and assert on `is_visible()` / `bounding_box()`
plus a screenshot. The `webapp-testing` skill wraps this.

### About `data/bitt.db`

The original walkthrough database was deleted once 0.5.0 landed, and the user
then set up fresh **real accounts** on the running `:8080` instance. So
`data/bitt.db` is live user data now — do not test against it, and do not
migrate it casually (migrations are forward-only). Two traps from the old
database are still worth carrying forward, because both are about behaviour, not
that specific data:

- It held two Payoff tabs whose loan amount had been typed into a *line item*
  instead of the loan amount, so neither had a principal charge and both read as
  paid off. **0.5.0 removes the trap**: a Payoff tab's expected payment is now
  its own field and line items belong to Services tabs only.
- Migration 0006 backfills `loan_payment_cents` from the sum of a Payoff tab's
  active line items, which is exactly what the old code read. That is faithful
  on upgrade, but a tab mis-configured the old way will arrive carrying its
  **loan amount as its per-period payment**. If someone upgrades a real database
  and a loan shows an implausible payment, that is why. Fix it in
  Setup → Loan term and payment; never edit the ledger directly, it is
  append-only and theirs.

---

## Architecture, and why

```
cmd/bittabby/          main, config load, graceful shutdown, embedded tzdata
internal/version/      the version constant; ldflags inject commit/date
internal/money/        Cents (int64). No float anywhere in the money path
internal/schedule/     pure period arithmetic. No database, no clock, no I/O
internal/fee/          pure late-fee sizing (fixed/percent, cap, rounding)
internal/loan/         pure amortization: suggested payment, projection, drift
internal/store/        persistence CONTRACT (interfaces only, no SQL)
internal/store/sqlite/ the only implementation, plus embedded migrations
internal/ledger/       the ONLY write boundary for financial entries. accrual.go
                       (period charges), fees.go, interest.go, payoff.go,
                       statement.go -- all accrual runs lazily on read
internal/auth/         Argon2id, sessions, CSRF
internal/web/          routes, handlers, templ views, embedded static assets
```

The four pure packages (`money`, `schedule`, `fee`, `loan`) have no I/O and are tested
exhaustively on their own — that is where the awkward arithmetic (DST, month-end,
rounding, caps) is pinned before any wiring. Every accrual type — period charges,
late fees, interest — follows one pattern: a claim table with a `(tab, period)`
primary key and append-only triggers, written in the same transaction as its
entry, so it happens exactly once even under concurrent reads. Copy that pattern
for any new accrual; do not invent a second one.

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

> **Superseded in 0.5.0.** The compounding half of that paragraph is no longer
> true, and `loanBalanceThrough` is gone. Interest now accrues on outstanding
> **principal** only; interest a short payment did not cover sits in its own
> bucket where it is owed, is repaid before principal, and never accrues. That
> is the U.S. Rule, which Regulation Z permits for closed-end credit and which
> consumer lenders run. `ledger.AllocateLoan` is the single source of truth for
> the split and is shared by interest accrual, payoff progress, and fees.
>
> The old reasoning — "a loan compounds" — is true of a bond or a revolving line
> and false of a US consumer installment loan. The practical consequence is what
> mattered: under the old rule a borrower who missed a payment and caught up
> separated permanently from their bank's figure, with no way back. The rule was
> documented and never tested, which is why inverting it broke no test. It is
> now pinned in both `internal/loan` and `internal/ledger`.

**Version lives in `internal/version`** (a constant `Number`, ldflags inject
commit/date). Shown in the footer and healthz. Bump `Number` on every functional
change per semver; add a CHANGELOG entry. The Makefile injects provenance only.

---

## The tab page layout (as of the UI reorg, `4da3c2d`)

`views.TabDetail` is organized top to bottom as: **orientation** (always visible —
name, kind, balance, schedule/fee summary, loan progress, upcoming notice), then
two `<details>` groups — **Day to day** (open: payment, charge, read-only items,
periods, people, history) and **Setup & configuration** (collapsed, provider/
admin only: tab details, schedule, late fee, interest, line-item editing). The
groups are native `<details>` (no JS, CSP-safe); styles are under
"Setup / Operational collapsible groups" in `app.css`. A payee sees only
orientation + Day-to-day.

Every transaction entry point takes a **note**: the full payment form, the
dashboard settle card, one-off charges (memo), and fee waivers (reason).

---

## Phase 5 is next: notifications

Not in the original 54; added when the user asked for payment reminders. The
design is recorded in ROADMAP.md and is deliberately constrained:

- **External cron drives it**, hitting an authenticated `/internal/tick`
  endpoint. No timer inside the binary — a background scheduler is the
  Out-of-Scope item that stalled the predecessor, and the ledger stays lazy.
- **Idempotent via a sent-notifications claim table**, the same pattern as
  `posted_periods` / `posted_fees` / `posted_interest`: a re-run of the same hour
  sends nothing twice, a missed hour sends late rather than never.
- Email and ntfy delivery; per-user, per-event preferences; secrets via env/file.
- Deliver: a payment request two weeks before a due date, reminders one week and
  one day before (configurable), and notices on a payment made and a payment
  missed, to all parties on a tab.

**The load-bearing rule:** this is the first outbound side effect in the app. It
must stay entirely off the balance path — a failed or double send can never
touch a ledger. The in-app half already ships (the two-week upcoming-payment
notice, `Server.upcoming`); Phase 5 is only the pushed-delivery half.

### Do the security design first — it is gated on it

Phase 5 is the app's first outbound side effect and its first reach to external
hosts, so it changes the threat model rather than extending it. **A security
design review was run before any code** (multi-agent threat model + adversarial
verification); its output lives in **[../SECURITY-PHASE5.md](../SECURITY-PHASE5.md)** and must be read
before writing the phase. The threats that review exists to pin down, so they
are not forgotten if the doc drifts:

- **SSRF via ntfy delivery.** ntfy sends to a configurable server/topic URL. A
  user-controlled URL is a straight path to internal metadata endpoints and
  localhost services. The design needs an explicit allowlist policy, decided up
  front.
- **Email injection.** Notification content interpolates tab names, memos, and
  display names — all user-controlled. CRLF into headers and HTML/links into the
  body are the classic vectors, and the CSP does not protect email.
- **`/internal/tick` auth.** It must be exempt from `requireAuth` (an external
  cron has no session), so it needs its own authentication — a shared secret,
  constant-time compared, failing CLOSED when unset. And it must not become a
  balance-path write: if computing "what is due" runs accrual, an outbound-
  triggered endpoint would post ledger entries. Keep accrual and the send apart.
- **Send-once ordering.** Claim-then-send loses a notice on a delivery failure;
  send-then-claim double-sends on a crash between the two. Pick the guarantee
  deliberately, and keep the claim out of any ledger transaction so a send
  failure can never roll back a financial entry.
- **Secrets.** SMTP/ntfy credentials must never reach a log, an error message, a
  rendered page, or a notification body. Verify the existing "nothing sensitive
  is logged" claim still holds for new delivery code.

After Phase 5, **Phase 6 — ship**: MariaDB as a second backend (DEPLOY-02 kept
the repository interface clean for exactly this), Docker as non-root, PWA shell,
backup/restore. 5 requirements: UI-05, DEPLOY-03/05/06/07.

---

## Open items, none blocking

- **License is unset.** README says so. Pick one before publishing.
- **`internal/config`, `internal/store`, and `web/views` have ~no tests.** The
  first two are largely declarations, covered through their users; views are
  templates covered through `web`. The one behavioural view helper with a test is
  `initials` (`web/views/initials_test.go`).
- **A recurring footgun for the assistant:** `./bittabby -someflag` no longer
  starts a server (0.5.1 fixed that), but pointing any run at `data/bitt.db`
  still migrates it forward-only. Always use a scratch DB path for tests.
- **`auth` coverage is 44%**, mostly because `DummyVerify` and some session
  paths are not directly exercised. The security-relevant paths — hashing,
  salting, malformed-hash rejection, fail-closed sessions — are covered.
- **Planning docs live in `docs/planning/`**, not the GSD-conventional
  `.planning/`. If you run GSD tooling that expects the latter, it will not find
  these. Moving them is a `git mv` plus link fixes.
- **The demo runs used `BITT_SECURE_COOKIES=false`** for plain HTTP. Production
  defaults to `true`; the server logs a warning when it is off.

---

## 0.5.0 landed: loan terms, scheduled payments, intervals

Added after Phase 4, at the user's request, when it became clear a Payoff tab
could not say how long it ran or what it cost per period.

**The lender's number wins, always.** The Provider enters the payment their bank
charges; the app computes what the terms imply and shows both. It never
overwrites the entered figure. This is not politeness -- many auto lenders accrue
per diem on actual/365, where the split depends on the days between deposits, and
no period-based model can match that to the cent. Showing the disagreement is
honest; resolving it silently would not be.

**The suggestion is simulated, not solved.** `loan.SuggestPayment` binary-searches
the payment and runs each candidate through the same arithmetic the ledger posts
-- same per-period rounding, same U.S. Rule allocation. The closed-form annuity
formula needs a float (forbidden in the money path) and disagrees with integer
per-period rounding by a few cents over a long term. The simulation cannot,
because it is the same arithmetic. Do not "simplify" this to the formula.

**It is pinned against an issued schedule, not a formula.** `TestAgainstAQuotedSchedule`
carries a lender's own amortization schedule: $21,852.48 at 5.24% over 48 monthly
payments, quoted at $505.65, computed here at $505.63. The two-cent tolerance is
deliberately tight -- widen it and the test stops being able to detect a
convention change. A day-count disagreement would show up as dollars over 48
months, not cents.

**The rate basis is a fraction of a year, not a period count.** `Schedule.RateBasis`
returns months/12 for month-stepping schedules (30/360, so plain monthly is
exactly the APR/12 a borrower can check by hand) and days/365 for week-stepping
ones (actual/365, because there is no whole number of weeks in a year). This is
what makes "every three weeks" expressible at all: 21/365, where 52/3 is not an
integer. Weekly and biweekly tabs carrying interest see a slightly different
figure on future periods than they did in 0.4.0.

**`biweekly` is gone as a kind**, rewritten to `weekly` with interval 2. Both
compute period n as anchor + 14n, so the rewrite is date-identical and no period
key moves -- that equivalence is what makes migration 0006 safe, and
`TestNormalizeRewritesBiweekly` pins it. The constant is retained as deprecated
and `Normalize` still rewrites it, so a row that escapes the migration behaves.

**Two shipped bugs fell out of adding the term.** Both payoff progress and the
late-fee expectations capped the expected cumulative at the *principal*, which on
an interest-bearing loan is less than the loan costs -- so the schedule stopped
expecting payments partway through, a borrower paying exactly on time could read
as behind, and the last months of a term went unfined.

**The true-up recasts on payments made, not calendar periods elapsed.**
`Payoff.RemainingPeriods` counts scheduled payments not yet made. The calendar
answer is wrong in the ordinary case: a tab created today has one period already
due and nothing paid, and dividing the whole principal over the remaining
calendar periods reports every correctly-configured new loan as underfunded.
This was caught by a test during the build; do not "fix" it back.

**Drift under five cents is not reported.** That is a lender rounding its annuity
result up. Flag it and the Provider learns to ignore the notice.
