# BitTabby

## What This Is

BitTabby is a self-hosted, mobile-first web app for tracking money owed between people who trust each other — framed as running "tabs" rather than formal invoices. Providers bill, Payees pay, and the app maintains an accurate running balance for each tab. It never processes payments itself; it records payments made elsewhere (cash, Venmo, bank transfer) so both sides share one authoritative record.

It is built for personal, self-hosted home use: family phone plans, shared insurance, recurring services, a loan being paid off between relatives. Slogan: *"Tabs, not invoices."*

## Core Value

**The running balance is always correct.** If recurrence, statements, and polish all fail, a user must still be able to open BitTabby and trust the number it shows them.

## Scope Posture

This document is a deliberate reduction. A prior implementation ([bit-tabby](https://github.com/johnzastrow/bit-tabby)) scoped v1 at 79 requirements across 7 phases and stalled: three phases went into ledger core, OIDC-capable auth, and three database backends, and it never reached a screen where a person could look at a tab. The failure was sequencing, not engineering — the heaviest subsystems all sat *behind* the point where the product becomes usable.

The rules governing this rewrite:

1. **A usable application exists after Phase 1.** Every phase after that adds to something a person can already open and use.
2. **No subsystem lands before the product it serves.** Offline sync, invoice lifecycles, and late-fee automation are all deferred until real use demonstrates the need.
3. **Nothing infrastructural is built twice.** SQLite first, MariaDB after the product works, behind an interface designed for both from the start.

v1 is 54 requirements across 5 phases — down from 79, with the ordering inverted so that value lands first.

## Requirements

Full requirement text with stable REQ-IDs lives in [REQUIREMENTS.md](REQUIREMENTS.md). Summarized by area:

**Ledger** — Append-only entries, never edited or deleted; corrections are reversing entries. Balances derived by summation, never cached in a mutable column. Integer USD cents throughout, no floating point in the money path. Server-assigned sequence numbers define ordering. Idempotency keys make a repeated submit incapable of double-posting.

**Tabs and items** — Two tab types: Services (recurring, no end) and Payoff (fixed total drawn down by payments). A tab carries line items with amounts that sum to its periodic charge, so both parties can see what they are paying for and which line moved when a cost shifts. Items carry no balance of their own; payments settle against the tab as a whole.

**Schedules and charges** — An anchor date plus weekly, every-two-weeks, monthly-on-a-day, or monthly-on-last-day. Every period carries a due date derived from the schedule. Due periods post lazily inside the read transaction rather than on a background timer. Each posted period snapshots its item breakdown. A period statement is a view over ledger entries, not a stored record.

**Late fees** — Configured per tab as a fixed amount or a percentage of the overdue period charge, with a grace period. Once grace elapses on an unpaid period, the fee posts automatically as a ledger entry through the same lazy path as charges. Total accrued fees are capped, and a Provider can waive one as a reversing entry with a reason.

**Payments** — One tap to settle from the dashboard with fields prefilled. Payment method captured, since money moves outside the app. Providers may record on a Payee's behalf. Undo is a reversing entry. Paying ahead creates a credit that offsets future charges.

**Accounts** — Email and password with Argon2id. Secure session cookies. A one-time first-run setup screen creates the first admin and then locks. Every request authorized against the specific tab it touches.

**Interface** — Mobile-first, fully usable on desktop. Dashboard of tab cards showing balance and status at a glance. Chalk & Pastel palette expressed as design tokens. Installs to a phone home screen as a PWA shell.

**Deployment** — Single static binary with everything embedded. SQLite first, MariaDB second, behind one repository interface. Docker image published via GitHub, running as non-root.

### Out of Scope

**Deferred to a later milestone** — valuable, but not required to prove the core value:

- **Offline data entry and sync** — the PWA shell installs in v1, but recording a payment requires a connection. The write path is built idempotent and API-shaped so this stays additive rather than becoming a retrofit
- **Invoice as a stored entity** — v1 renders period statements from ledger entries. The original design built invoices so that late fees would have a due date to work from; schedules supply that directly, which removes the reason
- **Notifications** — email, ntfy, per-event preferences, reminders. Deferred entirely; for a household instance, both parties can see the tab
- **PostgreSQL** — SQLite and MariaDB only. The repository interface does not preclude it
- **OIDC** — built-in auth is sufficient for a home deployment
- **Theme switching** — one palette, expressed as tokens, so alternates remain cheap to add
- **Reporting and exports**, **ad-hoc search**, **in-app messaging**, **receipt attachments**, **external integrations**

**Explicitly excluded** — not planned:

- **Processing or holding funds** — BitTabby records payments made in other systems. Adding processing changes the regulatory posture entirely
- **Multi-tenant SaaS hosting** — each deployment serves one household. Tenant isolation is a different product
- **Multi-currency and FX** — USD only. Every use case is domestic and recurring
- **Per-item balances and payment allocation** — items break down what a charge covers; they do not accrue or settle independently. This removes an entire class of allocation logic
- **Balance snapshot caching** — a cache is the single most likely way "the balance is always correct" fails. At household scale, deriving balances per read is fast enough. If ever needed it must be an additive, provably-rebuildable projection, never a second source of truth
- **Two-party payment confirmation** — Splitwise built this and removed it after users found it too slow, which is precisely the friction this project is designed against. The event trail and undo-as-reversal serve the same purpose
- **Automatic proration on cost change** — changes take effect the following period by design
- **Open public registration** — self-hosted instances provision users deliberately

## Context

**Prior work.** A comparable architecture was built in [actalog](https://github.com/johnzastrow/actalog) and again in [bit-tabby](https://github.com/johnzastrow/bit-tabby). Three shortcomings drive this rebuild:

1. **Dated, unpolished visuals** — motivating the modern, token-driven UI
2. **Too many clicks per action** — motivating "settle in one tap, controls prefilled," which shapes the entire interaction model
3. **Infrastructure ahead of product** — bit-tabby's actual failure, and the reason this document is a reduction rather than a copy

These are the failure modes to design against from Phase 1, not to retrofit later.

**Why lazy accrual.** Posting due periods inside the transaction that reads a tab removes the background scheduler entirely — no timer process, no downtime catch-up logic, no DST tick handling. Catch-up becomes inherent: whatever periods are due get posted the moment anyone looks. The tradeoff is that nothing accrues while nobody is looking, which is harmless when the balance is correct the instant it is observed. A unique constraint on (tab, period) makes concurrent reads incapable of double-posting.

**Why append-only still matters without offline.** Offline sync was the original justification for an append-only ledger, and offline is now deferred. The design survives on its own merits: balances stay reconstructible from primary records, corrections leave an honest trail, and undo is a reversal rather than a destructive edit. It also keeps offline genuinely additive if it is ever built.

**Design language.** Minimalist and grid-driven — closer to a modern interactive spreadsheet than an accounting package. Friendly and collaborative in tone, aimed at people tracking what they are owed without sending formal paperwork.

## Constraints

- **Security** — real financial records and real user accounts. Production-quality: parameterized queries throughout, Argon2id password hashing, authorization checked against the specific tab on every request, append-only financial records, secrets from environment or file. This is not a scratch project
- **Deployment** — a single self-contained binary, published as a Docker image through GitHub, running as non-root
- **Data backend** — SQLite first, then MariaDB. All SQL sits behind a repository interface with no dependence on SQLite-only behavior, since the second backend is committed rather than hypothetical
- **Money handling** — integer USD cents only. No floating-point arithmetic anywhere in the accounting path, including serialization and display formatting
- **Platform** — mobile-first responsive web, installable as a PWA shell. Writes must be idempotent and reachable as JSON so offline capability stays additive
- **Scale** — one household per deployment. Design decisions may assume small data volumes and trusted participants

## Key Decisions

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| Reduce v1 from 79 requirements to 54 across 5 phases | bit-tabby's v1 was not too hard, it was ordered wrong — infrastructure landed before anything usable | Resolved |
| Light planning ceremony; long build stretches between check-ins | Process weight was part of what kept the predecessor from reaching working software. Design decisions get discussed; routine writes and tests do not | Resolved |
| A usable application must exist after Phase 1 | The single rule that would have prevented the prior stall | Resolved |
| Go with templ and htmx, server-rendered | Offline writes are deferred, which removes the client-database rationale for an SPA. One language, one static binary, no API contract to keep in sync, no build pipeline. One-tap settle and mobile-first are entirely achievable server-rendered | Resolved |
| SQLite first, MariaDB in a later phase | Both backends are committed, but paying for two dialects during the phases that reach a product is what stalled the predecessor. The interface is designed for both from the start | Resolved |
| PostgreSQL dropped | Two backends cover self-hosted home deployment; the interface does not preclude adding it | Resolved |
| USD only | Every use case is domestic and recurring. Removes per-tab currency, currency-carrying amount types, and cross-currency arithmetic guards | Resolved |
| Append-only ledger, corrections as reversing entries | Balances stay reconstructible from primary records and corrections leave an honest trail | Resolved |
| Balances always derived, never cached | A cache is the primary way "the balance is always correct" fails | Resolved |
| Integer USD cents everywhere | Eliminates float rounding drift across the whole money path | Resolved |
| Lazy accrual instead of a background scheduler | Removes the scheduler process, downtime catch-up, and DST tick handling. Catch-up becomes inherent; a unique constraint on (tab, period) prevents double-posting | Resolved |
| Both tab types in v1: Services and Payoff | Recurring services and a loan being paid off are both real household cases, and Payoff is the type where settling has a natural end | Resolved |
| Payoff tabs carry an expected payment schedule | Without it a loan payoff cannot show whether the Payee is on track, which is most of what makes it feel like a loan — and the expected schedule is what gives late fees a due date | Resolved |
| Late fees in v1, driven by the expected schedule | The lazy accrual path with its (tab, period) unique constraint is already the exact mechanism a fee assessment needs, only with a different trigger. What made fees expensive in the predecessor was the scheduler and invoice lifecycle underneath them, both of which are gone | Resolved |
| Due dates come from the schedule, not from invoice records | The original design built invoice entities so late fees would have a due date to work against. Deriving the due date from the period removes that dependency entirely | Resolved |
| Percentage fees computed on the overdue period charge, not the running balance | Prevents fees compounding on top of previously assessed fees, which is the failure mode that makes a household late-fee feature feel punitive and hard to explain | Resolved |
| Flexible periods: weekly, biweekly, monthly-on-day, monthly-on-last-day | Real household bills use all four. Lazy accrual makes the generality cheap since there is no scheduler tick to reason about | Resolved |
| Items carry amounts that sum to the charge | Both parties need to see what they are paying for and which line moved when a cost shifts. Amounts on items deliver that without per-item balances or allocation rules | Resolved |
| Period statement is a view, not a stored entity | Delivers the statement experience with no new table, no status lifecycle, and no possibility of drift between statement and ledger | Resolved |
| Cost changes take effect the following period | Preserves ledger immutability without proration date math | Resolved |
| Credits auto-apply to future charges | Matches the natural mental model of being paid ahead | Resolved |
| Idempotency keys retained despite offline being cut | About ten lines and a unique constraint, standing between a double-tapped settle button and a wrong balance | Resolved |
| Writes built API-shaped and idempotent from Phase 1 | The only part of offline capability that is expensive to retrofit. Cheap now, and it keeps the outbox genuinely additive | Resolved |
| PWA shell in v1, offline writes deferred | Installable and app-like for near nothing; the outbox and replay machinery is what stalled the predecessor | Resolved |
| Instance-wide timezone | One household lives in one timezone. Removes timezone plumbing through the accrual path and most period-boundary edge cases | Resolved |
| Minimal admin; admins may also be Providers | Household instances provision people deliberately and rarely | Resolved |
| First-run one-time admin setup screen | Table stakes for self-hosted apps, friendliest for non-technical deployers, and locks permanently after use | Resolved |
| Backup and restore documented rather than built | A documented procedure is the right deliverable at household scale | Resolved |
| Notifications, OIDC, and invoice entities deferred | Each is a subsystem landing behind the point where the product is usable, and none is required by the core value | Resolved |

## Evolution

This document evolves at phase transitions and milestone boundaries.

**After each phase transition:**
1. Requirements invalidated? Move to Out of Scope with a reason
2. Requirements validated? Move to Validated with a phase reference
3. New requirements emerged? Add to Active — and check them against the Scope Posture rules above before accepting
4. Decisions to log? Add to Key Decisions
5. "What This Is" still accurate? Update if it has drifted

**After each milestone:**
1. Full review of all sections
2. Core Value check — still the right priority?
3. Audit Out of Scope — reasons still valid?
4. Update Context with current state

---
*Revised: 2026-07-23. Supersedes the bit-tabby-derived scope; the pre-revision version is in git history.*
