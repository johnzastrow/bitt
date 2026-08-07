# Spec — Event notices: payment made, payment missed

**Status:** specified, not built
**Drafted:** 2026-08-07
**Completes:** the Phase 5 follow-ups recorded in
[ROADMAP.md](ROADMAP.md) ("payment-made/missed event notices, a backlog cap")
and [HANDOFF-PHASE5.md](HANDOFF-PHASE5.md) ("Documented follow-ups").

Phase 5's stated goal was "email and ntfy delivery of: a payment request two
weeks before a due date, a reminder one week and one day before (all
configurable), **and notices on a payment made and a payment missed, to all
parties on a tab**." Only the pre-due reminders shipped. This closes the gap.

---

## 1. What exists today

One event type, `reminder:<due>:d<lead>`, delivered by the hourly
`/internal/tick` scan. Two delivery call sites in the whole codebase: that scan,
and the admin test button.

Nothing notifies on a payment, a missed due date, a posted charge, a settlement,
or being added to a tab. The Provider — the person actually owed money — receives
no notification of any kind.

Overdue is not merely unimplemented, it is structurally unreachable:
`nextUnpaidDue` discards past periods (`if p.Due.Before(today) { continue }`), so
`daysBetween(today, due)` is never negative and no past-due date can match a rule.

---

## 2. Decisions taken

| # | Decision | Choice |
|---|----------|--------|
| D-A | Who receives a payment-made notice | **All parties on the tab, including the payer.** The payer's copy is a receipt confirming the payment landed. |
| D-B | Overdue cadence | **Configurable, mirroring the existing lead days**, expressed as negative leads (d+1, d+7). Per-tab overridable exactly like current reminders. |
| D-C | Who receives overdue notices | **Payee and Provider.** The payee is chased; the Provider learns the money did not arrive. |
| D-D | Opt-out granularity | **Reuse the existing `notify_email` / `notify_ntfy` toggles.** No migration, no new profile UI. |

D-B carries a useful consequence: **the cap falls out of the configuration.**
Configured negative leads of d+1, d+7, d+14 produce exactly three overdue
notices per period and then silence. There is no separate "stop nagging" rule to
design, and no unbounded dunning loop.

D-D's accepted cost: someone who wants due-date reminders cannot decline payment
chatter. Revisit only if it is felt in practice.

---

## 3. Prerequisite — NOTIF-00: the claim key is missing `user_id`

**This must land before anything else in this spec, and it is a live defect
today.**

`sent_notifications` is keyed:

```sql
PRIMARY KEY (tab_id, event_key, channel)
```

and `WasSent(ctx, tabID, eventKey, channel)` checks exactly that. But
`runNotifications` loops over every payee, and each calls `WasSent` with the
same three values. So on a tab with more than one payee:

1. Payee A — not sent → deliver → `ClaimSent` writes the row
2. Payee B — `WasSent` returns **true** → skipped, silently

**Only the first payee on a tab ever receives a reminder.** The row records
`user_id`, and there is an index on it (`idx_sent_notifications_user`), but
neither the key nor the check uses it. The per-payee loop is pointless as
written, which is what marks this as an oversight rather than a design choice.

It is latent today because production tabs appear to have a single payee. Every
feature in this spec notifies several people about one event, so it would bite
immediately and invisibly: the first recipient would claim the event for
everyone else.

### Fix

- Migration `0011`, both `sqlite/` and `mariadb/`: change the primary key to
  `(tab_id, event_key, channel, user_id)`.
- `WasSent` gains a `userID` parameter and filters on it.
- SQLite cannot alter a primary key in place: rebuild as create-new →
  `INSERT … SELECT` → drop-old → rename. **The append-only triggers must be
  recreated on the new table** — they are row-level and do not block the DDL, so
  it is easy to end up with the constraint quietly gone. Assert their presence
  after migrating.
- Existing rows carry a real `user_id`, so the copy is lossless and no
  already-sent notice is re-sent.

### Test

A tab with two payees, both with a usable channel, receives two deliveries for
one reminder event. This test fails before the migration and passes after.

---

## 4. NOTIF-01 — Payment-made notices

### Trigger

A payment entry appearing in the ledger. Events are **derived from the ledger,
not queued**: the tick lists entries per tab and treats each unnotified payment
as an event.

This is not an efficiency choice. Phase 5's stated risk is that outbound
delivery "must stay off the balance path entirely — a failed or double send can
never affect a ledger," and the handoff is blunter: a send inside a ledger
transaction "becomes a ledger write. Do not." Sending inline from the payment
handler would put SMTP and HTTP I/O on the ledger write path. Deriving from the
ledger keeps every existing property: no timer in the binary, idempotent,
self-healing, off the balance path.

### Event key

`payment:<seq>`

The entry sequence is unique and immutable, so the existing claim table dedupes
for free once NOTIF-00 lands. No new table.

### Recipients

Every participant on the tab — payees, the Provider, and tab administrators —
including the actor who posted the payment (D-A).

### Suppression

Per the handoff, event notices are **exempt from the stale-state suppression**
that governs reminders: a payment is a fact that occurred, and remains true
whether or not a balance is now owed. The `balance >= 0 → skip` rule must NOT be
applied here.

One exception, which is not stale-state suppression but truth: **an entry that
has been reversed is not announced.** A payment posted and undone between two
ticks never happened as far as the tab is concerned, and `Entry.ReversesSeq`
makes this cheap to check. A payment already announced and *later* undone is not
retracted — see §7.

### Message

Reuses the existing template mechanism with the reminder variables plus the
payment amount and the payer's display name. Tab names stay in the body, never
a header — the header-injection rule in `internal/notify` is unchanged.

---

## 5. NOTIF-02 — Payment-missed (overdue) notices

### Trigger

A period whose due date has passed while a balance is still owed.

Requires `nextUnpaidDue` to stop discarding past periods. Rather than change its
meaning — reminders depend on it returning a *future* date — add a sibling
`lastUnpaidDue` that returns the most recent past-due unpaid period. The
reminder path keeps the function it has; the overdue path gets its own.

### Event key

`overdue:<due>:d<n>` where `n` is the positive number of days *since* the due
date. Distinct from `reminder:` keys, so the two never collide in the claim
table.

### Cadence

Negative leads on the existing per-tab reminder rules (D-B). A rule at d+1 fires
one day after the due date, d+7 a week after. Instance defaults apply to any tab
the Provider has not customised, exactly as reminders do.

The existing "a customised tab is customised completely" rule carries over
unchanged: a tab's rule list replaces the instance list rather than merging.

### Recipients

The payee who owes, and the Provider (D-C). Tab administrators are **not**
included by default — they are not owed the money and the tab may have several.
Reconsider if it is missed.

### Suppression

Unlike NOTIF-01, overdue notices **do** re-derive live state before sending:
nothing owed means nothing overdue. A period paid late, between the due date and
the d+7 notice, must not produce that notice. This is the existing
`balance >= 0 → skip` behaviour and it applies here.

Archived tabs are skipped, as now.

---

## 6. NOTIF-03 — Backlog cap

The outstanding "cap and drain" item from the Phase 5 security review, and now
load-bearing: deriving events from the ledger means a long cron outage could
otherwise produce a burst of notices across every tab at once.

- **Lookback window.** Only entries newer than `BITT_NOTIFY_LOOKBACK` (default
  7 days) are candidates. Anything older is never announced — after a week's
  outage, a payment notice is noise, not news.
- **Per-tick send ceiling.** `BITT_NOTIFY_MAX_PER_TICK` (default 50) bounds one
  run. Undelivered events are not lost: with no claim written, the next tick
  picks them up.
- **Log what was dropped.** A tick that hits the ceiling logs the count left
  behind. A silent cap reads as "nothing to send," which is the failure mode
  this session already demonstrated is expensive to diagnose.

The review also asked for per-recipient failure bounding — stop retrying a hard
failing recipient. That needs attempt/error columns on `sent_notifications`,
which the handoff anticipates. **Deferred**: it is a separate change with its own
migration, and the ceiling above already bounds the blast radius.

---

## 7. Deliberately not in scope

- **Retracting a notice when a payment is undone.** Once a notice is out it
  cannot be recalled; a "payment reversed" notice is a different feature and
  should be decided on its own merits, not smuggled in here.
- **Notices for charges, fees, settlement, or tab membership.** Each is a
  separate event type with its own recipient question.
- **Digesting several events into one message.** A tab with ten payments in an
  hour sends ten notices. Acceptable at current scale; revisit if it bites.
- **Per-event-type opt-out** (D-D).
- **Per-recipient failure bounding** (§6).

---

## 8. Exit criteria

- [ ] A tab with two payees receives two deliveries for one reminder event
      (proves NOTIF-00; fails before the migration)
- [ ] The append-only triggers on `sent_notifications` still exist and still
      refuse UPDATE and DELETE after migration `0011`, on both backends
- [ ] Posting a payment produces one notice per participant per enabled channel,
      including the payer
- [ ] A payment posted and reversed before the next tick produces no notice
- [ ] A payment announced once is never announced twice, across restarts
- [ ] A tab past its due date with a balance owed produces an overdue notice at
      each configured negative lead, and none at any other time
- [ ] A period paid before a later overdue lead produces no further notice
- [ ] An archived or settled tab produces nothing
- [ ] Nothing in this spec writes to the ledger, and no delivery happens inside
      a ledger transaction
- [ ] A tick that exceeds the ceiling logs what it deferred, and the next tick
      delivers it

---

## 9. Build order

1. **NOTIF-00** — migration `0011`, `WasSent` signature, multi-payee test.
   Ships on its own; it is a bug fix and valuable without the rest.
2. **NOTIF-03** — the cap, before the features that need it.
3. **NOTIF-01** — payment-made.
4. **NOTIF-02** — overdue, including `lastUnpaidDue` and negative leads.

Steps 1 and 2 are prerequisites; 3 and 4 are independent of each other.

Version: NOTIF-00 alone is a patch. The set is a minor.

---

## 10. Open questions

- **Should the Provider's payment-made notice differ in wording from the
  payer's receipt?** Same event, different reader — "you paid" versus "someone
  paid you." Cheap to do at template level; needs a decision on whether the
  per-tab template mechanism grows a second slot.
- **What does a tab with no Provider-set rules do about overdue?** The instance
  defaults currently carry no negative leads, so overdue would be silent until
  an administrator adds one. Ship a default (say d+1) or leave it opt-in?
