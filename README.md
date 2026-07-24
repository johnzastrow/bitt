# BitTabby

**Tabs, not invoices.**

A self-hosted, mobile-first web app for tracking money owed between people who trust each other. Providers bill, Payees pay, and the app keeps one authoritative running balance per tab.

BitTabby never touches money. It records payments made elsewhere — cash, Venmo, a bank transfer — so both sides share one record they can trust.

Built for personal, self-hosted home use: family phone plans, shared insurance, recurring services, a loan being paid off between relatives.

## Status

**Usable.** Phases 1-4 of 6 complete — 49 of 54 v1 requirements.

```
make build && ./bittabby     # http://localhost:8080
```

A fresh deployment opens on a one-time setup screen. From there you can add a
second person, create a tab with line items, give it a schedule so it bills
itself, post one-off charges as well, attach the other person as payee, settle
in one tap plus one confirmation or pay any other amount, record a payment on
someone's behalf, undo anything as a reversing entry, pay ahead to build a
credit, and read a per-period statement showing what each cycle covered and what
has been paid against it. Tabs come in two kinds, Services and Payoff, and a
tab's name, kind, people, and schedule stay editable after creation; a tab can
be archived, which stops it billing without touching its history. Payoff loans
track remaining balance, progress, and on-track/behind status; charge late fees
(fixed or percentage, with grace and a cap) and declining-balance interest, both
accruing lazily on read; and a payment request appears in-app two weeks before it
is due. Pushed notifications (email/ntfy) and packaging are still to come.

- [docs/PROJECT.md](docs/PROJECT.md) — what this is, what it deliberately is not, and why
- [docs/REQUIREMENTS.md](docs/REQUIREMENTS.md) — 54 v1 requirements with stable IDs
- [docs/DATA-MODEL.md](docs/DATA-MODEL.md) — physical schema, ER diagram, and data dictionary
- [docs/planning/ROADMAP.md](docs/planning/ROADMAP.md) — five phases with exit criteria
- [docs/planning/STATE.md](docs/planning/STATE.md) — where things currently stand
- [docs/planning/HANDOFF.md](docs/planning/HANDOFF.md) — pick the work up cold

## Core value

**The running balance is always correct.** Everything else is negotiable.

That commitment drives the architecture: entries are append-only and never edited, corrections are reversing entries, balances are derived by summing entries rather than cached in a column, and all money is integer USD cents with no floating point anywhere in the money path.

## Design at a glance

| Concern | Approach |
|---|---|
| Stack | Go, server-rendered with templ and htmx. One language, one static binary |
| Data | SQLite first, MariaDB second, behind a single repository interface |
| Recurrence | Due periods post lazily inside the read transaction — no background scheduler |
| Idempotency | Every form carries a key; a double-tapped submit posts exactly once |
| Corrections | Undo posts a reversing entry; the original stays visible |
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
