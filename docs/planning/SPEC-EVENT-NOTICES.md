# Spec — Event notices: payment made, payment missed

**Status:** built and tested (v1.3.0)
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
| D-E | Wording per recipient | **One message for everyone**, in generic third person — "{payee} made a payment". No second template slot, no per-reader variant. |
| D-F | Overdue defaults | **Ship editable instance defaults** (d+1 and d+7) rather than leaving overdue silent until an administrator opts in. |

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

### Fix — no migration required

The first draft of this spec called for a migration adding `user_id` to the
primary key. **That was wrong, and reading migration `0008` is what corrected
it.** Its comment states the intended shape outright:

> `event_key` identifies one notifiable event **for one recipient**, e.g.
> `"req:2026-08-01:u7"` (a two-week payment request for the Aug 1 period, **to
> user 7**).

The recipient was always meant to live *inside* the key. The schema is right;
the scan simply dropped the `:u<id>` suffix when it built the key. So the fix is
a one-line scoping change, not a schema change:

- `eventFor(occasion, userID)` appends `:u<id>`, and **every event key in the
  package must go through it**. An unscoped key silently means "this tab has
  been told" rather than "this person has been told".
- No migration, no primary-key change, no table rebuild, and none of the
  attendant risk of silently losing the append-only triggers.

**Upgrade note.** The key format changes, so any event already claimed under the
old format no longer matches. An instance with a reminder in flight sends one
duplicate for that occasion, once. This is exactly the "harmless duplicate" the
send-then-claim design already tolerates, and it is a one-time effect at the
version boundary.

### Test

`TestTickScanNotifiesEveryPayee`: a tab with two payees, both with a usable
channel, receives two deliveries for one reminder event, and a second tick in
the same window sends nothing. Verified to fail without the fix (one delivery)
and pass with it.

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

**One message, sent to everyone, written in generic third person** (D-E) —
"{payee} made a payment of {paid} on {tab}". The payer receives the same text as
the Provider. It reads very slightly oddly to the person who just paid, and that
is the accepted cost of not maintaining two templates, two variable sets, and a
rule about which reader gets which.

New variables, and one thing to be careful about:

| Variable | Meaning |
|----------|---------|
| `{payee}` | Display name of whoever **recorded** the payment |
| `{paid}` | The payment amount |
| `{balance}` | What remains owed after it |

**`{payee}` is the entry's actor, and that is a compromise.** A payment entry
records who posted it and nothing about whom it was posted for. Ordinarily the
payee records their own payment and the wording reads naturally. When a Provider
records one on a payee's behalf, the notice says "Jane Provider made a payment"
— technically true of the entry, misleading about the money. The actor is the
only signal the ledger carries, so this is accepted rather than solved, and
`TestPaymentNoticeNamesWhoeverRecordedIt` pins it so it stays a decision.
Solving it properly would mean recording a subject on payment entries, which is
a ledger change and out of scope here.

**Header safety rests on placement.** `{payee}` appears in the body only; the
title carries `{tab}` alone. Body text cannot inject a header — the separator is
already written — so a control character in a display name is inert, while one
in a tab name still fails the send closed. Moving `{payee}` into the title would
quietly turn a user-controlled name into header input;
`TestPaymentNoticeHeaderSafety` asserts both halves so that change breaks a test.

`{amount}` is **not** reused for the payment amount. It already means "the
balance owed" in every reminder template, including per-tab templates a Provider
has already customised and saved. Redefining it per event type would silently
change the meaning of stored text. A new name costs nothing and cannot misfire.

`{tab}` and `{url}` behave as they do now. Tab names stay in the body, never a
header — the header-injection rule in `internal/notify` is unchanged, and a
display name reaching `{payee}` is user-controlled text subject to exactly the
same treatment.

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

**Defaults (D-F): d+1 and d+7, editable.** Overdue is on out of the box rather
than waiting for an administrator to discover it — a feature nobody switches on
is a feature that does not exist. Two notices is restrained: one the morning
after, one a week later, then silence. An administrator can change them, add a
third, or clear them entirely to turn overdue off.

Note this is a behaviour change on upgrade: an instance that has never touched
its reminder settings starts sending overdue notices it did not send before.
That is the intent, but it belongs in the release notes rather than being a
surprise.

`leadPhrase` needs an overdue form. Today it renders 0/1/7/14 as "today",
"tomorrow", "in one week", "in two weeks"; a negative lead has to read "1 day
ago" / "a week ago" rather than "in -7 days".

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

- **Lookback window.** Only entries newer than `BITT_NOTIFY_LOOKBACK_DAYS`
  (default 7) are candidates. Anything older is never announced — after a week's
  outage, a payment notice is noise, not news. Consumed by NOTIF-01, where
  events first become ledger-derived.
- **Per-tick send ceiling.** `BITT_NOTIFY_MAX_PER_TICK` (default 50) bounds one
  run. Zero or negative is unbounded. Undelivered events are not lost: with no
  claim written, the next tick picks them up.
- **Log what was dropped.** A tick that hits the ceiling logs a warning with the
  count left behind, and adds `deferred=N` to the response body — omitted when
  zero, so its presence is itself the signal. A silent cap reads as "nothing to
  send," which is the failure mode this session already demonstrated is
  expensive to diagnose.

**The ceiling is spent only on work about to happen.** `budget.take()` comes
*after* the already-claimed check, never before. Reversed, a backlog of settled
events consumes the ceiling and starves the events still to send — and because
the starved ones are re-deferred every tick, they are starved permanently.
`TestTickCeilingDrainsOnLaterTicks` pins this: with the order inverted the
second tick reports `sent=0 skipped=2 deferred=1` and the deferred payee is
never reached.

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
      (proves NOTIF-00; verified to fail without the fix)
- [ ] Every event key in the package is built through `eventFor`, so no future
      event type can reintroduce a tab-scoped claim
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
- [ ] `{amount}` still renders the balance owed in an existing saved reminder
      template — the new `{payee}`, `{paid}` and `{balance}` variables must not
      change the meaning of text a Provider already stored
- [ ] A display name containing a control character fails the send closed,
      exactly as a hostile `{tab}` does today
- [ ] A fresh instance has overdue rules at d+1 and d+7, and an administrator
      can edit them, extend them, or clear them to silence overdue entirely
- [ ] A negative lead renders as "1 day ago", never "in -1 days"

---

## 9. Build order

1. **NOTIF-00** — scope the event key to the recipient, multi-payee test. No migration.
   Ships on its own; it is a bug fix and valuable without the rest.
2. **NOTIF-03** — the cap, before the features that need it.
3. **NOTIF-01** — payment-made.
4. **NOTIF-02** — overdue, including `lastUnpaidDue` and negative leads.

Steps 1 and 2 are prerequisites; 3 and 4 are independent of each other.

Version: NOTIF-00 alone is a patch. The set is a minor.

---

## 10. Open questions

None outstanding. The two the draft carried are resolved as D-E and D-F.

Two things to watch during the build rather than decide now:

- **The generic wording on the payer's own copy.** "Alice made a payment" read
  by Alice is the known cost of D-E. If it grates in practice the cheapest
  remedy is dropping the payer from the recipient list, not adding a second
  template.
- **Whether d+1 and d+7 is the right default pair.** It is a judgement, not a
  derivation. Easy to change before release, harder afterwards once instances
  have customised around it.
