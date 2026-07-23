# BitTabby

**Tabs, not invoices.**

A self-hosted, mobile-first web app for tracking money owed between people who trust each other. Providers bill, Payees pay, and the app keeps one authoritative running balance per tab.

BitTabby never touches money. It records payments made elsewhere — cash, Venmo, a bank transfer — so both sides share one record they can trust.

Built for personal, self-hosted home use: family phone plans, shared insurance, recurring services, a loan being paid off between relatives.

## Status

Pre-implementation. Scope is defined; no application code yet.

- [docs/PROJECT.md](docs/PROJECT.md) — what this is, what it deliberately is not, and why
- [docs/REQUIREMENTS.md](docs/REQUIREMENTS.md) — 54 v1 requirements with stable IDs
- [docs/planning/ROADMAP.md](docs/planning/ROADMAP.md) — five phases with exit criteria
- [docs/planning/STATE.md](docs/planning/STATE.md) — where things currently stand

## Core value

**The running balance is always correct.** Everything else is negotiable.

That commitment drives the architecture: entries are append-only and never edited, corrections are reversing entries, balances are derived by summing entries rather than cached in a column, and all money is integer USD cents with no floating point anywhere in the money path.

## Design at a glance

| Concern | Approach |
|---|---|
| Stack | Go, server-rendered with templ and htmx. One language, one static binary |
| Data | SQLite first, MariaDB second, behind a single repository interface |
| Recurrence | Due periods post lazily inside the read transaction — no background scheduler |
| Money | Integer USD cents. USD only |
| Tabs | Services (recurring, no end) and Payoff (fixed total drawn down) |
| Items | Amounts that sum to the periodic charge, so cost changes are visible. No per-item balances |
| Statements | Rendered from ledger entries, not stored as records |
| Auth | Email and password with Argon2id, per-tab authorization on every request |
| Deployment | Single binary, Docker image via GitHub, non-root |

## Prior work

BitTabby follows [actalog](https://github.com/johnzastrow/actalog) and [bit-tabby](https://github.com/johnzastrow/bit-tabby). bit-tabby scoped v1 at 79 requirements and stalled: three phases went into ledger core, OIDC-capable auth, and three database backends without ever reaching a screen where a person could look at a tab.

This rewrite inverts the ordering. A usable application exists after Phase 1, and every phase after that adds to something you can already open and use.

## License

Not yet chosen.
