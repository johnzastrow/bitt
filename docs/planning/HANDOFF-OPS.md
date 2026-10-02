# Handoff — post-v1, deployed and operating

Written 2026-07-27, updated 2026-10-01 through v1.6.0, to be read cold. v1
shipped; the app is **live in production** and the work since has been small
releases, notifications follow-ups (1.3.0 to 1.6.0, August), deep test coverage,
and running the deployment. Nothing is in progress. The project's shape, ledger invariants, and earlier
traps are in [HANDOFF.md](HANDOFF.md); this file is the current delta and the one
to start from.

---

## Current state in one paragraph

BitTabby is at **v1.6.0**, public at github.com/johnzastrow/bitt (MIT), imaged at
`ghcr.io/johnzastrow/bitt` (**amd64 only**), and **live at
https://btabby.fluidgrid.site** on the `recipe.fluidgrid.site` host. It runs as a
Docker Compose stack on the host's MariaDB, `network_mode: host`, behind the
host's apt-installed Caddy. Working tree clean; everything pushed. Test suite
green on SQLite (2026-10-01); the MariaDB suite was last run with the August
releases.

**Verified 2026-10-01:** tag `v1.6.0` = commit `3796f6d`; the Release run built
it; GHCR `bitt:1.6.0` is index `sha256:27ffbe1d…` (amd64 manifest
`sha256:f3ab3e05…`); the host's `bittabby` container runs that index, healthy;
the host's compose pins 1.6.0. **Drift:** the repo's `compose.fluidgrid.yaml`
still pins 1.2.0 (see Open threads, item 0).

---

## The deployment (how it actually runs)

- **Host:** `recipe.fluidgrid.site` (`45.56.117.76`, Linode, amd64). SSH as
  `jcz@recipe.fluidgrid.site`. **sudo needs a password we do not have** — see
  the Caddy trap below for the consequence.
- **App:** `~/bittdocker/` holds `compose.fluidgrid.yaml` + a chmod-600 `.env`
  (holds `BITT_DB_DSN` and `BITT_TICK_SECRET`; never committed). Container binds
  `127.0.0.1:8091`. A `reminders` sidecar POSTs `/internal/tick` hourly.
- **DB:** host MariaDB, database `btabby`, user `btabby`@127.0.0.1/localhost.
  Root password is in `~/actadocker/.env` as `DB_ROOT_PASSWORD` (that is how DB
  admin is done, since sudo mysql needs a password).
- **Caddy:** host `/etc/caddy/Caddyfile`. `btabby.fluidgrid.site` reverse-proxies
  `127.0.0.1:8091`. Unknown hosts over HTTP return 404 (`:80` block).
- **DNS:** `*.fluidgrid.site` wildcard → the host, at Linode.

Full runbook (DB/user/grants, `.env`, Caddy block, backup/restore) is in
[DEPLOY-FLUIDGRID.md](DEPLOY-FLUIDGRID.md). More host detail and gotchas are in
the memory file `bitt-production-deployment`.

### Redeploy runbook (a new release)

1. Bump `internal/version` and add a CHANGELOG entry; commit; `git tag vX.Y.Z`
   and push the tag — the Release workflow builds and pushes the amd64 image.
2. Bump the pinned tag in the host's `~/bittdocker/compose.fluidgrid.yaml` in
   place (sed the `image:` line), and the same line in the repo copy; **commit
   it**. The repo copy was synced from the host on 2026-10-02 and is identical
   except for `BITT_SMTP_USERNAME`, which is a placeholder in the repo so the
   real login is not published. Never copy the repo file over the host's: that
   line would replace the real login.
3. **If the release includes a migration**, back up first:
   `RP=$(grep -m1 ^DB_ROOT_PASSWORD= ~/actadocker/.env | cut -d= -f2-)` then
   `mysqldump -uroot -p"$RP" --single-transaction --routines --triggers btabby > ~/bittdocker/backups/btabby-pre-X.Y.Z.sql`
4. `scp compose.fluidgrid.yaml jcz@recipe...:~/bittdocker/` then on the host
   `cd ~/bittdocker && docker compose -f compose.fluidgrid.yaml pull bittabby && docker compose -f compose.fluidgrid.yaml up -d`.
5. Verify: `curl -s https://btabby.fluidgrid.site/healthz` → `ok vX.Y.Z`, and the
   container is healthy.

---

## Traps learned since v1 (do not relearn these the hard way)

- **MariaDB counts rows *changed*, not *matched*.** An UPDATE that writes a
  row's existing values back returns `RowsAffected()==0`, and the store read 0
  as `ErrNotFound` → a 500 when a provider re-saved an unchanged tab setting.
  Fixed globally with `clientFoundRows` on the MariaDB DSN. Guard:
  `TestNoOpUpdatesSucceed`. **Any new `Set*`/`Update*` store method keyed on an
  id inherits this only because of that flag — keep it.**
- **`role` is a reserved word AND the inline check's auto-name on MariaDB.**
  Migration 0011 could not `DROP CONSTRAINT role` / `DROP CHECK role` (1091 /
  1064), so the MariaDB path **rebuilds the table** to widen the role CHECK.
  Lesson: to change an inline column CHECK on MariaDB, rebuild the table; do not
  fight the constraint name.
- **On-demand-TLS catch-all took the site down (reverted).** A `*.fluidgrid.site`
  wildcard site with `tls { on_demand }` conflicts with the explicit per-site
  blocks and broke TLS for **every** site. Recovered instantly via the Caddy
  **admin API** (`localhost:2019/load`) — a transactional reload that keeps the
  old config on failure and needs no sudo. The clean way to get HTTPS 404s for
  typo'd subdomains is a wildcard cert via **DNS-01 + the Linode DNS plugin**
  (custom Caddy build), not on-demand. We chose to leave it: unknown HTTPS hosts
  just fail TLS, which is normal.
- **No sudo on the host.** Cannot edit `/etc/caddy/Caddyfile`, `systemctl reload
  caddy`, or `sudo chown 65532`. Consequences baked into the design: secrets go
  in a chmod-600 `.env` (not file-based Docker secrets), and live Caddy changes
  go through the admin API while the operator persists the file edit. Hand the
  user one-liners for anything needing sudo.
- **A schema CHECK can silently undo a parser change (1.4.1).** Overdue notices
  use negative lead times; the parser accepted them in 1.3.0, but both reminder
  tables still had `CHECK (days > 0 ...)`, so saved rules could never include
  overdue. It looked fine because built-in defaults live in code. Lesson: test
  the round trip through storage, not only the pure function. Migration `0012`
  rebuilt both tables (the MariaDB CHECK lesson above applies).
- **Template variables keep their meaning forever (1.3.0, 1.4.0).** `{amount}` is
  the tab balance, and Providers have saved templates that rely on it; on a
  Payoff tab that quoted a whole car loan as "due tomorrow". The fix added
  `{payment}` (the installment) rather than redefining `{amount}`. Add new
  variables; never change an existing one's meaning.
- **Notices are derived from the ledger, not queued (1.3.0).** The tick treats
  each unannounced payment or missed period as an event; a claim is written only
  after confirmed delivery; the per-tick ceiling (`BITT_NOTIFY_MAX_PER_TICK`) is
  spent only after the already-sent check, or a backlog starves new events. Keep
  delivery off the ledger write path.
- **Do not rely on the mail relay's manners (1.3.1).** SMTP2GO added the missing
  `Date` and `Message-ID`, which hid the bug. The app now sets both.
- **Redirect after login is an allowlist (1.6.0).** Only same-site paths; `//host`,
  `/\host`, schemes and unparseable values are refused. Keep it that way: an open
  redirect would make every notification a phishing link.
- **Logs behind the proxy:** `clientIP` trusts `X-Forwarded-For` only from a
  loopback peer and takes the right-most entry (unforgeable). Container logs
  rotate via the compose json-file driver. `docker logs bittabby` is where
  errors and events land.

---

## Working agreement carried forward

- **Add deep tests with every functional change** (user directive, memory
  `bitt-deep-tests-with-changes`). Exercise **both** backends for store changes:
  a local MariaDB for tests — `docker start bittmaria` (recipe in
  HANDOFF-PHASE6.md), DSN `bitt:p@tcp(127.0.0.1:13306)/bitt_test`, then
  `BITT_TEST_MARIADB_DSN=... go test ./internal/store/sqldb/`.
- Security-relevant code gets the adversarial cases (injection, spoofing, SSRF).
- `make check` before committing; commit messages reference the change; tag +
  push for a release.

---

## Coverage (SQLite run, 2026-10-01)

Strong: fee 96%, tz 96%, money 96%, schedule 93%, loan 90%, auth 90%, avatar 88%,
ledger 87%, notify 82%, version 100%. Moderate: web 76%, config 71%,
store/sqldb 68% (higher under MariaDB), store 48%.

**Untested and deliberately so:** `cmd/bittabby` main/run (process wiring,
exercised live), `web/views` (templ-generated, covered through handler tests),
and MariaDB-dialect functions that read 0% on a SQLite-only run but are covered
under the MariaDB suite.

**Next coverage targets if resumed:** remaining `web` handler error-paths, and
`config`/`ledger` internal helpers (`activeItems`, `flatPayments`).

---

## Open threads (nothing blocking; pick up any)

0. **Commit the compose pin (do first).** Diff the host's
   `~/bittdocker/compose.fluidgrid.yaml` (pins 1.6.0) against the repo's (1.2.0),
   commit the host's version. Also update PROJECT.md, whose Out of Scope still
   lists notifications as deferred.
1. **Per-payee balances** — the user asked whether a tab can have multiple
   payees. It can *structurally* (attach several as payees; they share the tab's
   one balance and all get reminders). What does **not** exist is a *separate
   balance per payee* — that changes the "one balance per tab" core and is its
   own milestone. Scope it before building.
2. **HTTPS 404 for unknown subdomains** — left as a TLS error on purpose; the
   safe implementation is a DNS-01 wildcard cert (Linode plugin), see the Caddy
   trap above.
3. **More deep coverage** — see the coverage section.

---

## Quick verification for the next session

```
git -C /home/jcz/Github/bitt log --oneline -8
curl -s https://btabby.fluidgrid.site/healthz          # ok v1.6.0
gh run list --workflow=Release --limit 1               # last release success
ssh jcz@recipe.fluidgrid.site 'docker ps --filter name=bitt --format "{{.Names}} {{.Image}} {{.Status}}"'
```
