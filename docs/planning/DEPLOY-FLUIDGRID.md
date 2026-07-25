# Deploying BitTabby to btabby.fluidgrid.site

**Target:** the `recipe.fluidgrid.site` host, serving BitTabby at
**`btabby.fluidgrid.site`**, as a Docker Compose stack, behind the host's
existing **Caddy** reverse proxy, on the host's **MariaDB** — the same shape as
the other sites on that host.

This plan is written to be executed on the host. It does not require anything
from this repository's author beyond the checkout and the two secrets you
create. The tailored stack is [`compose.fluidgrid.yaml`](../../compose.fluidgrid.yaml);
this document is the runbook and the reasoning. The general-purpose deployment
guide, including backup and restore, is [`docs/DEPLOY.md`](../DEPLOY.md).

---

## The shape, and why

Three facts about the host drive every choice below:

| Host provides | So BitTabby | Consequence |
|---|---|---|
| Caddy, fronting the other sites, terminating TLS | publishes to **loopback only**; Caddy proxies to it | secure cookies + HSTS on; nothing on the network reaches the app except through Caddy |
| MariaDB, used by the other sites | uses **MariaDB as its backend**, not SQLite | **no data volume** — every byte of state, avatars included, lives in the database |
| A single host running both | reaches MariaDB via `host.docker.internal` | the DB connection never leaves the host |

Because avatars are stored as `MEDIUMBLOB`s in the database (not files on disk),
choosing MariaDB makes the container **disk-stateless**. That is why the stack
keeps no named volume and runs with a **read-only root filesystem** — a
meaningful hardening step the SQLite deployment cannot take, since SQLite must
write its WAL beside the database.

```
Internet ──HTTPS──▶ Caddy (host) ──HTTP──▶ 127.0.0.1:8087 ──▶ bittabby:8080 (container)
                                                                     │
                                                          host.docker.internal:3306
                                                                     ▼
                                                            MariaDB (host)
```

---

## Prerequisites on the host

- Docker Engine + the Compose plugin (`docker compose`, v2).
- The host's MariaDB reachable from containers. Confirm it listens on an address
  the Docker bridge can reach — either `0.0.0.0`/the bridge gateway, or bind it
  to the docker0 gateway (commonly `172.17.0.1`). If MariaDB is bound to
  `127.0.0.1` only, containers cannot reach it; see "MariaDB not reachable" in
  Troubleshooting.
- DNS: an `A`/`AAAA` record for `btabby.fluidgrid.site` pointing at the host, so
  Caddy can obtain a certificate for it. (Same as the other sites.)
- The repository checked out on the host, at a known path.

---

## Step 1 — Create the database, user, and grants

On the host's MariaDB, as an admin:

```sql
CREATE DATABASE btabby CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

-- '%' so the grant matches a connection arriving over the Docker bridge, which
-- does not present as localhost. Restrict the host pattern further if your
-- MariaDB is configured for it (e.g. the docker subnet '172.%').
CREATE USER 'bitt'@'%' IDENTIFIED BY 'CHOOSE-A-STRONG-PASSWORD';
GRANT ALL PRIVILEGES ON btabby.* TO 'bitt'@'%';
FLUSH PRIVILEGES;
```

`utf8mb4_bin` is **not optional**. MySQL/MariaDB's default collation is
case-insensitive, under which two idempotency keys or two period keys differing
only in case would collide — and those keys are exactly what stop a double
charge. The app also pins the collation and `STRICT_ALL_TABLES` on its own
connection, but creating the database correctly keeps everything consistent.

The app creates and migrates its own tables on first start; it does **not**
create the database itself.

### Optional: append-only hardening MariaDB allows and SQLite does not

The ledger's append-only rule is enforced by triggers on both backends. On
MariaDB you can additionally deny the application role the ability to break it:

```sql
REVOKE UPDATE, DELETE ON btabby.entries            FROM 'bitt'@'%';
REVOKE UPDATE, DELETE ON btabby.entry_items         FROM 'bitt'@'%';
REVOKE UPDATE, DELETE ON btabby.posted_periods      FROM 'bitt'@'%';
REVOKE UPDATE, DELETE ON btabby.posted_fees         FROM 'bitt'@'%';
REVOKE UPDATE, DELETE ON btabby.posted_interest     FROM 'bitt'@'%';
REVOKE UPDATE, DELETE ON btabby.sent_notifications  FROM 'bitt'@'%';
```

Do this **after** the first start has created the tables, and know that a future
migration needing those grants will fail loudly until you restore them. This is
defense in depth, not required for correctness.

---

## Step 2 — Create the secrets

From the checkout directory:

```bash
mkdir -p secrets

# The shared secret that authenticates the reminder caller to /internal/tick.
head -c 32 /dev/urandom | base64 > secrets/tick_secret

# The MariaDB DSN. It carries the password, so it is a file secret, never inline.
# Form: user:pass@tcp(host:port)/dbname  -- no extra params needed, the app adds
# collation and sql_mode itself.
printf 'bitt:CHOOSE-A-STRONG-PASSWORD@tcp(host.docker.internal:3306)/btabby' > secrets/db_dsn

# Both containers run as uid 65532 and the app fails closed if it cannot read a
# secret. Hand the files to 65532 and lock the mode.
sudo chown 65532 secrets/tick_secret secrets/db_dsn
chmod 600 secrets/tick_secret secrets/db_dsn
```

`secrets/` is git-ignored by the repository's `.gitignore` pattern for secret
material — confirm before the first commit on the host that neither file is
tracked. If MariaDB is on a **different** host than the container, append
`?tls=true` to the DSN so the connection is encrypted; for a same-host bridge
connection it stays on the host and plaintext is acceptable.

---

## Step 3 — Bring the stack up

```bash
docker compose -f compose.fluidgrid.yaml up -d
docker compose -f compose.fluidgrid.yaml logs -f bittabby   # watch it migrate and bind
```

A healthy first start logs the migrations applying and then the server
listening. It is now answering on `127.0.0.1:8087` on the host — not yet on the
internet, which is Caddy's job next.

Quick local check before wiring Caddy:

```bash
curl -s http://127.0.0.1:8087/healthz     # -> ok v0.8.2 (or later)
```

---

## Step 4 — Add the Caddy site

Add this to the host's Caddyfile, alongside the other sites, then reload Caddy
(`caddy reload` or the host's usual mechanism):

```caddy
btabby.fluidgrid.site {
	reverse_proxy 127.0.0.1:8087

	# The app already sets its own security headers (CSP, HSTS, X-Frame-Options,
	# nosniff). Do not have Caddy add a second, conflicting set.

	# Optional: put the real client IP in front of the app. The app currently
	# logs the connection's remote address, which behind a proxy is the proxy;
	# these headers let a future version, and Caddy's own access log, record the
	# true client. Harmless to set now.
	header_up X-Forwarded-For {remote_host}
	header_up X-Forwarded-Proto {scheme}
}
```

Caddy obtains and renews the TLS certificate automatically. Because the app runs
with `BITT_SECURE_COOKIES=true`, its session cookies carry `Secure` and it emits
HSTS — both correct now that it is reached only over HTTPS.

Verify from anywhere:

```bash
curl -sI https://btabby.fluidgrid.site/healthz
```

Then open `https://btabby.fluidgrid.site` in a browser and complete the one-time
first-run setup, which creates the first admin account.

---

## Step 5 — Confirm the PWA and reminders

- **PWA (UI-05).** On a phone, open the site in the browser and use "Add to Home
  Screen." It installs with the BitTabby icon and opens to the cached shell.
  Data still requires a connection by design — offline shows a styled offline
  page, never a stale balance.
- **Reminders.** The `reminders` sidecar posts to `/internal/tick` hourly over
  the internal network. Confirm in the app under **Notifications** that the tick
  secret is recognised (the screen states whether delivery is armed). To drive
  reminders from a host timer instead of the sidecar, remove the `reminders`
  service and add a crontab entry:

  ```cron
  # hourly, on the host
  0 * * * * curl -fsS -X POST -H "Authorization: Bearer $(cat /path/to/secrets/tick_secret)" https://btabby.fluidgrid.site/internal/tick >/dev/null 2>&1
  ```

---

## Backup and restore

With MariaDB the backup is a database dump, not a volume archive — simpler and
consistent with the other sites on this host.

```bash
# Backup
mysqldump --single-transaction --routines --triggers btabby > btabby-$(date +%F).sql

# Restore into an empty database
mysql btabby < btabby-YYYY-MM-DD.sql
```

`--single-transaction` takes a consistent snapshot without locking writers.
`--triggers` is essential: the append-only enforcement lives in triggers, and a
restore without them would recreate the tables unprotected.

---

## Upgrading

1. Pin a released tag in `compose.fluidgrid.yaml` (`ghcr.io/johnzastrow/bitt:vX.Y.Z`)
   rather than `:latest`, so a redeploy is a deliberate version change.
2. `docker compose -f compose.fluidgrid.yaml pull bittabby`
3. `docker compose -f compose.fluidgrid.yaml up -d bittabby`

The app runs any new migrations on start. Migrations are forward-only; take the
backup above first. The PWA's service worker invalidates its cache from the
asset digest, so a browser picks up the new shell on its next navigation.

---

## If Caddy runs in a container

The primary plan assumes Caddy on the host (which fits "MariaDB on the host").
If instead Caddy runs as a container with its own Docker network, change two
things in `compose.fluidgrid.yaml`:

1. **Drop** the `ports:` block from `bittabby` — no host port is needed.
2. **Join** Caddy's network and proxy by service name:

   ```yaml
   services:
     bittabby:
       networks: [caddy_net]
   networks:
     caddy_net:
       external: true
   ```

   Then in the Caddyfile: `reverse_proxy bittabby:8080`. The container-to-host
   MariaDB path via `host.docker.internal` is unchanged.

---

## Troubleshooting

- **App exits immediately, log mentions a secret.** The secret file is not
  readable by uid 65532. Re-run the `chown 65532` / `chmod 600` from Step 2. The
  app fails closed on an unreadable secret rather than starting without it.
- **MariaDB not reachable / "connection refused" to host.docker.internal.**
  MariaDB is likely bound to `127.0.0.1` only. Bind it to the docker bridge
  gateway as well (e.g. `bind-address = 172.17.0.1` or add it), or bind
  `0.0.0.0` behind the host firewall. Confirm the `bitt'@'%'` grant matches the
  bridge source address.
- **"Access denied for user 'bitt'".** The grant host pattern does not match the
  bridge connection. Use `'bitt'@'%'` (or the docker subnet), not
  `'bitt'@'localhost'`.
- **Browser shows a certificate/redirect loop.** Caddy is terminating TLS and
  the app has `BITT_SECURE_COOKIES=true`; that pairing is correct. A loop usually
  means Caddy is proxying to the wrong port or the app is not up — check
  `curl http://127.0.0.1:8087/healthz` on the host.
- **Read-only filesystem error on start.** Something in the container tried to
  write locally. With MariaDB nothing should; flip `read_only: true` to `false`
  in the compose file as a stopgap and report it.

---

## What this deployment does and does not carry

- **Carries:** the full app at v1 feature-complete, MariaDB backend, the PWA,
  reminders via the sidecar, TLS via Caddy, secrets as files, read-only
  container, no local state.
- **Does not carry:** any migration from an existing SQLite instance. The two
  backends are not migrate-compatible; this is a fresh instance. If there is
  SQLite data to bring over, that is a bespoke export/import, not a supported
  button (see `docs/DEPLOY.md`, "Choosing a database").
