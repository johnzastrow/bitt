# Spec — Reconciling payments against bank statements

**Status:** draft for approval (revised 2026-10-01 with the owner's answers and the reference export)
**Drafted:** 2026-10-01
**Requested by:** the owner: "upload a CSV of bank transactions and match them to
payments recorded in BitTabby. The amounts and dates will differ, and the CSV
format may change. The delta ... needs to be recorded back on the tab (credit or
debit). One CSV may serve many tabs, and one tab may get payments from many
CSVs." Available to administrators only, with new controls.

---

## 1. What exists today

- A payment is a `payment` entry: positive cents, a `method`, an `effective_at`
  (may be backdated) and a `created_at`, append-only, idempotent by key
  (LEDGER-05, LEDGER-07, PAY-02). Payments on a Payoff tab are allocated
  interest-first and count toward the late-fee window.
- A correction is an `adjustment`: signed, reason required, `method = ''`. A
  credit adjustment is allocated principal-first (DATA-MODEL §5).
- Undo is a `reversal` of one whole entry; at most one per entry.
- **Who may post:** `CanTransact()` is participants only (PAY-03, AUTH-05). An
  administrator overseeing a tab may manage it but **cannot** post a payment to
  it; a test guards that from both sides.
- Nothing records what happened in a bank. Whether "$100 by transfer on the 3rd"
  matches what actually arrived is invisible to the app.

## 2. Decisions taken (2026-10-01)

| Topic | Decision |
|---|---|
| Who | **Administrators only**, and only those given the new **Can reconcile** permission (section 3). |
| Delta | **By what moved.** Bank shows more than recorded: the extra is a new `payment` (money moved; it counts for late fees and interest-first allocation). Bank shows less: a debit `adjustment` (money recorded but not moved). Equal: no entry. The recorded payment is never touched. |
| Matching | **Suggest, a person confirms.** Matches are scored and proposed; nothing reaches the ledger until an administrator confirms a match or a batch. |
| Unmatched bank lines | **Offer to record them** as a new payment on a chosen tab, prefilled from the line; or mark them "not BitTabby" so they are not shown again. |
| CSV layouts | **Saved column mappings**, recognised by the header row; a new or changed layout is mapped once, with a preview. |
| Many to many | One file may hold lines for many tabs; a tab may be reconciled from many files. Overlapping files do not duplicate lines (section 5). |
| Record of matches | The database records **which row of which uploaded file matched which payment on which tab**, queryable from either end (section 6). |
| Dates | **Flag only.** A date difference is shown and stored on the match; it never changes an entry. |
| Match parameters | **Adjustable** in the Reconciliation setup controls (section 7). |
| Retention | **Bank lines are kept forever**, matched, recorded and ignored alike; no purge. |

## 3. RECON-01 — The "Can reconcile" control

A new per-account permission, **off by default**, that only an administrator
can hold and only an administrator can grant or remove.

- Shown and changed on the People screen beside the existing administrator
  controls; every change is logged with both user ids (the 1.5.0 pattern).
- Removing an account's administrator role removes this permission with it, in
  the same transaction.
- It allows exactly three writes, and only from the reconciliation screens:
  the delta `payment`, the delta `adjustment`, and a `payment` recorded from an
  unmatched bank line. It does **not** change `CanTransact()`: the ordinary
  payment form on a tab the administrator is not on still refuses, and the
  existing guard test stays as it is.
- Every reconciliation entry is attributed to the administrator who confirmed
  it (`actor_user_id`) and points at its bank line (section 6), so "who changed
  this balance, and on what evidence" has an answer.
- An instance setting, **Bank reconciliation: on/off** (default off), hides the
  feature entirely until an administrator turns it on.

This is a **documented exception to AUTH-05's administrator rule**, narrowed to
entries backed by an imported bank line. It belongs in PROJECT.md's Key
Decisions when built.

## 4. RECON-02 — Importing a CSV

**Upload:** one file at a time, at most 5 MB and 10,000 data rows; UTF-8 (with or
without BOM) or Windows-1252, comma or semicolon separated, quoted fields per RFC
4180, **including line breaks inside a quoted field**. Anything else is refused
with the reason. The upload keeps the file name and its SHA-256; each kept line
keeps its **original row text** and its row number in the file, so a match can
always be shown against exactly what the bank exported. Outgoing lines are not
stored.

**The reference export (2026-10-01):** the owner's credit union's
"TransactionHistory" CSV. What the parser must handle, all seen in that file:

| Column | Example | Handling |
|---|---|---|
| `Account` | `PRIMARY SHARE` | optional **account** column: one export can cover several accounts; stored per line and shown |
| `Date` | `9/1/2026` | month/day/year, no zero padding |
| `Description` | `"Deposit Transfer` + newline + sender details` | **two lines inside quotes**; both kept; the second line often names the sender, which feeds name matching |
| `Note` | sometimes present | optional **note** column, kept, used in matching text |
| `Check #` | usually empty | optional **reference** column |
| `Amount` | `$150.00`, `"$3,239.01"`, `($336.41)` | `$` and thousands separators stripped; **parentheses mean negative** (money out) |
| `Balance` | `"$3,239.01"` | ignored |

It also contains monthly **dividends** ($0.04 to $0.12) as incoming deposits:
real money in, never a BitTabby payment. The minimum line amount and the ignore
rules (section 7) exist for these. The real file is **never committed**; tests use
a synthetic file in the same layout.

**Layouts (`bank_formats`):** the header row is normalised (trimmed, lower-case)
and hashed into a signature. A known signature picks its saved mapping; an
unknown one opens the mapping screen:

- which column is the **date**, and its layout (chosen from a short list of
  common layouts, shown against the file's first rows);
- **amount** as one signed column (a leading minus or **parentheses** for
  negative, `$` and thousands separators allowed), or as separate **debit** and
  **credit** columns;
- **which direction is incoming** (money received is positive or negative in
  this export);
- **description**, and optionally **account**, **note** and **reference**
  columns; anything unmapped (such as a running balance) is ignored;
- a name for the layout, such as the bank and account.

A preview of the first ten parsed rows is shown before saving. Amounts are parsed
straight to integer cents (thousands separators and currency symbols stripped,
no floating point), per the money rule.

**Only incoming money is kept** for matching: outgoing lines in the same export
(groceries, rent) are counted in the import summary and discarded.

## 5. Duplicates across files

Bank exports overlap (last month's file and this month's both contain the 28th).
Each line gets a **fingerprint**: layout id, date, amount, normalised
description, reference, and its occurrence number among identical lines in that
file (two genuine $50 transfers on one day stay two lines). The fingerprint is
**UNIQUE**; a re-imported line is counted as "already imported" and skipped.

## 6. Storage

New tables, beside the ledger and never inside it. No change to `entries`.

| Table | Holds | Key rules |
|---|---|---|
| `bank_formats` | saved mappings | `header_signature` UNIQUE |
| `bank_imports` | one row per upload: layout, file name, file SHA-256, who, when, counts (kept, skipped as duplicate, outgoing, ignored by rule, refused) | |
| `bank_lines` | one incoming line: import, **row number in the file**, original row text, account, `posted_on` (date), `amount_cents` (positive), description, note, reference, fingerprint, `state` (`open`, `matched`, `recorded`, `ignored`), ignore reason | `fingerprint` UNIQUE |
| `bank_matches` | a match: **bank line, its import, the payment `entry_seq`, and the payment's `tab_id`**; the delta entry seq (nullable); bank and recorded dates and amounts as they were; the date flag; who and when confirmed; `undone_at` | see below |

**Which row matched which payment on which tab** is one join: `bank_matches`
→ `bank_lines` (row number, original text) → `bank_imports` (file name, upload
date), and `bank_matches.entry_seq` → `entries` (the payment) with
`bank_matches.tab_id` stored alongside, so a tab's reconciliation history is a
direct lookup and a file's is too. Undone matches keep their row (with
`undone_at`), so the history of a line includes matches later undone.
Nothing here is ever purged.

**One active match per bank line and per payment, on both backends.** MariaDB has
no partial unique indexes, so the rule cannot be "UNIQUE where `undone_at` IS
NULL". Instead each match row carries `active_line_id` and `active_entry_seq`,
set while the match stands and set to NULL when it is undone, each UNIQUE (both
backends allow many NULLs). A second match of the same line or payment is then a
constraint violation, not a race.

## 7. RECON-03 — Suggesting matches, and the setup controls

Candidates are **unmatched, unreversed `payment` entries** on any tab (the
administrator holds Can reconcile), within the import's date range widened by
the date window.

Each pair (bank line, payment) is scored, and only pairs inside every limit are
considered:

| Signal | Rule |
|---|---|
| Date | within the **date window** either side |
| Amount | within the **amount tolerance**; exact scores highest |
| Name | words of a tab participant's display name, or the tab's name, in the line's description or note |
| Method | recorded method `transfer` scores above `cash` (cash rarely appears in a bank) |

Pairs are assigned best score first, each line and each payment used once. Ties
are not guessed: they are shown as "two possible payments" for a person to pick.

**Reconciliation setup** (a Setup section on the Reconciliation screen, holders
of Can reconcile only; every change logged with who and when):

| Control | Default | Range | Why |
|---|---|---|---|
| Date window | 7 days | 0 to 60 | how far a bank date may be from the recorded date |
| Amount tolerance, percent | 10% | 0 to 100 | relative difference allowed |
| Amount tolerance, at most | $25.00 | $0 to $10,000 | a cap, so 10% of a large payment is not huge |
| Amount tolerance, at least | $1.00 | $0 to $100 | a floor, so cents of rounding still match small payments |
| Minimum line amount | $1.00 | $0 to $10,000 | lines below it are not offered for matching (the dividends) |
| Use names | on | on/off | score the description against participant and tab names |
| Date flag after | 3 days | 0 to 60 | see section 8 |
| Ignore rules | none | a list | "description contains ..." per layout; matching lines are imported as **ignored**, with the rule as the reason (for example `Deposit Dividend`) |

The tolerance used for a pair is the percent of the recorded amount, held
between the floor and the cap. Changing a control affects suggestions from then
on; confirmed matches are untouched.

## 8. RECON-04 — Confirming a match and posting the delta

Shown per match: the bank line (date, amount, description), the recorded payment
(tab, date, amount, method, who recorded it), and **what confirming will post**:

| Bank vs recorded | Posts | Detail |
|---|---|---|
| Bank B > recorded R | `payment` of B − R | method `transfer`, `effective_at` = bank date, memo "Bank reconciliation: <description> (line N of <file>)" |
| Bank B < recorded R | `adjustment` of −(R − B) | a debit; reason "Bank reconciliation: recorded <R>, bank shows <B> (<description>)" |
| B = R | nothing | the match is recorded only |

- **Dates are flagged, never posted** (decided 2026-10-01). The match is
  flagged when the bank and recorded dates differ by more than the **date flag**
  setting, or fall on different sides of one of the tab's due dates (the case
  that can move a late fee). The flag is shown on the card, stored on the match,
  and listed on the tab's reconciliation history; the recorded payment's date
  stands.
- Each posted entry's idempotency key is derived from the match
  (`recon:<match id>`), so confirming twice is a replay, not a second entry.
- "Confirm all exact matches" confirms every suggestion where B = R in one
  action.
- **Undo** a match: reverses its delta entry (if any) with the normal reversal,
  sets `undone_at`, and returns the line and the payment to unmatched.

## 9. RECON-05 — Lines with no matching payment

- **Record as a payment:** choose the tab (searched by name or participant);
  the form is prefilled with the bank amount, date and description, method
  `transfer`. Posting it marks the line `recorded` and links the entry.
- **Not BitTabby:** marks the line `ignored`, with an optional note; it never
  shows again unless un-ignored.
- **Unmatched payments** (recorded, with no bank line in the imported range) are
  listed for information only: cash, or a bank not imported. No action.

## 10. Screens (mobile first)

**RECON-00 — Navigation moves under the avatar (decided 2026-10-01).** Today the
top bar holds the brand, then for administrators **People** and
**Notifications**, then the avatar, then Log out. A third link would make it
too wide on a phone. Instead the avatar and name become the button for a menu:

| Menu item | Shown to |
|---|---|
| Profile | everyone (today the avatar itself links here) |
| People, Notifications | administrators (moved from the top bar) |
| **Reconciliation** | administrators holding Can reconcile, when the instance switch is on |
| Log out | everyone (the existing POST form with its CSRF token, moved into the menu) |

Built as a native `<details>`/`<summary>` disclosure: no JavaScript, so nothing
new for the Content Security Policy; keyboard and screen-reader accessible by
default; closes on selection by navigation. The top bar is then brand and avatar
only, at every width. The current-page item is marked (`aria-current`). This
changes the layout for every account, so it is built and released first, on its
own.

1. **Reconciliation** (from the avatar menu, `/admin/reconcile`): upload, recent
   imports with their counts, and the open work: suggested matches, unmatched
   lines.
2. **Map this layout** (only for an unknown header): the column choices and the
   ten-row preview.
3. **Review**: suggested matches as cards, each with its delta and Confirm;
   "Confirm all exact matches"; unmatched lines with Record and Not BitTabby;
   unmatched payments, collapsed.
4. **On a tab**: a reconciled payment shows a small "matched to bank" mark with
   the line's date and amount; delta entries read as ordinary entries with
   their memo.

## 11. Security notes

- Upload: CSRF, Can reconcile checked on every request (not only on the menu),
  size and row caps, parsed with `encoding/csv` with `LazyQuotes` off; no cell is
  ever evaluated, and nothing is exported back as CSV (no formula injection
  surface).
- Rate limit uploads per account.
- Bank descriptions and original rows contain names and sometimes account
  fragments, and are **kept forever** (decided 2026-10-01): stored only for
  incoming lines, shown only to holders of Can reconcile, included in backups
  like other data. The real example file stays out of the repository.
- Every reconciliation write goes through the ledger service, inside its
  transaction, with the normal append-only and idempotency guarantees.

## 12. Deliberately not in scope

- **Split or combined payments** (one transfer covering two tabs, or two
  transfers for one payment). A line matches one payment and a payment one line;
  the rest is recorded by hand. Revisit if real files need it.
- Bank connections (Plaid and similar), OFX/QFX/CAMT formats, scheduled imports.
- Changing a recorded payment's date from the bank's.
- Outgoing lines (a Payee's own bank): this reconciles money received.

## 13. Exit criteria

- A file with lines for three tabs imports; re-importing it (and an overlapping
  file) adds nothing; outgoing lines are counted and dropped.
- A new header asks for a mapping once; the same header later does not.
- A synthetic file in the reference layout parses exactly: two-line quoted
  descriptions, `$` and thousands separators, `($336.41)` as outgoing (dropped),
  `9/1/2026` dates, the Account and Note columns; dividends below the minimum
  line amount are not offered, and an ignore rule marks them ignored with its
  reason.
- Each setup control changes suggestions as stated (window, percent between
  floor and cap, minimum amount, names on/off, date flag), and a change is
  logged; out-of-range values are refused.
- For any tab, its reconciliation history lists each matched payment with the
  file name, row number and original row; for any file, each row's outcome. An
  undone match still appears, marked undone.
- A date flag appears when the dates differ by more than the setting, or straddle
  one of the tab's due dates; no entry's date changes.
- Suggestions: exact match, within tolerance, outside tolerance (not suggested),
  a tie (shown, not guessed).
- Confirm with B > R posts a payment, B < R a debit adjustment, B = R nothing;
  each balance is exactly right; confirming twice posts once; undo restores the
  balance exactly.
- A payment and a line can each be in one active match, under concurrent
  confirms, **on SQLite and on MariaDB**.
- Without Can reconcile, an administrator gets 403 on every reconciliation route,
  and still cannot use the ordinary payment form on a tab they are not on.
- The avatar menu: a non-admin sees Profile and Log out only; an administrator
  also People and Notifications; Reconciliation only with the permission and the
  switch on. Log out still requires the CSRF token. Nothing overflows at 360 px.
- Deep tests on both backends, per the working agreement.

## 14. Build order

0. RECON-00: the avatar menu (People, Notifications and Log out move into it);
   its own small release, since every account sees it.
1. RECON-01: the permission, the instance switch, the People control.
2. RECON-02 and 5: formats, import, fingerprints (migration).
3. RECON-03: suggestions (pure function, heavily tested).
4. RECON-04: confirm, delta posting, undo.
5. RECON-05: record and ignore.
6. Screens and an end-to-end walk on a phone width; a release.

## 15. Open questions

None blocking. Answered 2026-10-01: dates are flagged only; match parameters are
adjustable in the setup controls; bank lines are kept forever; the database
records which file row matched which payment on which tab.

One proposal to confirm while building: the **ignore rules** and **minimum line
amount** in section 7 were added after the reference file showed monthly
dividends arriving as deposits.
