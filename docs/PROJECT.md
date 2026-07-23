# BitTabby

## What This Is

BitTabby is a self-hostable, mobile-first Progressive Web App for tracking money owed between trusted parties — framed as running "tabs" rather than formal invoices. Providers bill, Payees pay, and the app maintains an accurate running balance for each tab. It never processes payments itself; it logs payments made elsewhere (cash, Venmo, bank transfer) so both sides share one authoritative record.

It is built for people running ongoing shared costs with people they trust — family phone plans, shared insurance, recurring services — who want the numbers tracked without sending intimidating paperwork. Slogan: *"Tabs, not invoices."*

## Core Value

**The running balance is always correct.** If reporting, notifications, and polish all fail, a user must still be able to open BitTabby and trust the number it shows them.

## Requirements

### Validated

(None yet — ship to validate)

### Active

**Ledger & accounting**

- [ ] Balances are computed from an append-only ledger; entries are never edited, only reversed
- [ ] All monetary values stored as integer minor units (cents), never floating point
- [ ] Each bucket declares its own currency; balances never cross currencies
- [ ] A full event log records who did what, when, across all major entities
- [ ] Services buckets accrue automatically via scheduled ledger entries on a recurring period
- [ ] Declining Balance buckets draw down against a fixed total
- [ ] Bucket balances may be positive (credit) or negative (owed)
- [ ] Credit balances auto-apply against future accruals until exhausted
- [ ] Provider can change a recurring rate with a recorded reason; effective next period
- [ ] Provider can post an optional adjustment entry to correct the current period

**Buckets & items**

- [ ] Provider can create a bucket of either type (Services or Declining Balance)
- [ ] A bucket contains one or more Items as a descriptive breakdown
- [ ] Accounting occurs at the bucket level; Items carry no independent balance
- [ ] Provider attaches a Payee to a bucket directly; the bucket appears on the Payee's dashboard

**Invoicing & settlement**

- [ ] Services buckets auto-generate an invoice each accrual period with a due date
- [ ] Provider can adjust a generated invoice before it is issued
- [ ] Payee can mark an invoice paid in one tap from the dashboard, amount and date prefilled
- [ ] Provider can record a payment on a Payee's behalf
- [ ] Declining Balance buckets support a "Settle Up" action; Services buckets show a running balance and payment log instead

**Late fees**

- [ ] Late fees configurable per bucket (amount/rate, grace period)
- [ ] Overdue invoices accrue late fees automatically as ledger entries

**Accounts & access**

- [ ] Built-in email/password authentication with secure session management
- [ ] Optional OIDC/OAuth so self-hosters can delegate to an existing identity provider
- [ ] Administrative user manages global settings and user accounts
- [ ] Users may act as Provider on some buckets and Payee on others

**Platform**

- [ ] Installable PWA, mobile-first, fully usable on desktop
- [ ] Full offline capability: log payments offline, sync on reconnect
- [ ] Offline sync is idempotent — a replayed entry never double-posts
- [ ] Themeable UI with Chalk & Pastel as the default theme
- [ ] SQLite as the v1 data backend, with the data layer designed for MariaDB and PostgreSQL to follow
- [ ] Deployed as a Docker image published via GitHub

### Out of Scope

**Deferred to a later milestone** (valuable, but not required to prove the core value):

- Full notification system (email, per-event preferences, templates, due/overdue reminders) — a minimal ntfy payment alert is in v1 as the safety net for one-sided recording; the rest defers
- Invitations and terms acceptance — v1 uses direct attachment; the invite/accept flow is additive
- Reporting, aggregations, and forecasts — four export pipelines (PDF/XLSX/Markdown/CSV) is a large surface
- Ad-hoc search across entities — depends on a settled data model
- In-app private messaging — orthogonal to the accounting core
- External integrations (Invoice Ninja, payment portals) — the app deliberately does not move money

**Explicitly excluded** (not planned):

- Processing or holding funds — BitTabby records payments made in other systems; it is not a payment processor
- Multi-tenant SaaS hosting — each deployment serves one trusted group; no tenant isolation layer
- Multi-currency conversion — buckets are single-currency; no FX rates, no cross-currency math
- Per-item balances and payment allocation rules — accounting is bucket-level by design

## Context

**Prior work.** A comparable architecture was built and proven in [actalog](https://github.com/johnzastrow/actalog). Two specific shortcomings drive this rebuild:

1. **Dated, unpolished visuals** — motivating the extensive theme exploration and the requirement for a modern, themeable UI
2. **Too many clicks per action** — motivating the "settle in one tap, controls pre-populated" requirement that shapes the entire interaction model

These are the two failure modes to design against from the first phase, not to retrofit later.

**Architectural insight.** The append-only ledger decision de-risks offline sync considerably: offline entries are appends rather than edits, so reconciliation is a merge, not a conflict resolution. The remaining hard problems are idempotency on replay and clock skew on entry timestamps.

**Scope posture.** v1 was initially scoped to the settle loop alone, then deliberately expanded to include scheduled accrual, late-fee automation, and offline sync. This was an informed choice: the roadmap should sequence phases so a usable application exists early, with the heavier subsystems layered on after.

**Design language.** Minimalist and grid-driven — closer to a modern interactive spreadsheet than an accounting package. Friendly and collaborative in tone, aimed at people tracking what they are owed without sending formal paperwork. Six candidate palettes were explored; Chalk & Pastel is the default, and the rest remain available as switchable themes.

## Constraints

- **Security**: Financial data with real user accounts — parameterized queries, Argon2id password hashing, authorization on every request, append-only financial records. This is production-quality code, not a scratch project.
- **Deployment**: Docker images published through GitHub — the server must be cross-platform and produce a self-contained, easily-deployed artifact.
- **Data backend**: SQLite first, then MariaDB, then PostgreSQL — the data access layer must not bind to SQLite-only semantics, since portability is a committed roadmap item rather than a hypothetical.
- **Platform**: Mobile-first PWA with full offline support — constrains the frontend architecture toward a client capable of local persistence and background sync.
- **Money handling**: Integer minor units only — no floating-point arithmetic anywhere in the accounting path.
- **Tech stack**: Server language and frontend framework deliberately unresolved — to be recommended by the research phase. Go is the incumbent candidate based on actalog experience.

## Key Decisions

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| Self-hosted, others deploy their own instance | Avoids multi-tenant isolation complexity while still serving users beyond the author | — Pending |
| Append-only ledger + full event log | Balances stay reconstructible from primary records; also makes offline sync a merge rather than a conflict | — Pending |
| Integer minor units, per-bucket currency | Eliminates float rounding drift; supports mixed-currency users without FX complexity | — Pending |
| Items are descriptive labels; accounting at bucket level | Matches the original requirements doc; removes the need for payment allocation rules entirely | — Pending |
| Scheduled job posts accrual ledger entries | Real transactions make balances a simple sum and keep the audit trail honest; requires reliable scheduling and catch-up on downtime | — Pending |
| Rate changes effective next period, with optional adjustment entry | Preserves ledger immutability while still allowing correction | — Pending |
| Credits auto-apply to future accruals | Matches the natural mental model of being paid ahead | — Pending |
| Services buckets never "settle" | A recurring tab has no natural end; Settle Up is meaningful only on Declining Balance buckets | — Pending |
| Services invoices auto-generated each period | Gives late fees a due date to work from, with no Provider action required | — Pending |
| Provider records payments on a Payee's behalf but is not a Payee | Clarifies the role model without adding self-dealing permission cases | — Pending |
| Full late-fee automation in v1 | Late fees need due dates and a scheduler, both of which the accrual engine already requires | — Pending |
| Full offline with sync in v1 | Offline-first is architecturally invasive and far cheaper to build in than to retrofit | — Pending |
| Built-in auth plus optional OIDC | Self-hosters get a working default without an IdP, while homelab users can wire in Authentik/Keycloak | — Pending |
| SQLite → MariaDB → PostgreSQL | SQLite proves the accounting model without infrastructure overhead; migrating later validates portability | — Pending |
| Chalk & Pastel default theme | Closest to the "interactive spreadsheet" feel; themeability keeps the other five palettes available | — Pending |
| Server and frontend stack deferred to research | The requirements doc explicitly asks for a recommendation rather than assuming Go | ✓ Resolved — Go 1.26 + chi + sqlc, embedding a SvelteKit 5 PWA; see research/STACK.md |
| Dexie + hand-rolled outbox for offline sync, not RxDB/PowerSync/ElectricSQL | Append-only entries never conflict, so heavyweight sync engines solve a problem this project does not have; PowerSync is FSL-licensed and ElectricSQL requires Postgres | — Pending |
| Balances always derived, never cached in a mutable column | Architecture and pitfalls research converged on this independently; a cache is the primary way "the balance is always correct" fails | — Pending |
| Minimal ntfy payment alert moved into v1 | Research showed notifications — not confirmation flows — are how comparable products catch one-sided recording mistakes; Splitwise removed confirmation after users found it too slow | — Pending |
| First-run one-time admin setup screen | Table stakes for self-hosted apps; friendliest for non-technical deployers, and locks permanently after use | — Pending |
| Late fee cap and logged waiver added to v1 | Standard property-management billing convention, low marginal cost on top of the fee engine already planned | — Pending |

## Evolution

This document evolves at phase transitions and milestone boundaries.

**After each phase transition** (via `/gsd-transition`):
1. Requirements invalidated? → Move to Out of Scope with reason
2. Requirements validated? → Move to Validated with phase reference
3. New requirements emerged? → Add to Active
4. Decisions to log? → Add to Key Decisions
5. "What This Is" still accurate? → Update if drifted

**After each milestone** (via `/gsd-complete-milestone`):
1. Full review of all sections
2. Core Value check — still the right priority?
3. Audit Out of Scope — reasons still valid?
4. Update Context with current state

---
*Last updated: 2026-07-19 after initialization*
