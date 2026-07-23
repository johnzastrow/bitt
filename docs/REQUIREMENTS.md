# BitTabby — Requirements

**Milestone:** v1
**Defined:** 2026-07-23
**Core value:** The running balance is always correct.
**Target:** Personal, self-hosted home use. One household per deployment.

Requirements are grouped by area. Each carries a stable REQ-ID used for roadmap traceability.

**Status:** Phases 1 and 2 complete and verified (30/54). Phase 3 next.

This is a reduction of the bit-tabby-derived scope from 79 requirements to 54. The pre-revision document is in git history. See [PROJECT.md](PROJECT.md) for the reasoning behind each cut.

---

## v1 Requirements

### Ledger Core (LEDGER)

The accounting spine. Everything else calls into this; nothing else writes entries directly.

- [x] **LEDGER-01**: Posted entries are append-only — the system never updates or deletes one. Enforced by a single write boundary in code, backed by a database abort trigger enabled by default and disableable via configuration for development and manual repair
- [x] **LEDGER-02**: A correction is recorded as a reversing entry referencing the original, never as an edit
- [x] **LEDGER-03**: Tab balances are derived by summing ledger entries; no mutable balance column exists as a source of truth
- [x] **LEDGER-04**: All monetary amounts are stored and computed as integer USD cents; no floating-point arithmetic appears anywhere in the money path, including serialization and display formatting
- [x] **LEDGER-05**: Every entry records an effective timestamp (when it applies), a created timestamp (when it was recorded), and the user who created it
- [x] **LEDGER-06**: Every entry receives a server-assigned monotonic sequence number defining authoritative ordering independent of client clocks
- [x] **LEDGER-07**: Every entry carries a client-supplied idempotency key with a database-level unique constraint, so a repeated or replayed write can never double-post

### Tabs & Items (TAB)

- [x] **TAB-01**: Provider can create a Services tab — recurring charges on a schedule, with no defined end
- [ ] **TAB-02**: Provider can create a Payoff tab — a fixed total drawn down by payments, with an expected payment schedule
- [x] **TAB-03**: Provider can attach an existing user to a tab as Payee; the tab then appears on that user's dashboard
- [x] **TAB-04**: A tab carries one or more line items, each with its own amount; the items sum to the tab's periodic charge
- [x] **TAB-05**: Items carry no balance of their own — payments settle against the tab as a whole, with no allocation rules
- [x] **TAB-06**: A tab balance may be negative (owed) or positive (credit), and both parties can view the tab's full entry history

### Schedules & Periods (SCHED)

- [ ] **SCHED-01**: A tab's schedule is an anchor date plus one of: weekly, every two weeks, monthly on a given day, or monthly on the last day
- [ ] **SCHED-02**: Period boundaries are computed in the instance-wide configured timezone and do not drift at month end — a day-31 anchor lands correctly in February and returns to the 31st afterward
- [ ] **SCHED-03**: Due periods are posted lazily, computed and written inside the transaction that reads the tab, so no background scheduler process exists and catch-up after downtime is inherent rather than engineered
- [ ] **SCHED-04**: A unique constraint on (tab, period) makes posting a period twice impossible, including under concurrent reads
- [ ] **SCHED-05**: Every period carries a due date derived from the schedule, with no invoice record required to supply one

### Charges & Statements (CHG)

- [ ] **CHG-01**: Each posted period snapshots its item breakdown, so both parties can see exactly which line changed and when a cost shifted
- [ ] **CHG-02**: Provider can change an item's amount or add an item; the change takes effect the following period and never alters posted entries
- [x] **CHG-03**: Provider can post a one-off charge or a correcting adjustment to any tab
- [ ] **CHG-04**: A period statement renders the charge, its item breakdown, its due date, and the payments applied to it — computed from ledger entries and stored nowhere separately

### Payments & Settlement (PAY)

- [x] **PAY-01**: Payee can record a payment in a single tap from the dashboard, with amount and date prefilled
- [x] **PAY-02**: A payment records its method (cash, transfer, other), since money moves outside the app
- [x] **PAY-03**: Provider can record a payment on a Payee's behalf, attributed to the Provider on the entry
- [x] **PAY-04**: A recorded payment can be undone, implemented as a reversing entry rather than a deletion
- [x] **PAY-05**: A payment made in advance of anything owed becomes a credit that automatically offsets future charges until exhausted

### Payoff Tabs (PAYOFF)

- [ ] **PAYOFF-01**: A Payoff tab shows its remaining balance and progress against the original total
- [ ] **PAYOFF-02**: A Payoff tab shows whether the Payee is on track, ahead, or behind its expected payment schedule
- [ ] **PAYOFF-03**: A Payoff tab reaching a zero balance is marked settled and moves out of the active dashboard

### Late Fees (FEE)

Driven entirely by the schedule's due dates. Reuses the lazy posting path.

- [ ] **FEE-01**: Provider can configure a late fee per tab as either a fixed amount or a percentage
- [ ] **FEE-02**: Provider can configure a grace period per tab, expressed in days after the due date
- [ ] **FEE-03**: Once the grace period elapses on a period that remains unpaid, a late fee posts automatically as a ledger entry
- [ ] **FEE-04**: Late fees post through the same lazy path as charges, with a unique constraint on (tab, period, fee) making double-assessment impossible
- [ ] **FEE-05**: A percentage fee is computed on the overdue period charge rather than the running balance, so fees never compound on previously assessed fees; rounding to whole cents is deterministic and documented
- [ ] **FEE-06**: Provider can configure a cap on total accrued late fees per tab
- [ ] **FEE-07**: Provider can waive a late fee; the waiver is recorded as a reversing entry with a reason

### Accounts & Access (AUTH)

- [x] **AUTH-01**: User can create an account with email and password, hashed with Argon2id
- [x] **AUTH-02**: User can log in and remain logged in across sessions via secure, HttpOnly, SameSite cookies, and can log out from any page
- [x] **AUTH-03**: A fresh deployment presents a one-time setup screen creating the first admin account, which locks permanently once completed
- [x] **AUTH-04**: Admin can add and remove user accounts, and may also hold the Provider or Payee role on tabs
- [x] **AUTH-05**: Every request is authorized against the specific tab it touches; a user can never read or write a tab they do not participate in, and may hold Provider on some tabs and Payee on others

### Interface (UI)

- [x] **UI-01**: The dashboard presents each active tab as a card showing current balance and status at a glance
- [x] **UI-02**: The interface is mobile-first and fully usable on desktop
- [x] **UI-03**: Settling from the dashboard takes one tap plus one confirmation, with every field prefilled and immediate visual feedback on success
- [x] **UI-04**: Colors, spacing, and type are defined as design tokens using the Chalk & Pastel palette rather than hardcoded values
- [ ] **UI-05**: The app installs to a phone home screen as a PWA and opens to a cached shell; all data operations require a connection

### Deployment & Data (DEPLOY)

- [x] **DEPLOY-01**: The application runs on SQLite as its initial data backend, with schema migrations running automatically and safely on startup
- [x] **DEPLOY-02**: All SQL is isolated behind a repository interface with no dependence on SQLite-only behavior, so a second backend can be added without touching call sites
- [ ] **DEPLOY-03**: MariaDB is supported as a second backend, selectable by configuration
- [x] **DEPLOY-04**: The application ships as a single static binary with templates, assets, and migrations embedded
- [ ] **DEPLOY-05**: A Docker image is published via GitHub and runs as a non-root user
- [ ] **DEPLOY-06**: Secrets are supplied by environment or file rather than baked into the image or committed to the repository
- [ ] **DEPLOY-07**: Backup and restore for the financial data store is documented

---

## Deferred (v2 and beyond)

Expected eventually; deliberately not in v1.

- **Offline data entry and sync** — local outbox, queued payments, sync on reconnect, replay idempotency, stale-data indicators. The v1 write path is built idempotent and API-shaped specifically so this stays additive
- **Invoice as a stored entity** — issued/due/paid lifecycle with Provider adjustment before issue. v1 renders statements from ledger entries instead
- **Notifications** — ntfy and email delivery, per-event preferences, due and overdue reminders
- **PostgreSQL backend** — the repository interface does not preclude it
- **OIDC / external identity providers** — for homelab users wanting Authentik or Keycloak
- **Theme switching** — v1 defines tokens for one palette; alternates become cheap
- **Point-in-time balance reconstruction UI** — the data supports it; the interface is deferred
- **Full event log across all entities** — v1 records the acting user on each ledger entry, which covers the financial trail
- **Reporting and exports** — pre-made reports, aggregations, forecasts, PDF/XLSX/Markdown/CSV
- **Ad-hoc search** across entities
- **In-app private messaging**
- **Receipt attachment** — attachment only, no OCR and no line-item allocation
- **External integrations** — Invoice Ninja, payment portal connectors

---

## Out of Scope

Explicitly excluded, with reasoning, to prevent silent re-adding.

- **Payment processing** — BitTabby records payments made in other systems and never touches money. Adding processing changes the product's regulatory posture entirely
- **Multi-tenant SaaS hosting** — each deployment serves one household. Tenant isolation is a different product
- **Multi-currency and FX** — USD only. Every target use case is domestic and recurring
- **Per-item balances and payment allocation rules** — items break down what a charge covers; they never accrue or settle independently. This removes an entire class of allocation logic
- **Itemized receipt splitting and OCR** — structurally incompatible with tab-level accounting
- **Balance snapshot caching** — the single most likely way "the balance is always correct" fails. At household scale, deriving balances per read is fast enough. If ever needed it must be an additive, provably-rebuildable projection, never a second source of truth
- **Two-party payment confirmation and reject flow** — Splitwise built this and removed it after users found it too slow, which is precisely the friction this project is designed against. Undo-as-reversal serves the same purpose
- **Automatic proration on cost change** — changes take effect the following period by design; proration adds date math and edge cases for marginal benefit
- **Compounding late fees** — percentage fees are computed on the overdue period charge, never on a balance that already includes fees
- **Multi-party debt netting** — a bilateral Provider/Payee model, not a group-splitting model
- **Open public registration** — self-hosted instances provision users deliberately; open signup is attack surface with no benefit here
- **Background scheduler process** — periods and fees post lazily inside read transactions. A timer process was the source of the catch-up, DST, and downtime complexity in the predecessor

---

## Traceability

Requirement-to-phase mapping. Every phase after Phase 1 adds to an application a person can already open and use.

| Phase | Theme | Requirements | Count |
|-------|-------|--------------|-------|
| 1 | Walking skeleton — it runs, you log in, a tab shows a correct balance | LEDGER-01, 03, 04, 05, 06; TAB-01, 04, 05; CHG-03; AUTH-01, 02, 03; UI-02; DEPLOY-01, 02, 04 | 16 |
| 2 | The settle loop — this is the product | LEDGER-02, 07; TAB-03, 06; PAY-01, 02, 03, 04, 05; AUTH-04, 05; UI-01, 03, 04 | 14 |
| 3 | Recurrence — schedules, lazy accrual, statements | SCHED-01, 02, 03, 04, 05; CHG-01, 02, 04 | 8 |
| 4 | Payoff tabs and late fees | TAB-02; PAYOFF-01, 02, 03; FEE-01, 02, 03, 04, 05, 06, 07 | 11 |
| 5 | Ship — MariaDB, Docker, PWA shell | UI-05; DEPLOY-03, 05, 06, 07 | 5 |

| REQ-ID | Phase | Status |
|--------|-------|--------|
| LEDGER-01 | Phase 1 | Complete |
| LEDGER-02 | Phase 2 | Complete |
| LEDGER-03 | Phase 1 | Complete |
| LEDGER-04 | Phase 1 | Complete |
| LEDGER-05 | Phase 1 | Complete |
| LEDGER-06 | Phase 1 | Complete |
| LEDGER-07 | Phase 2 | Complete |
| TAB-01 | Phase 1 | Complete |
| TAB-02 | Phase 4 | Pending |
| TAB-03 | Phase 2 | Complete |
| TAB-04 | Phase 1 | Complete |
| TAB-05 | Phase 1 | Complete |
| TAB-06 | Phase 2 | Complete |
| SCHED-01 | Phase 3 | Pending |
| SCHED-02 | Phase 3 | Pending |
| SCHED-03 | Phase 3 | Pending |
| SCHED-04 | Phase 3 | Pending |
| SCHED-05 | Phase 3 | Pending |
| CHG-01 | Phase 3 | Pending |
| CHG-02 | Phase 3 | Pending |
| CHG-03 | Phase 1 | Complete |
| CHG-04 | Phase 3 | Pending |
| PAY-01 | Phase 2 | Complete |
| PAY-02 | Phase 2 | Complete |
| PAY-03 | Phase 2 | Complete |
| PAY-04 | Phase 2 | Complete |
| PAY-05 | Phase 2 | Complete |
| PAYOFF-01 | Phase 4 | Pending |
| PAYOFF-02 | Phase 4 | Pending |
| PAYOFF-03 | Phase 4 | Pending |
| FEE-01 | Phase 4 | Pending |
| FEE-02 | Phase 4 | Pending |
| FEE-03 | Phase 4 | Pending |
| FEE-04 | Phase 4 | Pending |
| FEE-05 | Phase 4 | Pending |
| FEE-06 | Phase 4 | Pending |
| FEE-07 | Phase 4 | Pending |
| AUTH-01 | Phase 1 | Complete |
| AUTH-02 | Phase 1 | Complete |
| AUTH-03 | Phase 1 | Complete |
| AUTH-04 | Phase 2 | Complete |
| AUTH-05 | Phase 2 | Complete |
| UI-01 | Phase 2 | Complete |
| UI-02 | Phase 1 | Complete |
| UI-03 | Phase 2 | Complete |
| UI-04 | Phase 2 | Complete |
| UI-05 | Phase 5 | Pending |
| DEPLOY-01 | Phase 1 | Complete |
| DEPLOY-02 | Phase 1 | Complete |
| DEPLOY-03 | Phase 5 | Pending |
| DEPLOY-04 | Phase 1 | Complete |
| DEPLOY-05 | Phase 5 | Pending |
| DEPLOY-06 | Phase 5 | Pending |
| DEPLOY-07 | Phase 5 | Pending |

**Coverage:** 54/54 v1 requirements mapped across 5 phases. No orphans, no duplicates.

### Sequencing notes

- **Per-tab authorization (AUTH-05) lands in Phase 2, not Phase 1.** Phase 1 has a single admin account and no tab sharing, so there is nothing yet to authorize between users. It must land in the same phase that introduces a second participant (TAB-03), and cannot slip past it.
- **Idempotency keys (LEDGER-07) land in Phase 2** with the first user-facing write path, and are non-negotiable there — they are what makes a double-tapped settle button safe, and what keeps offline capability additive later.
- **Phase 4 depends on Phase 3's schedules.** Late fees are triggered by due dates (SCHED-05), and Payoff progress is measured against an expected schedule. Neither can be pulled forward.

---
*Requirements defined: 2026-07-23*
*Supersedes the bit-tabby-derived v1 scope of 79 requirements; pre-revision version in git history*
