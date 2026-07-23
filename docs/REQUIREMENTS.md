# BitTabby — Requirements

**Milestone:** v1
**Defined:** 2026-07-19
**Core value:** The running balance is always correct.

Requirements are grouped by category. Each carries a stable REQ-ID used for roadmap traceability.

---

## v1 Requirements

### Ledger Core (LEDGER)

The accounting spine. Everything else calls into this; nothing else writes entries directly.

- [x] **LEDGER-01**: All financial entries are append-only — the system never updates or deletes a posted entry
- [x] **LEDGER-02**: Append-only is enforced by the strongest mechanism the active backend supports. On SQLite (v1) that is the Ledger Core write boundary — no code outside it issues UPDATE/DELETE against entries — backed by abort triggers that are enabled by default and disableable via config for development and manual repair. SQLite has no role or privilege system, so a trigger is a guardrail against application bugs, not a barrier against an operator with file access. MariaDB and PostgreSQL landed in v1 via Phase 01.1: on PostgreSQL, UPDATE and DELETE on the entries table are additionally revoked from the application role, which the application cannot re-grant itself; on MariaDB, enforcement is a `SIGNAL SQLSTATE` trigger (D-114).
- [x] **LEDGER-03**: A correction is recorded as a reversing entry that references the original, never as an edit
- [x] **LEDGER-04**: Bucket balances are derived by summing ledger entries; no mutable balance column exists as a source of truth
- [x] **LEDGER-05**: All monetary amounts are stored and computed as integer minor units; no floating-point arithmetic appears anywhere in the money path, including serialization and display formatting
- [x] **LEDGER-06**: Every amount carries its currency, and the type system or schema prevents arithmetic between differing currencies
- [x] **LEDGER-07**: Every entry records both an effective timestamp (when it applies) and a created timestamp (when it was recorded)
- [x] **LEDGER-08**: Every entry carries a client-supplied idempotency key with a database-level unique constraint, so a replayed write can never double-post
- [x] **LEDGER-09**: Every entry receives a server-assigned monotonic sequence number that defines authoritative ordering independent of client clocks
- [x] **LEDGER-10**: A user can reconstruct the balance of any bucket as of any past point in time from the entry log alone
- [x] **LEDGER-11**: An event log records who performed each action, what changed, and when, across all major entities

### Buckets & Items (BUCKET)

- [ ] **BUCKET-01**: Provider can create a Services bucket (recurring, accrues on a period, no defined end)
- [ ] **BUCKET-02**: Provider can create a Declining Balance bucket (payments against a fixed total)
- [ ] **BUCKET-03**: Provider can add one or more Items to a bucket as a descriptive breakdown
- [ ] **BUCKET-04**: Items carry no independent balance; all accounting occurs at the bucket level
- [ ] **BUCKET-05**: Provider can attach an existing user to a bucket as Payee; the bucket then appears on that Payee's dashboard
- [ ] **BUCKET-06**: Each bucket declares its own currency, fixed at creation
- [ ] **BUCKET-07**: Each bucket declares its own timezone, used for all period-boundary calculations
- [ ] **BUCKET-08**: A bucket balance may be negative (owed) or positive (credit)
- [ ] **BUCKET-09**: Payee can view the full entry history for any bucket they participate in

### Recurring Accrual (ACCRUAL)

- [ ] **ACCRUAL-01**: Provider can configure a recurring amount and period for a Services bucket
- [ ] **ACCRUAL-02**: A scheduler posts a ledger entry for each elapsed accrual period automatically
- [ ] **ACCRUAL-03**: Posted periods are tracked in a dedicated table with a unique constraint on (bucket, period), making double-posting impossible even under concurrent or repeated runs
- [ ] **ACCRUAL-04**: After downtime, the scheduler catches up by posting every missed period exactly once
- [ ] **ACCRUAL-05**: The scheduler sweeps hourly rather than daily, so period boundaries remain correct across timezones and DST transitions
- [ ] **ACCRUAL-06**: Period boundaries are computed correctly for month-end anchors (a Jan 31 anchor does not drift in February)
- [ ] **ACCRUAL-07**: Provider can change a bucket's recurring rate with a recorded reason; the change takes effect the following period and never alters posted entries
- [ ] **ACCRUAL-08**: Rate history is versioned, so catch-up posting for a past period uses the rate in force during that period
- [ ] **ACCRUAL-09**: Provider can post an adjustment entry to correct the current period

### Credits (CREDIT)

- [ ] **CREDIT-01**: Payee can record a payment at any time, including in advance of any amount owed
- [ ] **CREDIT-02**: A credit balance automatically offsets future accruals until exhausted
- [ ] **CREDIT-03**: A bucket in credit displays a "paid through" date derived from the credit and the current rate

### Invoicing & Settlement (INV)

- [ ] **INV-01**: Services buckets auto-generate an invoice each accrual period with a due date
- [ ] **INV-02**: Provider can adjust a generated invoice before it is issued
- [ ] **INV-03**: Payee can mark an invoice paid in a single tap from the dashboard, with amount and date prefilled
- [ ] **INV-04**: The settle action captures the payment method (cash, transfer, other) since payments occur outside the app
- [ ] **INV-05**: A recorded payment can be undone, implemented as a reversing entry rather than a deletion
- [ ] **INV-06**: Provider can record a payment on a Payee's behalf, attributed to the Provider in the event log
- [ ] **INV-07**: Declining Balance buckets support a Settle Up action that clears the outstanding balance
- [ ] **INV-08**: Services buckets present a running balance and payment log rather than a Settle Up action

### Late Fees (FEE)

- [ ] **FEE-01**: Provider can configure a late fee per bucket as a fixed amount or a percentage
- [ ] **FEE-02**: Provider can configure a grace period per bucket
- [ ] **FEE-03**: Overdue invoices accrue late fees automatically as ledger entries after the grace period elapses
- [ ] **FEE-04**: Percentage-based fees allocate remainders deterministically so no minor unit is created or lost by rounding
- [ ] **FEE-05**: Provider can configure a cap on total accrued late fees per bucket
- [ ] **FEE-06**: Provider can waive a late fee; the waiver is recorded as a reversing entry with a reason

### Accounts & Access (AUTH)

- [x] **AUTH-01**: User can create an account with email and password, hashed with Argon2id
- [x] **AUTH-02**: User can log in and remain logged in across sessions via secure, HttpOnly, SameSite cookies
- [x] **AUTH-03**: User can log out from any page
- [x] **AUTH-04**: Instance can optionally be configured to authenticate via an external OIDC provider
- [x] **AUTH-05**: A fresh deployment presents a one-time setup screen to create the first admin account, which locks permanently once completed
- [x] **AUTH-06**: Admin can manage user accounts and global settings
- [x] **AUTH-07**: Every request is authorized against the specific bucket it touches; a user can never read or write a bucket they do not participate in
- [x] **AUTH-08**: A user can hold the Provider role on some buckets and the Payee role on others

### Offline & Sync (SYNC)

- [ ] **SYNC-01**: The app installs as a PWA to the home screen and opens to a cached shell
- [ ] **SYNC-02**: User can view current balances and tab history while offline
- [ ] **SYNC-03**: User can record a payment while offline; it queues locally in an outbox
- [ ] **SYNC-04**: Queued entries sync automatically on reconnect via an explicit application-managed queue, not the service worker's background sync
- [ ] **SYNC-05**: A replayed or duplicated sync request never produces a duplicate ledger entry
- [ ] **SYNC-06**: Entry ordering is determined by server-assigned sequence, so a client with a wrong clock cannot corrupt the ledger
- [ ] **SYNC-07**: The interface always indicates whether displayed data is current or stale, and shows pending unsynced entries distinctly
- [ ] **SYNC-08**: A client offline for an extended period syncs correctly on return without data loss

### Notifications (NOTIF)

Minimal slice only — the safety net for one-sided payment recording.

- [ ] **NOTIF-01**: When a payment is recorded on a bucket, the other party receives an ntfy notification
- [ ] **NOTIF-02**: Each user can configure their own ntfy topic, or disable notifications entirely
- [ ] **NOTIF-03**: Notification topics are cryptographically random rather than guessable
- [ ] **NOTIF-04**: A notification never triggers a state change by itself; any action it links to requires explicit confirmation in an authenticated session

### Interface (UI)

- [ ] **UI-01**: The dashboard presents active tabs as cards showing current balance and status at a glance
- [ ] **UI-02**: The interface is mobile-first and fully usable on desktop
- [ ] **UI-03**: Settling from the dashboard takes one tap plus one confirmation, with all fields prefilled
- [ ] **UI-04**: Theme is switchable, with Chalk & Pastel as the default
- [ ] **UI-05**: Theme colors are defined as design tokens rather than hardcoded values
- [ ] **UI-06**: Settling produces immediate visual feedback via a transition to the settled state color

### Deployment & Data (DEPLOY)

- [x] **DEPLOY-01**: The application runs on SQLite as its v1 data backend
- [x] **DEPLOY-02**: All dialect-specific SQL is isolated in a data access layer, with no dependency on SQLite-only behavior or on `UPDATE ... RETURNING`
- [x] **DEPLOY-03**: Schema migrations run automatically and safely on startup
- [ ] **DEPLOY-04**: The application ships as a Docker image published via GitHub
- [ ] **DEPLOY-05**: The image runs as a non-root user
- [ ] **DEPLOY-06**: Secrets are supplied by environment or file rather than baked into the image or committed to the repository
- [ ] **DEPLOY-07**: Documented backup and restore procedure for the financial data store

---

## v2 Requirements (deferred)

Expected eventually; deliberately not in v1.

- ~~**MariaDB backend support**~~ — **delivered in v1 by Phase 01.1** (promoted out of v2 as BACKLOG PORT-01); MariaDB is now the probable production target
- ~~**PostgreSQL backend support**~~ — **delivered in v1 by Phase 01.1**; first-class production option
- **Full notification system** — email delivery, per-event preferences, configurable templates, due/overdue/upcoming reminders
- **Invitations and terms acceptance** — Provider invites a Payee with terms; Payee accepts or rejects
- **Reporting and exports** — pre-made reports, aggregations, forecasts, exporting to PDF/XLSX/Markdown/CSV
- **Ad-hoc search** — search across invoices, payments, and messages with exportable results
- **In-app private messaging** — direct messages between parties with notification alerts
- **Receipt attachment** — attach a document to an entry (attachment only; no OCR, no line-item allocation)
- **External integrations** — Invoice Ninja and payment portal connectors

---

## Out of Scope

Explicitly excluded, with reasoning, to prevent silent re-adding.

- **Payment processing** — BitTabby records payments made in other systems and never touches money. Adding processing changes the product's regulatory posture entirely.
- **Multi-tenant SaaS hosting** — each deployment serves one trusted group. Research confirms the self-hosted + mobile-first intersection is the underserved niche; tenant isolation is a different product.
- **Multi-currency conversion** — buckets are single-currency. FX is table stakes for travel-splitting apps but not for recurring domestic billing, and historical rate handling is a large surface. *(Research flagged this tension explicitly; the exclusion was retained deliberately.)*
- **Per-item balances and payment allocation rules** — accounting is bucket-level by design, which removes an entire class of allocation logic. Matches the original requirements document.
- **Itemized receipt splitting and OCR** — structurally incompatible with bucket-level accounting. *(Research flagged this; attachment-only support is deferred to v2 as the compatible subset.)*
- **Two-party payment confirmation / reject flow** — Splitwise built this and removed it after users found it too slow, which is precisely the "too many clicks" failure this project is designed against. The event log, undo-as-reversal, and NOTIF-01 serve the same purpose without the friction.
- **Automatic proration on rate change** — rate changes take effect next period by design; proration adds date-math complexity and edge cases for marginal benefit.
- **Multi-party debt netting** — a bilateral Provider/Payee model, not a group-splitting model. Netting is structurally incompatible.
- **Open public registration** — self-hosted instances provision users deliberately; open signup is an attack surface with no benefit here.
- **Balance snapshot caching** — explicitly not built in v1. At expected scale, deriving balances per read is fast enough, and a cache reintroduces exactly the drift risk the append-only design eliminates. If ever needed, it must be an additive, provably-rebuildable projection — never a second source of truth.

---

## Traceability

Requirement-to-phase mapping. Populated during roadmap creation.

| REQ-ID | Phase | Status |
|--------|-------|--------|
| LEDGER-01 | Phase 1 | Complete |
| LEDGER-02 | Phase 1 | Complete |
| LEDGER-03 | Phase 1 | Complete |
| LEDGER-04 | Phase 1 | Complete |
| LEDGER-05 | Phase 1 | Complete |
| LEDGER-06 | Phase 1 | Complete |
| LEDGER-07 | Phase 1 | Complete |
| LEDGER-08 | Phase 1 | Complete |
| LEDGER-09 | Phase 1 | Complete |
| LEDGER-10 | Phase 1 | Complete |
| LEDGER-11 | Phase 1 | Complete |
| DEPLOY-01 | Phase 1 | Complete |
| DEPLOY-02 | Phase 1 | Complete |
| DEPLOY-03 | Phase 1 | Complete |
| AUTH-01 | Phase 2 | Complete |
| AUTH-02 | Phase 2 | Complete |
| AUTH-03 | Phase 2 | Complete |
| AUTH-04 | Phase 2 | Complete |
| AUTH-05 | Phase 2 | Complete |
| AUTH-06 | Phase 2 | Complete |
| AUTH-07 | Phase 2 | Complete |
| AUTH-08 | Phase 2 | Complete |
| BUCKET-01 | Phase 3 | Pending |
| BUCKET-02 | Phase 3 | Pending |
| BUCKET-03 | Phase 3 | Pending |
| BUCKET-04 | Phase 3 | Pending |
| BUCKET-05 | Phase 3 | Pending |
| BUCKET-06 | Phase 3 | Pending |
| BUCKET-07 | Phase 3 | Pending |
| BUCKET-08 | Phase 3 | Pending |
| BUCKET-09 | Phase 3 | Pending |
| CREDIT-01 | Phase 3 | Pending |
| INV-03 | Phase 3 | Pending |
| INV-04 | Phase 3 | Pending |
| INV-05 | Phase 3 | Pending |
| INV-06 | Phase 3 | Pending |
| INV-07 | Phase 3 | Pending |
| INV-08 | Phase 3 | Pending |
| UI-01 | Phase 3 | Pending |
| UI-02 | Phase 3 | Pending |
| UI-03 | Phase 3 | Pending |
| UI-04 | Phase 3 | Pending |
| UI-05 | Phase 3 | Pending |
| UI-06 | Phase 3 | Pending |
| ACCRUAL-01 | Phase 4 | Pending |
| ACCRUAL-02 | Phase 4 | Pending |
| ACCRUAL-03 | Phase 4 | Pending |
| ACCRUAL-04 | Phase 4 | Pending |
| ACCRUAL-05 | Phase 4 | Pending |
| ACCRUAL-06 | Phase 4 | Pending |
| ACCRUAL-07 | Phase 4 | Pending |
| ACCRUAL-08 | Phase 4 | Pending |
| ACCRUAL-09 | Phase 4 | Pending |
| CREDIT-02 | Phase 4 | Pending |
| CREDIT-03 | Phase 4 | Pending |
| INV-01 | Phase 5 | Pending |
| INV-02 | Phase 5 | Pending |
| FEE-01 | Phase 5 | Pending |
| FEE-02 | Phase 5 | Pending |
| FEE-03 | Phase 5 | Pending |
| FEE-04 | Phase 5 | Pending |
| FEE-05 | Phase 5 | Pending |
| FEE-06 | Phase 5 | Pending |
| SYNC-01 | Phase 6 | Pending |
| SYNC-02 | Phase 6 | Pending |
| SYNC-03 | Phase 6 | Pending |
| SYNC-04 | Phase 6 | Pending |
| SYNC-05 | Phase 6 | Pending |
| SYNC-06 | Phase 6 | Pending |
| SYNC-07 | Phase 6 | Pending |
| SYNC-08 | Phase 6 | Pending |
| NOTIF-01 | Phase 7 | Pending |
| NOTIF-02 | Phase 7 | Pending |
| NOTIF-03 | Phase 7 | Pending |
| NOTIF-04 | Phase 7 | Pending |
| DEPLOY-04 | Phase 7 | Pending |
| DEPLOY-05 | Phase 7 | Pending |
| DEPLOY-06 | Phase 7 | Pending |
| DEPLOY-07 | Phase 7 | Pending |

**Coverage:** 79/79 v1 requirements mapped across 7 phases. No orphans, no duplicates.

---
*Requirements defined: 2026-07-19*
*Informed by: `.planning/research/SUMMARY.md`*
*Traceability populated: 2026-07-19 during roadmap creation (see `.planning/ROADMAP.md`)*
