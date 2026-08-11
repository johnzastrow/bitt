# Spec — Seeing and controlling reminders

**Status:** specified, not built
**Drafted:** 2026-08-11
**Prompted by:** a production audit of `btabby.fluidgrid.site` that found three
settings problems, none of which the interface could have shown anyone.

---

## 1. What the audit found

The instance had been running for days with behaviour nobody could see, and
two of my own statements about it were wrong. Both errors have the same root
cause: **I reasoned from the code's defaults instead of reading the instance.**

### 1.1 Overdue notices are OFF here, not on

`instance_reminders` holds rows for 14, 7 and 1 days. A stored set **replaces**
the built-in one entirely — it does not merge — so the built-in overdue rules at
−1 and −7 are gone. Tab 2 additionally has its own per-tab rows (14/7/1), which
replace the instance set in turn.

So NOTIF-02 shipped, deployed, and is inert on this instance. Nothing in the
interface says so: the notification screen lists the rules, but nothing tells an
administrator that overdue exists and their configuration excludes it.

### 1.2 The v1.4.0 `{payment}` fix will NOT reach this instance

v1.4.0 changes the **built-in default** templates to lead with `{payment}`. The
stored rows here read `{tab}: {amount} due {when}` — the old text. Stored
templates are data; changing a default in code does not rewrite them.

Deploying v1.4.0 alone therefore leaves the car loan reminder still quoting
`$21,877.58`. The templates have to be edited, and **an administrator has no way
to know that** — the screen shows the template text, not what it renders to.

### 1.3 The Provider receives nothing, by design

Reminders go to payees only. On all three tabs the roles are:

| Tab | provider | payee | admin |
|---|---|---|---|
| 1 Tobi Phone Car Insurance | user 1 | user 3 | user 2 |
| 2 Tobi Car Loan | user 1 | user 3 | user 2 |
| 3 Pot Violation | user 1 | user 3 | user 2 |

User 1 is the Provider everywhere, so **user 1 never receives a reminder for
anything**. The car loan reminder went to user 3, which the claim rows confirm
(`reminder:2026-08-12:d1:u3`). This is correct per the design, but it is
invisible: nothing on the tab says who a reminder will actually reach.

### 1.4 Two users share a stale ntfy topic

Users 2 and 3 both have `ntfy_topic = FuzzyShark14` — the retired topic. On the
self-hosted server that topic has no reader ACL and no subscriber, so their
ntfy notifications publish successfully into a void. Worse, sharing one topic
between two people means either could read the other's notices.

**Neither user can fix this without logging in themselves**, and an
administrator cannot fix it for them. That is feature 3 below.

---

## 2. The common thread

All four findings are the same failure: **reminder behaviour is invisible until
it fires, and by then it has already gone to real people.** Nothing in the
interface answers the three questions an administrator actually has:

- What will this reminder *say*?
- *Who* will get it?
- Can I see it happen *now*, rather than waiting for a due date?

The three features below answer one question each.

---

## 3. REM-01 — Render the reminder in tab setup

**The highest-value item.** A rendered preview at configuration time would have
caught 1.2 before it reached anyone: `$21,877.58` in a title is obviously wrong
when you can see it, and invisible when you are looking at `{amount}`.

### What it shows

Beneath the Reminders card on a tab's Setup, for each configured lead time, the
Title and Body **as they would actually send**, with every variable substituted
from the tab's *live* figures — not sample data. Sample data would have rendered
a plausible `$450.00` and hidden the bug.

### Values

| Variable | Source |
|---|---|
| `{tab}` | the tab's name |
| `{amount}` | current balance owed |
| `{payment}` | current payment due — the installment on a Payoff tab |
| `{due}` | the next unpaid due date |
| `{days}`, `{when}` | that rule's lead time |
| `{url}` | the tab's URL |

When a figure cannot be derived — no schedule, nothing owed — the preview says
so in place of the rule rather than rendering a misleading zero. "This rule
cannot render: the tab has no schedule" is information; `$0.00` is a lie.

### It must also answer "who"

Beside the preview, the recipients that rule would reach, by name and channel,
derived from the same `channelsFor` logic the scan uses. A payee with no usable
channel is listed as **skipped, with the reason** — no topic set, notifications
off, no email. That single line is what would have exposed 1.4 immediately.

### Scope

Read-only. It renders; it does not send.

---

## 4. REM-02 — Send a reminder now, from tab setup

Answers "can I see it happen now".

### Behaviour

A control on the tab's Reminders card that runs the real send path for that tab
immediately: real message, real recipients, real channels.

### Decided: preview-to-self, bypassing both gates

The send goes **to the administrator who pressed it, and to nobody else**. It
renders the real message for the real tab, over that administrator's own
channels. It is a rehearsal, not an operational "mail everyone now" button.

That choice makes the other two decisions safe rather than dangerous:

- **Does it bypass the already-sent claim?** Respecting it means pressing the
  button for an occasion already delivered does nothing, silently — the exact
  failure mode this whole spec exists to eliminate. Bypassing means deliberate
  duplicates are possible.
- **Does it bypass the lead-day match?** Respecting it means the button only
  works on the precise day the reminder would have fired anyway, which is
  almost never the day you press it.

**Decided: bypass both.** Since the message only reaches the person who asked
for it, a duplicate is harmless and a button that silently does nothing is not.

**It MUST NOT write a claim.** This is the load-bearing detail of the whole
feature. A claim is what suppresses a later send of the same occasion — so a
rehearsal that claimed `reminder:2026-08-12:d1:u3` would silently cancel the
real reminder to the real payee. The rehearsal is not the event. Nothing is
recorded in `sent_notifications`, and the scheduled send proceeds untouched.

The result line should say plainly who it went to ("sent to you, over email and
ntfy") so it can never be mistaken for having notified the tab.

### Guard rails

- Provider or tab administrator only — the same authority that edits the tab.
- Rate limited per tab, so it cannot become a way to mail somebody repeatedly.
- Logged distinctly (`manual reminder sent`) so a claim row's origin is legible
  afterwards.

---

## 5. REM-03 — An administrator can edit another user's notification settings

Answers 1.4, which currently has no answer at all: two users hold a retired
ntfy topic and nobody but those users can change it.

### Behaviour

On the existing admin Users screen, an administrator may edit another account's
**notification settings only**: ntfy topic, email toggle, ntfy toggle. The same
validation the profile form uses (`notify.ValidTopic`) applies unchanged.

### Boundaries

- **Notification settings only.** Not password, not email address, not role. An
  administrator resetting somebody's password is a different feature with a
  different risk profile and should not arrive as a side effect of this one.
- The change is **logged with both user ids** — who changed whose. Editing
  another person's settings is exactly the action an audit trail is for.
- A user can still change their own settings; this does not take that away.

### Worth surfacing while there

The Users screen should show each account's topic and toggles in the list, not
only in an edit form. Two users sharing one topic is visible at a glance in a
column, and invisible in three separate forms.

---

## 6. Not in scope

- Rewriting stored templates automatically to use `{payment}`. Tempting, but it
  edits text a Provider wrote. The preview (REM-01) makes the problem visible
  and the fix is then one deliberate edit. See §8 for the operational steps.
- Per-user notification settings beyond the three fields above.
- Any change to who receives what. The Provider not receiving reminders is a
  design decision, and if it is wrong it deserves its own discussion rather
  than being quietly altered here.

---

## 7. Open questions

1. ~~Manual send: bypass the claim?~~ **Decided: yes**, and no claim is written.
2. ~~Manual send: bypass the lead-day match?~~ **Decided: yes.**
3. ~~Real recipients or self?~~ **Decided: preview-to-self.** It is a rehearsal.
   An operational "send to everyone now" button remains unbuilt and would be a
   separate decision.
4. **Should the Provider receive reminders too?** Out of scope here, but the
   audit shows the Provider currently receives nothing on any tab, which may
   not be what anyone intended.

---

## 8. Operational fixes this instance needs regardless

These are data, not code, and none of them wait for the features above:

1. **Update the stored templates** — instance and tab 2 — to lead with
   `{payment}`, after v1.4.0 is deployed. Until then `{payment}` renders
   literally.
2. **Add overdue rules** (−1, −7) to the instance set if overdue is wanted; it
   is currently off.
3. **Fix the ntfy topics** for users 2 and 3 — one topic each, not shared, with
   a matching ntfy account and read ACL per `~/ntfydocker/README.md`.
4. **Decide whether the Provider should be a payee** on any tab, or accept that
   user 1 receives no reminders.
