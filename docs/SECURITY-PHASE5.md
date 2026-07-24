# Security Design — Phase 5 (Notifications)

**Status:** pre-implementation. Phase 5 is gated on this document; read it before
writing the phase, and satisfy every control marked **MUST** below.

Phase 5 is the app's **first outbound side effect** and its **first reach to
external hosts**. That rewrites the threat model rather than extending it:
until now every input was rendered back into the app's own escaped HTML, behind
a strict CSP, and every write went through the append-only ledger. Notifications
send attacker-influenced text to SMTP servers and ntfy hosts, over channels the
CSP does not touch, triggered by an endpoint an external cron must reach without
a session.

This design was produced by a multi-agent threat model (5 reviewers by distinct
lens → adversarial verification of every finding → synthesis). 34 findings were
raised, 15 refuted on verification, 19 survived. **Net risk is low-to-medium and
entirely off the balance path — no surviving finding can touch the ledger.** The
single finding confirmed against behaviour that would exist as designed is
stale-reminder delivery (§2). Everything else is forward-looking hardening that
must be pinned here so it is not silently dropped when code lands.

The governing rule from PROJECT.md / HANDOFF.md stands and is the acceptance
test for the whole phase: **a failed or double send can never affect a ledger.**

---

## 0. Decisions — RESOLVED (2026-07-24)

All three were made by the user before any code. These are now settled; the
implementer follows them rather than re-opening them.

### D1 — SSRF policy for ntfy → **admin-pinned server, topic-only per user**
The ntfy **base URL is instance config** (`BITT_NTFY_URL`, env or `file:`). A
user chooses only a **topic string**, validated against `[A-Za-z0-9_-]` with a
bounded length. This is the smallest attack surface; a per-user full server URL
was rejected. The runtime guards in §3 still apply to the pinned URL: https-only,
no redirect following, private/loopback/link-local IP rejection, connect-time
re-resolution.

### D2 — Delivery semantic → **at-least-once (send-then-claim) for reminders**
Attempt delivery; on confirmed success, write the claim in its own short
transaction (§6). A crash between send and claim re-sends one harmless duplicate;
it never drops a payment request. Payment-made / missed notices are event-
anchored. Correct the ROADMAP/HANDOFF "idempotent, same pattern as
posted_periods" wording, which overclaims exactly-once (§4).

### D3 — `/internal/tick` auth → **shared secret in an Authorization header**
A dedicated secret via `config.loader.str` (env or `file:`), presented as
`Authorization: Bearer <secret>`, compared with
`crypto/subtle.ConstantTimeCompare`, **failing closed when unset**, checked in
middleware **before any scan**, and rate-limited (§5).

**Container note (governing deployment assumption).** BitTabby runs in a
container. Loopback binding of `/internal/*` is therefore **not** a reliable
control — the cron is typically a sidecar or a separate container, not
`localhost`. The shared secret is the **primary** control, not a backstop, and
`§5`'s "bind to loopback where the deployment allows" drops to a rare-case
extra. For the same reason the SSRF private-IP rejection (§3) matters more, not
less: a container can reach cloud metadata (`169.254.169.254`) and sidecar
services inside its network namespace. Secrets arrive as mounted files via
`file:/run/secrets/<name>`, which is world-readable inside an isolated namespace
— `warnIfSecretFileLoose` deliberately warns rather than refuses (§7).

---

## 1. MUST — write and satisfy this document

The phase is explicitly gated on this file (HANDOFF.md). Without it the controls
below live only as prose threats and drift or get dropped. This file is that
enforceable reference; keep it in step with the code.

---

## 2. MUST — do not send a stale reminder (the one CONFIRMED defect)

**Threat.** A claim row proves "not yet sent", not "still relevant". Two ways a
notice goes out for a debt that no longer exists:

- A payee who pays a period **early** still trips the T-7 and T-1 reminders — no
  claim exists yet, so nothing suppresses them.
- After any cron outage, a caught-up tick fires "due in one week" **requests for
  due dates already elapsed or already paid**.

Off the balance path, but a dunning email for a settled debt is a real trust
defect.

**Control.** Before sending a reminder/request, **re-derive live state**: the
tab balance (the same lazy `SUM(entries)` the app already uses) and the period's
paid state. Suppress the send if the period is satisfied, or if its target date
is past a small tolerance — **but still write the claim** to close it out.
Payment-made / payment-missed notices are event-anchored and exempt.

**Test.** Cron dark 14 days must not emit a "due in one week" request for an
already-paid or already-overdue period; an early payer receives no further
reminders for the period they cleared.

---

## 3. MUST — SSRF containment on ntfy delivery

**Threat.** ntfy delivery issues an outbound POST. If the target host is
user-controlled, the self-hosted binary can be aimed at
`http://169.254.169.254/` (cloud metadata / IAM credentials),
`http://127.0.0.1:…/internal/tick`, or any internal host — a request made from
**inside** the trust boundary.

**Controls.**
- Per D1, prefer an admin-pinned base URL; a per-user field carries only a
  topic, validated against a strict charset (`[A-Za-z0-9_-]`, bounded length).
- **https only**; **do not follow redirects** (a 302 to a private IP defeats a
  URL check).
- Resolve the host and **reject** any request whose IP is loopback, link-local
  (`169.254.0.0/16`, `fe80::/10`), RFC1918 / ULA private, or multicast, before
  dialing.
- Route through a dialer that **re-checks the IP at connect time** (defeats DNS
  rebinding), with a short timeout.

**Test.** A configured ntfy host resolving to a private/loopback/link-local IP
is refused before any connection; a redirect to such an IP is not followed.

---

## 4. MUST — email & ntfy header/body injection

**Threat.** Tab name, description, memo, and display name are unfiltered free
text (length-bounded only). They naturally compose the email **Subject** and the
ntfy **Title** header. An embedded CRLF injects extra SMTP headers (a `Bcc:` to
exfiltrate, a spoofed body) or extra HTTP headers on the ntfy call. The CSP and
templ autoescaping protect the browser, **not** these outbound channels. HTML
email that emits a tab name or memo as raw markup — or worse, as an `href` — is
an app-branded phishing vector.

**Controls.**
- Build addresses and headers only through `net/mail.Address` / a MIME library
  that quotes and rejects control characters — **never** string concatenation.
- **Strip or reject CR and LF** in any value placed in a header (email Subject,
  ntfy Title). Keep user text in the **body**, not headers.
- Prefer `text/plain` bodies. If HTML is required, render through templ or an
  equally hardened encoder, and **never** turn a user string into an `href`.

**Related in-repo fix already landed (this commit):** the profile email edit now
uses `looksLikeEmail`, which rejects control characters, so a stored login
identity can no longer carry a CRLF into a future `To:` header. Setup and admin
creation already used the strict check.

**Test.** A tab named with an embedded CRLF produces no extra email/ntfy headers.

---

## 5. MUST — authenticate `/internal/tick`, before any work, rate-limited

**Threat.** An external cron has no session, so `requireAuth` / `requireAdmin`
cannot cover this route — it needs its own secret. A naïve `token == compare` is
timing-attackable; a token in the query string is captured by any fronting
access log; and because the endpoint fans out email + ntfy across N tabs × M
parties, a weak/absent secret **or auth checked after the work** is an
amplification-DoS and a recipient-enumeration primitive.

**Controls.**
- Load the tick secret via `config.loader.str` (env or `file:`), never
  hardcoded.
- Require it in an **`Authorization` header**, never the query string.
- Compare with `crypto/subtle.ConstantTimeCompare`.
- **Fail closed when unset** — return 401 and log an auth failure; the endpoint
  must never run without a configured secret.
- Check it in **middleware, before the scan** (as `requireAuth` already does).
- **Rate-limit** the route (reuse the avatar limiter's fixed-window shape) and
  **cap sends per tick**.
- Loopback binding of `/internal/*` is unreliable here (container: the cron
  is often a separate container), so the secret is the primary control; treat
  loopback binding as a rare-case extra, not a substitute.
- Assert **no code path logs the raw tick request URL or headers.**

**Interaction with lazy accrual (load-bearing).** Computing "what is due" today
runs accrual, which **writes ledger entries** in the read path (`accrueTab`). If
`/internal/tick` triggers accrual, an outbound-triggered, cron-reachable
endpoint becomes a **balance-path write** — exactly what the governing rule
forbids. **Keep the notification scan and the send off the write path:** derive
what to notify from already-posted state without provoking new accrual, or make
the tick's read strictly read-only. This is the single most important structural
decision in the phase.

---

## 6. SHOULD — delivery mechanics

- **Claim outside any ledger transaction.** The ledger's exactly-once property
  comes from writing claim + entry in one SQLite transaction; a network send
  cannot join that transaction, so "same pattern as `posted_periods`" does not
  carry over. Write the sent-notification claim in its **own short transaction**,
  never inside a ledger transaction, so a send failure can never roll back a
  financial entry.
- **Bound permanently-failing recipients.** Record attempt count + last error on
  the claim; stop retrying a hard-failing address every tick, and alert rather
  than loop.
- **Cap and drain.** "A missed hour sends late rather than never" can replay a
  large backlog in one burst (self-inflicted mail flood, sender-domain
  blocklisting). Cap sends per tick and per recipient per window; drain a
  backlog gradually; **drop time-relative reminders whose target date has
  passed** rather than sending them late (only made/missed notices are
  legitimately "late is better than never").
- **Resolve recipients at send time from active participants only** — never a
  cached list. A removed or deactivated participant must not receive a notice;
  a deactivated account's ledger entries stay, but its notifications stop.
- **Build links from a validated `BITT_BASE_URL`,** never from `r.Host`. A
  notification email with a link built from a request Host header is a
  host-header-injection phishing vector; the tick request's Host is whatever the
  cron sent.

---

## 7. Secrets

- SMTP credentials, ntfy tokens, and the tick secret must **never** reach a log,
  an error message, a rendered page, or a notification body. A failed SMTP auth
  must not log the password; a delivery error must not echo the token.
- Supply via env or `file:` through `config.loader.str`.
- **Fail closed on an unreadable `file:` secret** — landed this commit:
  `loader.str` now records the read error and `Load` refuses to start, instead
  of silently using a blank default that looks like "not configured".
- **Permission hygiene:** `warnIfSecretFileLoose` warns (does not refuse) when a
  `file:` secret is group/other-readable, because the `file:/run/secrets/name`
  convention mounts world-readable inside an isolated namespace. Document the
  0600 expectation in `.env.example` beside the new secret keys.

---

## 8. Existing gaps fixed alongside this design

Two supporting gaps were real in the repo today and are fixed in the same commit
as this doc, since both are correct regardless of Phase 5:

1. **`handlers_profile.go`** — the profile email edit used `!strings.Contains(email, "@")`, weaker than the `looksLikeEmail` used at setup and admin creation, and admitted CR/LF into the stored login identity. Now uses `looksLikeEmail`. (§4)
2. **`config.go`** — `envString` silently fell back to the default when a `file:` secret could not be read (fail-open for a blank credential). Reworked as `loader.str`, which fails the load. Plus `warnIfSecretFileLoose` for permission hygiene. (§7)

---

## 9. What the review refuted (do not re-litigate)

15 findings did not survive adversarial verification — among them: that
notifications leak balances to unauthorised parties (payees already see the tab;
recipients are participants), that the ledger could be corrupted by a send (the
append-only triggers and single write path hold), and various speculative design
flaws with no reachable scenario. If a future reviewer raises one of these,
the burden is a concrete reachable scenario, not a category name.
