# Changelog

All notable changes to BitTabby are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/), and the project uses semantic
versioning. Pre-1.0, the minor version tracks the delivered phase.

The version is defined once, in `internal/version`, shown in the app footer and
in the `/healthz` response, and a build stamps in the commit and date.

## [1.2.2] - 2026-08-07 — Every payee on a tab gets their reminder

### Fixed
- **A tab with more than one payee notified only the first of them.** The
  send-once claim table is keyed `(tab_id, event_key, channel)` with no user
  column, because the recipient was always meant to live inside the event key —
  migration `0008` documents the shape, `req:2026-08-01:u7`. The reminder scan
  built the key without that suffix, so the first payee's claim matched every
  later payee's already-sent check and they were skipped in silence: no error,
  no log line, indistinguishable from nothing being due.

  Fixed by scoping the key to its recipient. No migration: the schema was right
  all along.

  **Upgrade note.** The key format changes, so a reminder already claimed under
  the old format sends one duplicate at the version boundary. This is the
  harmless duplicate the send-then-claim design already tolerates, and it
  happens once.

## [1.2.1] - 2026-08-07 — Notification settings say why email cannot send

### Fixed
- **An SMTP username with no password no longer reports as ready.** Email counts
  as configured on a server address alone, because a server that wants no
  credentials is legitimate. That meant a half-filled credential — a username
  saved through the form with no `BITT_SMTP_PASSWORD` in the environment — showed
  green on the notification settings screen while every send failed
  authentication, with the reason visible only in the container log. It now
  reports as **Misconfigured** and names the variable to set.

  The colour is the exception that proves the existing rule: an unconfigured
  channel stays neutral, because a deployment that has not got there yet is not
  a fault. A setting that looks finished and cannot work is.

### Changed
- **The settings screen explains where credentials come from.** The absence of
  an SMTP password field is deliberate — a secret in the database would need a
  key that has to come from the environment anyway — but the screen did not say
  so anywhere an administrator looking for the field would read it. The username
  field now states it, and the Credentials section carries a worked example of
  setting a variable through Compose and a `.env`, along with the fact that
  credentials are read once at startup and so need a restart, unlike the
  delivery settings, which apply on the next tick.
- **The port field notes the 587 problem.** Many hosting providers block
  outbound 587 and mail services offer an alternative such as 2525; a send that
  times out rather than being rejected is a blocked port, not a bad credential.
- **The credential guidance is accurate about what `.env` does and does not
  buy.** It keeps a secret out of the version-controlled compose file; it does
  not hide the value from `docker inspect`, which shows the container's
  environment whichever route the value took. Keeping it out of the environment
  entirely is what `file:/run/secrets/name` is for. The note also mentions
  `env_file`, and that anything the environment sets becomes read-only here.

## [1.2.0] - 2026-07-26 — Tab administrators; create form keeps your entries

### Added
- **A per-tab administrator role.** Someone can now be attached to a tab as an
  administrator, not only a payee. A tab administrator is a member who helps run
  the tab: they can change its settings, schedule, items, and people, and they
  bill and transact on it (charges, adjustments, payments, undo) the way the
  Provider can — without being the Provider (the single biller set at creation).
  It is distinct from the instance-wide administrator, who may manage any tab but
  is never a party to the money on one they are not a member of. The attach
  control on the tab's people section now offers "as a payee" or "as an
  administrator". Schema migration `0011` widens the participant role.

### Changed
- **The new-tab form keeps what you typed when a field is wrong.** A validation
  error used to send you back to an empty form; now it re-renders in place with
  every entry intact — name, description, kind, loan fields, line items, and a
  valid schedule or fee — and shows the error, so one mistake no longer discards
  a carefully filled form.

### Authorization notes
- Billing actions (charge, adjustment) and undoing any entry now admit the
  Provider **or** a per-tab administrator, both as members; a Payee still only
  records and undoes their own entries, and the instance administrator still
  cannot move money on a tab they are not a member of. Covered by tests across
  the whole role matrix.

### Verified
- Migration `0011` applies and is idempotent on both backends; the `admin` role
  round-trips (the MariaDB path rebuilds the table, since its inline CHECK is
  auto-named `role` — a reserved word MariaDB's `DROP CONSTRAINT`/`DROP CHECK`
  cannot resolve). New tests cover attach-as-admin, a tab admin managing and
  transacting, a payee still being unable to manage or bill, an unknown role
  being refused, and the create form preserving entries on error. Full suite
  green on SQLite and against a real MariaDB.

## [1.1.1] - 2026-07-26 — MariaDB no-op-save 500, and log improvements

### Fixed
- **Re-saving an unchanged tab setting no longer 500s on MariaDB.** Hit in
  production twice — `POST /tabs/1/schedule` and `/tabs/1/fee` both returned
  `store: not found`. Root cause: MySQL/MariaDB's `UPDATE` reports rows *changed*,
  so writing a row's existing values back reports zero affected rows, and the
  store read zero as "no such row" → `ErrNotFound` → 500. SQLite counts a matched
  row as affected, so it never surfaced there. Fixed at the source with
  `clientFoundRows` on the MariaDB connection (report rows *matched*, like
  SQLite), which corrects every one of the ~13 `Set`/`Update` methods at once.
  A new `TestNoOpUpdatesSucceed` re-saves each with identical values and is run
  against a real MariaDB in CI; a dedicated `TestSetScheduleNoOpSucceeds` pins
  the specific case.

### Logging
- **Security logs now record the real client IP behind the proxy.** `clientIP`
  reads `X-Forwarded-For`, but only from a loopback peer (the host's Caddy) and
  only the right-most entry the proxy appended, so a client cannot forge it. A
  direct request still uses its connection address. Before this, every logged
  event behind Caddy showed `127.0.0.1`. Covered by `TestClientIP`, including the
  spoofing cases.
- **Container logs rotate.** `compose.fluidgrid.yaml` sets the json-file driver
  with `max-size`/`max-file`, so `docker logs bittabby` keeps a bounded, useful
  history for diagnosis instead of growing without limit.

## [1.1.0] - 2026-07-25 — Admin can send a test notification

### Added
- **A "Send a test notification" button** on the admin Notifications screen. It
  delivers a fixed test message to the requesting administrator over every
  configured channel their account has a coordinate for — their email, and their
  ntfy topic if they have set one on their profile — so delivery can be confirmed
  end to end without waiting for a real reminder to come due. The button appears
  only once a channel is configured.
- New endpoint `POST /admin/notifications/test` (admin-only, CSRF-checked).

### Notes
- The test is a **pure side effect**: it posts no ledger entry and writes no
  sent-notification claim, so it never touches the balance path and can be run as
  often as needed without affecting or suppressing real reminders. It also
  ignores the admin's own per-channel reminder toggles — a request to test should
  not be silenced by an opt-out from reminders.
- The per-channel result (sent, or a one-line failure reason) is shown to the
  admin. Delivery errors describe the transport and, by the notify package's
  construction, never carry the SMTP password or ntfy token, so surfacing them to
  the administrator debugging their own instance is safe — and the point.

## [1.0.0] - 2026-07-25 — v1

All 54 v1 requirements are delivered, verified, and in use. This is the first
tagged release and the first published image. Nothing in the app changed between
0.8.2 and this tag — 1.0.0 marks the milestone, not new code.

The version convention changes here. Up to now the minor tracked the delivered
phase (pre-1.0). From 1.0.0 on it is ordinary semver: patch for a fix, minor for
a backward-compatible feature, major for a break.

### Added
- **An MIT license.** The README's long-standing "license unset" item is closed.
- **First release.** Tagging `v1.0.0` publishes `ghcr.io/johnzastrow/bitt` for
  linux/amd64 and linux/arm64 with a provenance attestation, via the existing
  release workflow.

### What v1 is
- One authoritative running balance per tab, always derived by summing an
  append-only ledger — never a cached column. Integer USD cents, no float in the
  money path.
- Services and Payoff tabs; recurrence that posts lazily on read with no
  background scheduler; late fees and declining-balance interest under the U.S.
  Rule; corrections as reversing entries; per-period statements.
- Email/ntfy payment reminders, driven by an external `/internal/tick` caller
  that fails closed, kept entirely off the ledger path.
- Argon2id auth with per-tab authorization on every request; a distroless,
  non-root container on SQLite or MariaDB; file-based secrets; and an installable
  shell-only PWA.

### Deployment
- First production deployment target documented and built:
  **`btabby.fluidgrid.site`** on the `recipe.fluidgrid.site` host, on the host's
  MariaDB behind its apt-installed Caddy, matching the pattern of the other sites
  there (`network_mode: host`, Caddy reverse-proxying a loopback port). See
  [docs/planning/DEPLOY-FLUIDGRID.md](docs/planning/DEPLOY-FLUIDGRID.md).

## [0.8.2] - 2026-07-25 — Installable as a PWA (UI-05)

UI-05, the last of the 54 v1 requirements. The app now installs to a phone home
screen and opens to a cached shell. It is **shell-only by design**: the service
worker caches the static frame so the app paints instantly and looks native, but
every data operation still hits the network. There is no offline ledger and no
cached balance — the balance is derived server-side and must never be shown
stale (LEDGER-03), so caching tab data would be a way to show a wrong number.

### Added
- **A web app manifest** (`internal/web/static/manifest.webmanifest`): name,
  `display: standalone`, `start_url: /`, and background/theme colours from the
  chalk palette. Served with `application/manifest+json` (a MIME type Go's table
  does not carry, now registered) and linked from every page's `<head>`.
- **Home-screen icons** rasterised from `logo.svg`: 192px and 512px (`any`), a
  512px `maskable` variant with a generous safe zone so Android does not crop it,
  and a 180px `apple-touch-icon` for iOS, which ignores manifest icons. The one
  new set of binary assets, committed like the vendored htmx.
- **A service worker** (`sw.js`), served from the origin root via a dedicated
  `GET /sw.js` handler so its scope is the whole site — a worker under `/static/`
  would control only `/static/`. It precaches the shell (stylesheet, htmx, the
  scripts, the logo, the offline page, icons) and serves navigations
  **network-first**, falling back to a cached offline page only when the network
  is unreachable. Static assets are cache-first; nothing else is cached.
- **A styled offline page** (`offline.html`) shown when a navigation cannot reach
  the network. It renders from the cached stylesheet, so it looks like the app
  rather than a browser error, and it says plainly why there is nothing to show.
- **Theme-colour meta tags** for light and dark, and iOS web-app meta tags.

### Cache versioning
- The worker derives its cache name from the existing asset digest
  (`web.AssetVersion`), passed to it as `?v=` on the registration URL via a
  `<meta>` tag `sw-register.js` reads. A deploy changes the digest, which changes
  the worker's script URL, which makes the browser install a fresh worker that
  rebuilds the precache and drops the old one. The shell and its cache invalidate
  together — the same mechanism that fixed the 0.5.2 stale-stylesheet bug, not a
  second hand-maintained version string.

### Verified
- Full suite green including `-race`; new `pwa_test.go` pins the manifest, the
  icons, the root-scoped worker headers, the version-tracks-digest link, and a
  guard that the worker never references a tab route.
- Driven headless (Playwright, localhost as a secure context): the worker
  registers at scope `/` and activates, the manifest and all four icons resolve,
  and — taken **offline** — a navigation serves the styled offline page from
  cache while the cached stylesheet still applies. The one console error is the
  pre-existing htmx indicator-stylesheet CSP notice, not new. The home-screen
  install itself was checked by reason, not a real phone, per the standing rule.

### Status
- **v1 is feature-complete: 54 of 54 requirements.** What remains is release
  polish — a milestone audit against the original intent, then tag `v1.0.0`.

## [0.8.1] - 2026-07-24 — MariaDB as a second backend

DEPLOY-03. The store now runs on MariaDB (or MySQL) as well as SQLite, selected
by configuration. SQLite stays the default and the recommendation; MariaDB is
for a deployment that already runs one or wants the database off the app's disk.

### Added
- **`BITT_DB_DRIVER=mariadb` + `BITT_DB_DSN`** select the backend. The DSN
  carries a password, so it accepts the `file:` form and is never logged.
- **One store package, two dialects.** `internal/store/sqlite` became
  `internal/store/sqldb`; the SQL queries are shared and unchanged, and only the
  DDL, the trigger syntax, and error translation sit behind a small `dialect`
  interface. Each backend has its own migration set (`migrations/sqlite/`,
  `migrations/mariadb/`) with matching version stems.
- **The whole store suite runs against both backends** — one suite, exercised
  twice: set `BITT_TEST_MARIADB_DSN` and every existing test runs on a real
  MariaDB, each on its own throwaway database. CI does this in a service
  container. DEPLOY-03's "full test suite green against both backends", met
  literally.
- MariaDB deployment is documented in [DEPLOY.md](docs/DEPLOY.md#choosing-a-database),
  including the one hardening step SQLite cannot offer: `REVOKE UPDATE, DELETE`
  on the ledger tables, so the application role cannot break append-only even if
  a trigger were dropped.

### Fixed (both are SQLite-only assumptions MariaDB surfaced — as the roadmap predicted)
- **The last-admin guard needed real row locking.** On SQLite a single writer
  serializes every transaction, so a check-then-act is atomic by accident. On a
  server that runs transactions in parallel, two people each deactivating a
  different admin both saw the other as still active and both proceeded. The
  guard now locks the active-admin rows (`SELECT ... FOR UPDATE`) in a
  deterministic order, so they serialize without deadlocking. Pinned by a test
  that failed on MariaDB before the fix.
- **`idempotency_key` was too narrow.** The web layer suffixes a 64-hex-char key
  (`...-charge`), so the stored value runs to ~75 characters and overflowed
  `VARCHAR(64)`. MariaDB's `STRICT_ALL_TABLES` — which the app pins deliberately
  — rejected it outright rather than silently truncating, which is the right
  failure: truncating an idempotency key would collapse two distinct keys into
  one and drop a charge. Column widened to `VARCHAR(100)`.

### Security / correctness notes
- MariaDB connections run under **`utf8mb4_bin`** collation and
  **`STRICT_ALL_TABLES`**, both pinned by the app. Binary collation restores the
  byte-for-byte key comparison SQLite does by default — MySQL's case-insensitive
  default would let two keys differing only in case collide, and those keys are
  the double-charge guard.
- The append-only triggers use `SIGNAL SQLSTATE '45000'` with the same message
  text as SQLite's `RAISE(ABORT)`, so error handling is backend-agnostic.
  Verified firing on a live server (error 1644) against a real posted entry.

### Docs
- `docs/DATA-MODEL.md` refreshed to schema `0010`: the three notification tables
  documented, the new `instance`/`users` columns, a MariaDB type-mapping
  section, and the last-admin invariant updated for row locking.

## [0.8.0] - 2026-07-24 — Phase 6 begins: it deploys

Three of Phase 6's five requirements. Someone other than the author can now
stand this up, and the instructions are ones that have been run rather than
written.

### Added
- **A container image** (`Dockerfile`), ~25 MB on
  `gcr.io/distroless/static-debian12:nonroot` — no shell, no package manager,
  no userland. Runs as uid 65532 with `no-new-privileges` and all capabilities
  dropped. No templ step: the generated files are committed, so the image built
  from a tag is the code at that tag. **DEPLOY-05.**
- **`compose.yaml`**, with a named data volume, file-based secrets, and a
  `reminders` sidecar that calls `/internal/tick` hourly — because nothing sends
  without an external caller and that is the step most likely to be missed.
- **GitHub Actions**: CI (vet, test, race, image build, and a check that the
  committed `_templ.go` files are current) and a release workflow publishing
  multi-arch `linux/amd64,linux/arm64` images to ghcr.io on a version tag, with
  a build-provenance attestation and a guard that the tag matches
  `internal/version`. **DEPLOY-05.**
- **`bittabby --healthcheck`**, which probes `/healthz` on loopback and reports
  through its exit code. The image has no curl and no shell to probe with, and
  shipping either to satisfy a `HEALTHCHECK` would put a whole userland into an
  image holding one static binary.
- **[docs/DEPLOY.md](docs/DEPLOY.md)** — quick start, configuration, secrets,
  reminder delivery, backup, restore, and upgrading. **DEPLOY-07.**
- `.env.example` now covers the notification variables, which it had not since
  Phase 5 landed.

### Verified
- **The restore path was exercised, not just written.** Seeded a real tab and
  balance, took a cold backup, destroyed the volume with `down -v`, restored,
  and confirmed the tab and its balance came back. The procedure in DEPLOY.md is
  that run.
- The full compose stack was run end to end: non-root app, non-root sidecar, one
  `0600` secret file readable by both, healthcheck reporting `healthy`, and the
  sidecar authenticating against `/internal/tick`.

### Changed
- **The loose-secret-file warning now considers the directory.** A `0644` file
  inside a `0700` directory is not exposed — no other account can traverse to
  reach it — and warning about it fired on a legitimate container deployment.
  A warning that fires on correct configuration is one operators learn to
  scroll past.
- `docs/REQUIREMENTS.md` numbered the ship phase as 5, from before notifications
  were inserted as Phase 5. It is Phase 6 throughout now, matching the roadmap.

### Note on secrets and ownership
Docker bind-mounts a secret file with the **host's** ownership, so a `0600` file
owned by you is unreadable by a container running as 65532 — the app fails
closed rather than starting without it. `chown 65532` on the secret is what
makes `0600` work on the host and in both containers at once. This is called out
in `compose.yaml`, in DEPLOY.md, and here, because it is the step people will
"fix" back.

## [0.7.4] - 2026-07-24 — Notification settings in the app, secrets left out of it

Instance-wide notification config was environment-only, which meant a shell in a
container to reword a reminder. The non-secret half now has a screen. The secret
half deliberately does not.

### Added
- **An admin Notifications screen** at `/admin/notifications`, linked in the top
  bar for administrators. Three cards: a status readout of what can actually
  deliver, the delivery settings, and the default reminder set (with the same
  byte counters as the per-tab card).
- **Editable in the app:** SMTP server, port, username, from address; the ntfy
  server URL; and the default reminder days, title, and message. Migration
  `0010` adds the delivery columns to `instance` and an `instance_reminders`
  table. Changes take effect on the next tick with no restart — the effective
  config is resolved per use rather than held from startup.

### Security
- **Secrets stay in the environment.** `BITT_SMTP_PASSWORD`, `BITT_NTFY_TOKEN`,
  and `BITT_TICK_SECRET` have no columns, no form fields, and no code path that
  could store one. The screen reports each as set or not set and never renders a
  value, not even masked — a masked field still leaks the length. The reasoning
  is in migration `0010`: a secret in the database has to be encrypted, its key
  has to come from the environment anyway, so the result is the same number of
  environment secrets plus a key to manage and re-wrap — and it would put live
  credentials into every backup taken under DEPLOY-07.
- **The environment wins over stored delivery settings**, field by field, so a
  container behaves the same way every time it starts whatever is in its volume.
  Environment-owned fields render read-only rather than hidden, and saving the
  form does not capture their values into the database — otherwise they would
  silently take over the day the variable was unset. Both are pinned by tests.
- Delivery input is validated to the same rules the environment is held to: a
  from address parsed by `net/mail` (which refuses a newline, closing the header
  injection), a hostname that is not a URL, and **https-only for ntfy**, since a
  plaintext push carries the tab name and the amount owed.
- Reminders resolve tab → instance → environment; delivery resolves environment
  → instance. The two run opposite ways on purpose: a message is content, where
  the most specific author should win, and a delivery setting is deployment,
  where the thing under version control should win.

## [0.7.3] - 2026-07-24 — Per-tab reminders, set by the Provider

Completes the notification model Phase 5 set out to deliver. 0.7.0–0.7.1 shipped
instance-wide reminder defaults from the environment; those were always the
fallback layer. The Provider owns a tab's billing cadence, so the Provider now
says when its payees hear about it and what the message says.

### Added
- **A Reminders card on every tab**, in the Setup group, Provider-only like the
  schedule, late-fee, and interest cards. Lead times as a comma-separated list
  ("21, 7, 1", at most six), plus the title and message. Migration `0009` adds
  `tab_reminders`; a tab with no rows falls back to the instance defaults, and
  clearing the days field is how it goes back to them.
- **A customised tab is customised completely** — its list replaces the instance
  one rather than merging with it. Merging would make removal impossible: a
  Provider who dropped the 14-day notice would keep receiving it from the
  instance default. `Server.reminderForTab` is where that resolves, and a read
  failure sends nothing rather than falling back to a message the Provider may
  have deliberately replaced.
- **Live byte counters** on the title and message fields, with a caution band.
  Sizes are counted in BYTES, not characters, because that is what the limit is:
  ntfy.sh refuses a message over 4,096 bytes on a free account rather than
  shortening it, so an emoji costs four and an accented tab name costs two. The
  message ceiling is now that same 4,096 rather than an invented smaller number,
  and the caution starts at 90% to leave room for the variables to expand. The
  server renders the count too, so it is right with scripting off.

### Security
- These templates are **the first user-controlled text to reach a mail header**
  (via `{tab}` in a Subject), a real change from admin-only env config. Two
  layers now: `notify.ValidTitleTemplate` / `ValidBodyTemplate` refuse control
  characters at input, and `internal/notify` still rejects them at send time.
  Validating at input is not redundant — it is what stops a saved template from
  failing every one of a tab's reminders closed, days later, with nothing on
  screen to explain why.
- `tab_reminders` carries no append-only triggers, deliberately: it is
  configuration, not a claim. Editing a reminder cannot re-send anything, since
  `sent_notifications` is keyed on (tab, event, channel) and the event key
  carries the due date and lead time, never the message text.

### Fixed
- A `<textarea>` submits CRLF line endings per the HTML form spec, which the
  first version of the validator refused as control characters — making every
  multi-line message written in a browser impossible to save. Found by a
  Playwright pass after the Go tests, which post LF directly, had all passed.
  CRLF is now normalised before validation, and a test pins it.

## [0.7.2] - 2026-07-24 — Running balances and the projected payment schedule

### Added
- **A balance column in the tab history.** Every entry now shows the tab balance
  as it stood immediately after it, so the history reads like a bank statement.
  Rows run newest first, and the top row is the balance in the tab's header by
  construction -- the column is walked back from that figure, so the two cannot
  disagree.
- **A collapsible Payments table on Payoff tabs**, projecting every payment
  still to come: payment date, amount, and the balance left owed after it, down
  to zero. Its folded summary carries the count, the total cost to finish, and
  the projected payoff date. `loan.Project` runs the same per-period arithmetic
  as `loan.Simulate` -- U.S. Rule allocation, interest rounded once per period
  -- and a test pins the two together, so the schedule agrees with what the
  ledger will actually post. Nothing is stored; it is derived on each render
  from the entries that produce the balance, and it is empty whenever there is
  nothing honest to project (no schedule, no expected payment, already settled,
  or a payment too small to ever retire the loan -- that last is the true-up
  banner's business).

### Changed
- **The built-in reminder message now uses every template variable**, so the
  shipped default doubles as the worked example an administrator edits from.
  Title: `{tab}: {amount} due {when}`. Body: `Your {days}-day reminder: a
  payment on the tab "{tab}" is due {when}, on {due}.` / `{amount} is owed.` /
  `{url}`. Overriding it works exactly as before via `BITT_REMINDER_TITLE` /
  `BITT_REMINDER_BODY` and their per-lead-time variants.

## [0.7.1] - 2026-07-24 — Configurable reminder days and messages

### Added
- **Reminder lead times are configurable** via `BITT_REMINDER_DAYS` (e.g.
  "14,7,1"), defaulting to the built-in 14/7/1. Deduped and validated.
- **Reminder messages are configurable templates** with variables filled per
  send: `{tab}`, `{amount}`, `{due}`, `{days}`, `{when}`, `{url}` (a link to the
  tab's payment page, from `BITT_BASE_URL`). Set a default via
  `BITT_REMINDER_TITLE` / `BITT_REMINDER_BODY`, or override one lead time with
  `BITT_REMINDER_TITLE_<d>` / `BITT_REMINDER_BODY_<d>`. Templates are admin
  (env) text; a `{tab}` value with a control character still fails the send
  closed via the sender's header check.

### Note
- These are INSTANCE-WIDE defaults. Per-tab, provider-set reminders are the
  intended next step (see HANDOFF.md) and will override these defaults.

## [0.7.0] - 2026-07-24 — Phase 5: notifications

Payment reminders reach people who do not have the app open, over email and
ntfy, driven by an external cron. Built to the pre-implementation security
design (docs/SECURITY-PHASE5.md); every control there is in place, and the whole
feature stays off the balance path -- a failed or double send can never touch a
ledger.

### Added
- **Email and ntfy delivery** of payment reminders at 14, 7, and 1 days before a
  due date, to the payees on a tab. `internal/notify` is the delivery package
  and the security surface: it rejects (never strips) control characters in
  every header value, keeps all user text -- tab names, memos -- in the message
  body, refuses to follow redirects, and bounds each send with a timeout.
- **`POST /internal/tick`**, the cron entry point. Outside `requireAuth` (a cron
  has no session), authenticated by a shared secret in an `Authorization: Bearer`
  header, constant-time compared, **failing closed when unset**, checked before
  any work, and rate-limited. The scan it runs is read-only with respect to the
  ledger.
- **The sent-notifications claim table** (migration 0008), send-then-claim
  (at-least-once): deliver first, claim only on confirmed success, so a transient
  failure re-sends rather than dropping the notice. Before sending, live state is
  re-derived -- a paid-early or settled tab is never dunned.
- **Notification preferences** on the profile: per-channel email/ntfy toggles and
  an ntfy topic, validated to the same strict charset the sender enforces. The
  section that was deferred from 0.6.0 now that there is delivery behind it.
- **Config**: SMTP settings, an admin-pinned `BITT_NTFY_URL` (users choose only a
  topic -- the SSRF decision), `BITT_TICK_SECRET`, and `BITT_BASE_URL` for links.
  All via `file:`-capable, fail-closed config loading.

### Security notes
- ntfy SSRF policy is tuned for a self-hosted container: the ntfy host is admin
  config, so LAN/private addresses (a sidecar or LAN ntfy box) are allowed, while
  loopback and link-local -- the cloud metadata endpoint -- are refused, checked
  at dial time to defeat DNS rebinding.
- The tick endpoint never provokes ledger accrual, so an outbound-triggered,
  cron-reachable endpoint cannot become a balance-path write.

### Still open (documented for a follow-up)
- Payment-made / payment-missed event notices (only the pre-due reminders ship).
- A backlog cap and per-recipient failure bounding on the scan.

## [0.6.4] - 2026-07-24 — Pre-Phase-5 security design and hardening

Phase 5 (notifications) is the app's first outbound side effect and first reach
to external hosts, so a security design review was run before any code: a
multi-agent threat model (5 reviewers by lens, every finding adversarially
verified, then synthesis). 34 findings raised, 15 refuted, 19 survived. Net risk
is low-to-medium and entirely off the balance path.

### Added
- **`docs/SECURITY-PHASE5.md`** — the pre-implementation security design the
  phase is gated on. Captures the must-fix controls (stale-reminder suppression,
  SSRF containment on ntfy, email/ntfy header injection, `/internal/tick` auth
  and its balance-path hazard, delivery semantics, secret hygiene) and the three
  decisions the user must make first: the ntfy SSRF policy, at-most-once vs
  at-least-once delivery, and the tick-auth mechanism.

### Fixed (in-repo gaps the review confirmed, both correct regardless of Phase 5)
- **The profile email edit admitted control characters.** It checked only for an
  `@`, weaker than the `looksLikeEmail` used at setup and admin creation, so a
  CR/LF could enter the stored login identity — header injection waiting for a
  mail sender to land. Now uses `looksLikeEmail`.
- **An unreadable `file:` secret silently fell back to a blank default.**
  `config` reworked so a `file:`-supplied value that cannot be read fails the
  load instead of degrading to empty (fail-closed, which the Phase 5 secrets and
  the tick endpoint's own fail-closed guard depend on). Added a permission
  warning when a `file:` secret is group/other-readable.

### Tests
- `internal/config` gains tests (0% -> ~69%): fail-closed on an unreadable
  secret file, reading a secret file, defaults, and bad-timezone rejection.
- `internal/auth` session and CSRF managers gain direct unit tests (42% -> ~90%):
  session round-trip and digest-only storage, fail-closed resolution, expiry,
  Revoke vs RevokeOthers, CSRF double-submit and its rejections, DummyVerify.

## [0.6.3] - 2026-07-24 — Avatars in history rows

### Changed
- **The ledger history now shows who recorded each entry with a face**, not only
  a name, completing the avatar treatment across every place a person appears.
  The actor lookup already loaded the full user, so no extra query was added.
- `initials` was hardened to take the first *letter* of each word, skipping
  punctuation, so a removed account renders "R" rather than "(" and CJK names
  keep their first character. Covered by a unit test.

## [0.6.2] - 2026-07-24 — Avatars everywhere a person is named

### Changed
- **A person's picture now shows wherever they are named**, not only in their
  own header: the tab participants list and the admin users list. Uploading a
  picture and seeing it in one place but nowhere else read as half-finished.
  People without a picture keep their initials fallback, so every slot renders
  something.
- The `AvatarImage` component was generalised to `Avatar(id, name, timestamp)`
  from its old `store.User` argument, so anything that names a person can render
  a face; a `UserAvatar` wrapper keeps the common User case tidy.
- `store.Participant` now carries `AvatarUpdatedAt`, populated by the existing
  `ListParticipants` join, so a participant row shows a picture without a
  second query per row.

## [0.6.1] - 2026-07-24 — Settle buttons pay a period, not the whole balance

### Changed
- **The dashboard's primary settle button now offers one period's payment**, not
  the entire outstanding balance. On a Payoff loan that means the scheduled
  installment (e.g. "Pay $505.65") rather than "Settle $21,966.00"; on a
  Services tab it means one period's charge rather than every period that has
  accrued. Paying the period is the ordinary act, so it is the one-tap default;
  clearing a whole loan is the exception and lives behind "Other amount", which
  still prefills the full balance. The same default now prefills the tab page's
  payment field, so a full loan payoff is a deliberate edit rather than an
  accidental tap.
- The button reads "Pay $X" for a period payment and "Settle $X" when that
  amount clears the whole balance, since the two are different acts. When less
  than a period remains it settles what is left rather than overshooting an
  installment into credit. A hand-billed tab with no periodic amount keeps the
  settle-the-balance button, since there is no period to offer.

### Notes
- Verified with Playwright against the layout from the reported screenshot: the
  loan card offers "Pay $505.65" beside "Other amount".

## [0.6.0] - 2026-07-23 — Account profiles

Click your own name in the header to reach `/profile`.

### Added
- **Profile settings**: display name, email, password, and a picture, all
  self-service. Reached by clicking your name in the header, which is where
  people look for them.
- **Avatars.** Upload a PNG, JPEG, or GIF up to 2 MB; it is cropped square,
  scaled to at most 256px, and re-encoded as PNG. **What is stored is never what
  was uploaded** — decoding and re-encoding strips EXIF and GPS, discards data
  appended after the image, and means a file crafted to parse as two formats at
  once survives as neither. The filename is never read or stored, which makes
  path traversal structurally impossible rather than merely handled.
  Accounts without a picture get initials on a colour derived from their id, so
  every avatar slot renders something rather than a broken-image icon.
- **`internal/avatar`**, the whole upload surface in one package: a byte cap
  applied while reading, a magic-byte allowlist that ignores the declared
  content type, and **a pixel-dimension check taken from the header before
  decoding** — the control a size cap cannot provide, since a 30,000 x 30,000
  PNG compresses to a few kilobytes and expands to gigabytes.
- **Rate limiting on the upload route**, the first in the app. Image decoding is
  the only meaningful CPU work a request can trigger here, and an authenticated
  user can ask for it in a loop; ten uploads a minute per account, a fixed
  window rather than a general framework for one endpoint.
- **Migration 0007** adds `users.avatar_png` and `users.avatar_updated_at`. The
  timestamp is the ETag on the avatar route, so a browser revalidates cheaply
  and a changed picture still appears at once.

### Changed
- **Changing your password now signs out every other device**, keeping the one
  making the change. That is usually the point of changing it. It requires the
  current password, refuses a password identical to the current one, and
  confirms how many devices were signed out.
- **Changing your email requires your current password**, because the email is
  the login identity: an unattended session that could silently move the address
  elsewhere is a full account takeover, since recovery would follow the new
  address.

### Fixed
- **`GetSession` did not select `avatar_updated_at`.** It joins `users` with its
  own hand-written column list rather than reusing `userColumns`, so the
  authenticated user on every request carried a zero value and the header always
  fell back to initials even after an upload. Caught by a test; the hazard is now
  recorded in the data model doc, since any future `User` column has the same
  trap waiting.

### Deferred
- **Notification preferences.** They belong on this page but there is no
  delivery to configure yet, and switches that do nothing are worse than no
  switches. They land with Phase 5, where the per-event list is already designed.

## [0.5.2] - 2026-07-23 — Create-form layout, and assets that actually update

### Fixed
- **Static assets were cached for an hour behind a stable URL.** The reasoning
  in `static.go` was that replacing the binary made a long `max-age` safe; it
  does not, because it does nothing to a stylesheet a browser has already
  cached. The 0.5.1 kind-scoped fields therefore did not appear at all for
  anyone who had loaded the app in the previous hour, and the feature looked
  broken rather than stale. Asset URLs now carry a digest of the embedded
  content (`/static/app.css?v=…`), so a change to the stylesheet changes the
  URL. A request carrying the current digest is cached for a year and marked
  immutable; anything else gets 60 seconds, so a stale copy heals quickly.
  The digest hashes content rather than the version constant, since the version
  does not change between development builds but the stylesheet does.
- **`input[type="number"]` and `input[type="date"]` were never styled.** They
  fell outside the selector list and rendered at browser defaults — wrong width,
  no padding, no border radius — beside styled text fields. Visible as soon as
  0.5.1 added two number inputs, but the date input had been wrong since the
  schedule form shipped. A test now walks the rendered forms and fails if any
  input type in use is missing from the stylesheet.
- **The create form was cramped.** Bordered fieldsets nested inside a bordered
  card inside a 26rem column stacked three sets of padding. The kind-scoped
  groups are now plain sections with a small heading, and the form has its own
  34rem width rather than borrowing the login box's.
- **The kind radio sat above its own label text** instead of beside it:
  `.stack label` sets `flex-direction: column`, which won because the new rule
  never declared a direction.

### Notes
- Verified with Playwright against a throwaway instance: the Payoff and Services
  halves show and hide correctly, and the text, number, and date inputs now
  share a width and left edge.

## [0.5.1] - 2026-07-23 — Timezone picker and kind-scoped tab fields

### Added
- **Timezone autocomplete on the setup screen.** The field was free text, which
  asked people to recall an IANA name exactly. It now offers 410 zones through a
  `<datalist>`, so typing "York" finds `America/New_York` — a native `<select>`
  keyboard-searches only from the start of the value and would never find it.
  Common zones are listed first; the rest are alphabetical. The form now
  proposes `America/New_York` rather than `UTC`; `BITT_TIMEZONE` still overrides
  it, and UTC remains the *fallback* when a stored zone fails to load, because a
  broken value should degrade to something neutral rather than to a guess that
  shifts every boundary five hours.
- **`internal/tz`**, holding the embedded zone list. Go exposes no way to
  enumerate the zones inside `time/tzdata`, and reading `/usr/share/zoneinfo` at
  runtime would give a full list in development and an empty one in a scratch
  container — the exact case the embedded tzdata exists for. The list is
  generated and committed, the same arrangement as the vendored htmx asset, and
  every name is filtered through `time.LoadLocation` before being offered.

### Changed
- **The create form shows only the fields belonging to the chosen tab kind.**
  Loan amount, interest, term, and payment appear for Payoff; line items for
  Services. The kind is now a radio group, and the stylesheet hides the
  irrelevant half via `:has()` on the checked radio — no JavaScript, which the
  content-security policy forbids inline, and no round trip. Where `:has()` is
  unsupported every field shows, which is the previous behaviour.
  Nothing kind-scoped is `required`, since a hidden required field blocks
  submission with a message the browser cannot display; validation stays
  server-side, by kind, as it already was.

### Fixed
- **The binary accepted no arguments and silently ignored them**, so
  `bittabby --version` looked like a question and was answered by starting a
  server against `BITT_DB_PATH` — which on one occasion applied a forward-only
  migration to a database that was only meant to be inspected. `--version` and
  `--help` now answer without touching the database, and anything unrecognized
  exits 2 with usage.

### Notes
- The timezone list is a convenience, never the authority. A zone the running
  tzdata can load is accepted whether or not the committed list has caught up,
  and an unknown one is still rejected by `time.LoadLocation`.

## [0.5.0] - 2026-07-23 — Loan terms, scheduled payments, and schedule intervals

A Payoff loan can now state how long it runs and what it costs per period, and
the app works out that payment itself so a lender's figure can be checked
against it. Verified against an issued amortization schedule: $21,852.48 at
5.24% over 48 monthly payments, quoted at $505.65 and computed here at $505.63 —
a two-cent difference that is the lender's own rounding.

### Added
- **Loan term and scheduled payment** on Payoff tabs, each in its own field. The
  Provider enters the payment the lender actually charges, because that is the
  figure that has to be paid; the app computes what the terms imply and shows
  the two side by side rather than overriding one with the other.
- **A suggested payment**, derived by simulating the same arithmetic the ledger
  posts — the same per-period rounding, the same allocation — rather than by the
  closed-form annuity formula. The closed form needs a float, which is forbidden
  in the money path, and disagrees with integer per-period rounding by a few
  cents over a long term. The simulation cannot: it is the same arithmetic.
  Reports the final payment (smaller, as lenders adjust it) and lifetime interest.
- **A true-up**, recasting the required payment against the balance and the
  payments still remaining, and saying when the current payment will not clear
  the loan in time or will clear it early. Differences within five cents are not
  flagged, since that is a lender's rounding rather than a disagreement.
- **`internal/loan`**, a fourth pure package beside money, schedule, and fee.
  No I/O, no clock. Pinned by tests against both the closed-form annuity value
  and an issued lender amortization schedule.
- **Arbitrary schedule intervals** — every N weeks, every N months. "Every three
  weeks" was previously unrepresentable.
- **Maturity date and payment count** on the loan progress panel.
- **Adjustments in the interface** (CHG-03). The Provider can correct what is
  owed in either direction, with a required reason, from Day to day → "Adjust
  what is owed". `ledger.Adjustment` had existed since Phase 1 and was reachable
  from no route, which left a reconciliation with no honest home: the only way
  to reduce a balance was to record a payment that never happened, which claims
  money changed hands, satisfies the period's late-fee window, and on a loan is
  allocated to interest before principal. **A credit is allocated
  principal-first**, deliberately unlike a payment — a payment settles the
  oldest thing owed, while a credit is the Provider deciding part of the debt
  should not exist, and taking it off principal makes the reduction permanent
  and every later interest charge smaller. Credits count toward meeting the
  schedule, so forgiving what was expected clears "Behind"; they are excluded
  from payment totals, since an adjustment is not money.
- **`docs/DATA-MODEL.md`** — the physical schema with a mermaid ER diagram, a
  per-column data dictionary with rules, the database invariants, and the loan
  arithmetic conventions in one place.

### Changed
- **Interest no longer compounds on unpaid interest.** Interest now accrues on
  outstanding principal only; interest a short payment did not cover is owed,
  and is repaid before principal, but does not itself accrue. This is the U.S.
  Rule, which Regulation Z permits for closed-end credit and which consumer
  lenders run. The previous behaviour capitalized it, which meant a borrower who
  missed a payment and caught up would separate permanently from what their bank
  said — with no way back. The old rule was documented but never tested; it is
  now tested in both `internal/loan` and `internal/ledger`.
- **The per-period interest rate is now an exact fraction of a year** rather than
  APR divided by a periods-per-year count, which had no answer for an interval
  like three weeks (52/3 is not an integer). Month-stepping schedules use
  months/12, the 30/360 basis a US installment loan is quoted on, so a plain
  monthly loan is still exactly APR/12. Week-stepping schedules use days/365,
  actual/365. **Weekly and biweekly tabs carrying interest will see a very
  slightly different figure on future periods** (APR × 7/365 rather than APR/52);
  interest already posted is immutable and stands.
- **A Payoff tab's expected payment comes from its own field, not its line
  items.** Items are a Services tab's period charge. One field meaning both "what
  is charged" and "what is expected" is what let a mis-configured loan read as
  already settled. Payoff tabs no longer offer the line-item editor; existing
  items are shown, read-only, rather than deleted.
- **`biweekly` is now `weekly` with an interval of 2.** Both compute period n as
  anchor + 14n, so the rewrite is date-identical: no period key moves and no
  posted cycle can re-bill. Verified against the demo database.

### Fixed
- **The true-up flagged harmless overpayments.** Paying more than the term
  strictly requires is now only reported when it actually shortens the loan.
  A small credit, or a lender's payment rounded up, previously produced a notice
  reading "48 payments rather than 48".
- **Payoff progress and late fees stopped short on an interest-bearing loan.**
  Both capped expectations at the principal, which is less than the loan costs
  once interest is charged — so the schedule stopped expecting payments partway
  through, a borrower paying exactly on time could read as behind, and the last
  months of a term went unfined. Expectations now span the term.

### Migration
`0006_loan_terms` adds `schedule_interval`, `loan_term_periods`, and
`loan_payment_cents`; rewrites `biweekly` schedules; and backfills each Payoff
tab's payment from the sum of its active line items — exactly what the old code
read, so no tab's expectations change on upgrade. **A Payoff tab that was set up
with the loan amount in a line item will backfill that amount as its payment**
and should be corrected in Setup → Loan term and payment.

## [0.4.0] - 2026-07-23 — Payoff tabs and late fees

### Added
- **Payoff loans** (TAB-02, PAYOFF-01/02/03). A loan is a principal charged once
  and drawn down by payments; the schedule describes expected payment dates. A
  loan shows its remaining balance, progress against the original principal, and
  on-track / ahead / behind status, and drops off the active dashboard when
  settled. Payoff tabs post no scheduled charges.
- **Late fees** (FEE-01 through 07). Fixed or percentage, with a grace period and
  an optional cap, per tab. A fee is assessed once per missed period, judged in
  that period's own payment window, and never compounds — a percentage is taken
  on the period charge, not on a balance containing fees. Fees accrue lazily on
  read through a `posted_fees` claim table, exactly once even under concurrent
  reads. A fee can be waived, which records a reversing entry with a reason.
- **Declining-balance interest** on loans (by request, beyond the requirement
  list). Interest accrues each period on the loan's outstanding balance, so it
  falls as the loan is paid down and compounds on unpaid interest. Posted lazily
  like fees, one claim per period.
- **In-app upcoming-payment notice**, shown within two weeks of a due date.
- **Version display**: an `internal/version` package, shown in the footer (with
  the full build string on hover) and in `/healthz`.
- **Logo** in the header, linking home.

### Changed
- Settling from the dashboard now accepts any amount, not only the full balance.
- A tab's name, description, and kind are editable after creation; tabs can be
  archived; participants can be removed. A global administrator may manage the
  settings of any tab (an amendment to AUTH-05, logged), but may not move money
  on a tab they do not participate in.

## [0.3.0] - 2026-07-23 — Recurrence

### Added
- Recurring schedules (weekly, biweekly, monthly-on-day, monthly-last), billed in
  advance or in arrears (SCHED-01/02/05). Due periods post lazily inside the read
  transaction — no background scheduler — and catch up whenever a tab is opened
  (SCHED-03), exactly once even under concurrent reads (SCHED-04).
- Per-period statements computed from ledger entries (CHG-01/04); item changes
  take effect the following period and never alter posted entries (CHG-02).

## [0.2.0] - 2026-07-23 — The settle loop

### Added
- Payments, one-tap settle from the dashboard, payment on another's behalf, undo
  as a reversing entry, and pay-ahead credit (PAY-01 through 05, LEDGER-02/07).
- A second participant and per-tab authorization (TAB-03, AUTH-04/05).

## [0.1.0] - 2026-07-23 — Walking skeleton

### Added
- A single static binary: append-only ledger, integer-cent money, first-run
  admin setup, tabs with line items, balances derived by summing entries, and a
  mobile-first interface (LEDGER, TAB, AUTH, DEPLOY, UI foundations).

[0.4.0]: #040---2026-07-23--payoff-tabs-and-late-fees
[0.3.0]: #030---2026-07-23--recurrence
[0.2.0]: #020---2026-07-23--the-settle-loop
[0.1.0]: #010---2026-07-23--walking-skeleton
