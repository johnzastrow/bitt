# BitTabby — Current State

**Updated:** 2026-10-01 (brought up to v1.6.0; the project has been idle since 2026-08-11)

## Where things stand

**Shipped and deployed, past v1.** All 54 v1 requirements delivered and tagged
`v1.0.0`; since then `v1.1.0` (admin test notification), `v1.1.1` (MariaDB
no-op-save fix + logging), `v1.2.0` (per-tab administrator role + new-tab
form persistence), and a notifications run in August:

| Version | Date | What |
|---------|------|------|
| 1.3.0 | 2026-08-07 | Event notices: payment made (to every party; a receipt for the payer) and payment missed (payee and Provider), derived from the ledger; per-tick send ceiling `BITT_NOTIFY_MAX_PER_TICK` ([SPEC-EVENT-NOTICES.md](SPEC-EVENT-NOTICES.md)) |
| 1.3.1 | 2026-08-10 | Outgoing email carries `Date` and `Message-ID` |
| 1.4.0 | 2026-08-11 | `{payment}` template variable; reminders on Payoff tabs quote the installment, not the whole loan |
| 1.4.1 | 2026-08-11 | Migration `0012`: negative (overdue) lead times can be stored |
| 1.5.0 | 2026-08-11 | Reminders rendered on Setup with live figures and their reach; "Send this to me now"; admins edit others' notification settings ([SPEC-REMINDER-CONTROL.md](SPEC-REMINDER-CONTROL.md)) |
| 1.6.0 | 2026-08-11 | A dedicated payment screen `/tabs/{id}/pay` that reminder links open; login returns to the requested page (allowlisted) |
| 1.7.0 | 2026-10-01 | Navigation moves under the avatar (RECON-00 of [SPEC-BANK-RECONCILE.md](SPEC-BANK-RECONCILE.md)); initials avatars sized correctly. Tagged; not deployed |
| 1.8.0 | 2026-10-01 | Bank reconciliation ([SPEC-BANK-RECONCILE.md](SPEC-BANK-RECONCILE.md)); built with Go 1.26.8 (five reachable stdlib vulnerabilities in the 1.26.5 builds); migrations 0013-0018, rehearsed on a production copy. Tagged and released; not deployed |
| 1.8.1 | 2026-10-01 | Tab page: Record a payment in the top card, every section collapsed on open, Transfer the default method. Committed, **not yet tagged or deployed** |

**Live in production at https://btabby.fluidgrid.site** — a
Docker Compose stack on the `recipe.fluidgrid.site` host, on the host's MariaDB
(`btabby` database), `network_mode: host` behind the host's apt Caddy, matching
the other sites there. The repo is public at github.com/johnzastrow/bitt (MIT),
images publish to `ghcr.io/johnzastrow/bitt` (**amd64 only**, by policy).

**Start the next session from [HANDOFF-OPS.md](HANDOFF-OPS.md)** — it covers the
deployment, the redeploy runbook, the traps learned in the post-v1 sessions, and
the open threads.

| Item | State |
|------|-------|
| Repository | `main`, public; local commits ahead of GitHub (bank reconciliation in progress) |
| Version | 1.8.1 committed 2026-10-01, not yet tagged; 1.8.0 released (image published); production runs 1.6.0 |
| Deployment | Live at https://btabby.fluidgrid.site. Verified 2026-10-01: container `bittabby` runs `ghcr.io/johnzastrow/bitt:1.6.0` (index `sha256:27ffbe1d…`, the image the v1.6.0 release built from `3796f6d`), healthy; host compose pins 1.6.0. **The repo's `compose.fluidgrid.yaml` still pins 1.2.0**: see Next action |
| Scope | 54 requirements, 6 phases |
| Stack | Go 1.26 + templ + htmx 2.0.4 (vendored); SQLite or MariaDB |
| Phase 1 | Complete — walking skeleton |
| Phase 2 | Complete — the settle loop |
| Phase 3 | Complete — recurrence |
| Phase 4 | Complete — payoff tabs, late fees, interest |
| Phase 5 | Complete — notifications (email/ntfy, per-tab and instance settings); follow-ups finished in 1.3.0 to 1.6.0 |
| Phase 6 | Complete — Docker, deploy, backup/restore, MariaDB, and the PWA (UI-05) |

## What works today

The product is usable. Two people, one tab, money tracked correctly, and now
tabs that bill themselves.

```
make build && ./bittabby         # :8080, see .env.example for configuration
```

Run it, complete first-run setup, and you can: add a second person, create a tab
with line items, give it a schedule, post charges by hand as well, attach the
other person as payee, settle in one tap plus one confirmation from the
dashboard, pay a partial or larger amount instead, record a payment on someone
else's behalf, undo any of it as a reversing entry, pay ahead to build a credit
that offsets the next charge, and read a per-period statement showing what each
cycle covered and what has been paid against it.

Tabs are Services or Payoff, stated at the top of the tab, and both kinds are
editable. A tab's name, description, and kind can be changed after creation,
people can be detached as well as attached, and a tab can be archived -- which
stops it billing and drops it down the dashboard without touching a single
entry.

Notifications (email and ntfy) cover the whole cycle: reminders before a due
date, a notice when a payment is made, and an overdue notice when one is missed.
A tab's Setup screen shows each reminder rendered with its live figures, who it
reaches by channel (and why anyone is unreachable), and a "Send this to me now"
rehearsal. A reminder's link opens a one-payment screen, through sign-in if
needed.

## Requirements delivered

| Phase | Requirements |
|-------|--------------|
| 1 | LEDGER-01, 03, 04, 05, 06; TAB-01, 04, 05; CHG-03; AUTH-01, 02, 03; UI-02; DEPLOY-01, 02, 04 |
| 2 | LEDGER-02, 07; TAB-03, 06; PAY-01, 02, 03, 04, 05; AUTH-04, 05; UI-01, 03, 04 |
| 3 | SCHED-01, 02, 03, 04, 05; CHG-01, 02, 04; TAB-02 (pulled forward) |
| 4 | TAB-02; PAYOFF-01, 02, 03; FEE-01…07 |
| 5 | Notifications (added by request; email/ntfy reminders, per-tab + instance config) |
| 6 | DEPLOY-03 (MariaDB), 05 (Docker), 06 (secrets), 07 (backup/restore), UI-05 (PWA) |

## Verification performed

(These are the v1 checks. Each later release's CHANGELOG entry records its own,
and current coverage is in [HANDOFF-OPS.md](HANDOFF-OPS.md).)

- Full suite green, including under `-race`
- Coverage at v1: fee 96%, money 96%, ledger ~90%, schedule 87%, sqlite ~73%, web ~71%, auth 44%
- Migration `0003_schedules` applied to an existing Phase 2 database without
  incident, and the demo tab kept its balance
- Live binary walked end to end over HTTP: a tab anchored ten weeks back posted
  eleven cycles on first read for the correct total, a refresh posted nothing
  more, statements rendered with due dates and breakdowns, and a payment settled
  the oldest cycle first
- **Twenty simultaneous reads of an overdue tab produced 21 claims, 21 entries,
  21 distinct period keys, and exactly the right balance** — one charge per
  cycle under real parallel load (SCHED-04)
- Changing an item's amount left the posted cycle's entry and its snapshot
  untouched, and superseded the item row rather than overwriting it
- `posted_periods`, `posted_fees`, and `posted_interest` UPDATE/DELETE all abort
- Migration 0005 applied cleanly to the existing demo database (through 0004)
- Live: a $5,000 loan at 6% accrued $25 then $23.88 interest on the declining
  balance; a waived fee added $25 back and did not re-assess; a fully paid loan
  read settled and left the active dashboard; version shows v0.4.0 in footer and healthz
- The administrator exception to AUTH-05 is covered from both sides: an admin can
  rename a tab they are not on, and **cannot** post a payment to it. That second
  test caught a real hole during development and now guards it.

## Next action

Nothing is in progress: v1.6.0 is released and live, and both August specs are
built. The deploy and `v1.0.0` items that used to be listed here were done in
July.

Before the next feature:

1. **Bring the repo's compose pin up to 1.6.0.** The repo's
   `compose.fluidgrid.yaml` says `bitt:1.2.0`; the host's `~/bittdocker/` copy
   says 1.6.0 and production runs it (verified 2026-10-01). Redeploy runbook
   step 2 did not reach the repo for 1.3.0 to 1.6.0. Diff the host's file against
   the repo's (other edits may have been made there too), commit the result, so
   the next `scp` of the repo file does not roll production back to 1.2.0.
2. **Update [PROJECT.md](../PROJECT.md).** Its Out of Scope still lists
   notifications as deferred; they shipped (Phase 5, 1.3.0 to 1.6.0).

Then pick from the open threads in [HANDOFF-OPS.md](HANDOFF-OPS.md): per-payee
balances (needs scoping; changes the one-balance-per-tab core), HTTPS 404s for
unknown subdomains, and deeper coverage.

## Working agreement

- Build in long stretches; check in at phase boundaries or genuine design forks
- No permission needed for routine writes, tests, or commits within a phase
- Atomic commits referencing REQ-IDs
- `make check` before committing

## Relationship to bit-tabby

`bitt` is a fresh start, seeded only from bit-tabby's PROJECT.md and
REQUIREMENTS.md. No code, planning artifacts, or completion status carried
over. bit-tabby remains on disk at `../bit-tabby` as a reference
implementation of the ledger core and auth, but it is not a dependency and
nothing here assumes it.
