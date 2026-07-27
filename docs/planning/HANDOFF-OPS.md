# Handoff — post-v1, deployed and operating

Written 2026-07-27, to be read cold. v1 shipped; the app is **live in production**
and the work since has been small releases, a bug fix, deep test coverage, and
running the deployment. The project's shape, ledger invariants, and earlier
traps are in [HANDOFF.md](HANDOFF.md); this file is the current delta and the one
to start from.

---

## Current state in one paragraph

BitTabby is at **v1.2.0**, public at github.com/johnzastrow/bitt (MIT), imaged at
`ghcr.io/johnzastrow/bitt` (**amd64 only**), and **live at
https://btabby.fluidgrid.site** on the `recipe.fluidgrid.site` host. It runs as a
Docker Compose stack on the host's MariaDB, `network_mode: host`, behind the
host's apt-installed Caddy. Working tree clean; everything pushed. Full test
suite green on SQLite and against a real MariaDB.

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
2. Bump the pinned tag in `compose.fluidgrid.yaml`; commit.
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

## Coverage as of this handoff

Strong: money/schedule/ledger/auth/loan/fee/tz 87–100%; notify 81%; store enums
covered. Moderate: web ~74%, store/sqldb ~66% (higher under MariaDB), config 73%.

**Untested and deliberately so:** `cmd/bittabby` main/run (process wiring,
exercised live), `web/views` (templ-generated, covered through handler tests),
and MariaDB-dialect functions that read 0% on a SQLite-only run but are covered
under the MariaDB suite.

**Next coverage targets if resumed:** remaining `web` handler error-paths, and
`config`/`ledger` internal helpers (`activeItems`, `flatPayments`).

---

## Open threads (nothing blocking; pick up any)

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
curl -s https://btabby.fluidgrid.site/healthz          # ok v1.2.0
gh run list --workflow=Release --limit 1               # last release success
```
