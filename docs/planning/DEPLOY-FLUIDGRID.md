# Deploying BitTabby to btabby.fluidgrid.site

**Target:** the `recipe.fluidgrid.site` host (`45.56.117.76`), serving BitTabby
at **`btabby.fluidgrid.site`**, as a Docker Compose stack, behind the host's
apt-installed **Caddy**, on the host's **MariaDB** — matching the other sites on
that host (actalog, mealie).

The tailored stack is [`compose.fluidgrid.yaml`](../../compose.fluidgrid.yaml);
this is the runbook and the reasoning. The general-purpose guide (backup,
restore, the portable bridge-network variant) is [`docs/DEPLOY.md`](../DEPLOY.md).

---

## The pattern on this host (observed, not assumed)

The other sites follow one shape, and BitTabby matches it:

- **Caddy runs on the host** (apt, `systemctl` service, `/etc/caddy/Caddyfile`)
  and reverse-proxies each site to a **loopback port**: actalog → `localhost:8080`,
  mealie → `localhost:9925`.
- **App containers use `network_mode: host`**, so they bind their port directly
  on the host and reach the host's **MariaDB at `127.0.0.1:3306`** (the same DB
  the other sites use).
- **Deploy dirs live in `~`**, one per app (`~/actadocker`, …), each a compose
  file plus a git-ignored `secrets`/`.env`.

BitTabby takes **port 8091** (8080 is actalog's, 9925 is mealie's) and deploys
from **`~/bittdocker`**.

```
Internet ──HTTPS──▶ Caddy (host) ──HTTP──▶ 127.0.0.1:8091 ◀── bittabby (network_mode: host)
                                                                     │ 127.0.0.1:3306
                                                                     ▼
                                                            MariaDB (host)
```

Because avatars are `MEDIUMBLOB`s in the database, choosing MariaDB makes the
container **disk-stateless** — no data volume, and a **read-only root
filesystem**, a hardening step the SQLite deployment cannot take.

---

## Step 0 — DNS (the one thing that must be done off the host)

`btabby.fluidgrid.site` has **no DNS record yet**, and there is no wildcard.
Caddy cannot obtain a TLS certificate until it resolves. Add, at the DNS
provider for `fluidgrid.site`:

```
btabby.fluidgrid.site.  A  45.56.117.76
```

(the same IP as `al.` and `recipe.`). Everything below can be prepared before
this propagates; Caddy will get the certificate automatically once it does.

---

## Step 1 — Create the database, user, and grants

On the host, as MariaDB root (the root password is in `~/actadocker/.env` as
`DB_ROOT_PASSWORD`):

```sql
CREATE DATABASE btabby CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

-- The app connects over TCP to 127.0.0.1, so grant the loopback host. Adding
-- 'localhost' too covers a socket connection and costs nothing.
CREATE USER 'btabby'@'127.0.0.1' IDENTIFIED BY 'STRONG-PASSWORD';
CREATE USER 'btabby'@'localhost' IDENTIFIED BY 'STRONG-PASSWORD';
GRANT ALL PRIVILEGES ON btabby.* TO 'btabby'@'127.0.0.1';
GRANT ALL PRIVILEGES ON btabby.* TO 'btabby'@'localhost';
FLUSH PRIVILEGES;
```

`utf8mb4_bin` is **not optional**: the default collation is case-insensitive,
under which two idempotency keys or period keys differing only in case would
collide — and those keys are what stop a double charge. The app also pins the
collation and `STRICT_ALL_TABLES` on its connection. The app creates and
migrates its own tables on first start; it does not create the database.

### Optional append-only hardening (after first start creates the tables)

```sql
REVOKE UPDATE, DELETE ON btabby.entries            FROM 'btabby'@'127.0.0.1', 'btabby'@'localhost';
REVOKE UPDATE, DELETE ON btabby.entry_items         FROM 'btabby'@'127.0.0.1', 'btabby'@'localhost';
REVOKE UPDATE, DELETE ON btabby.posted_periods      FROM 'btabby'@'127.0.0.1', 'btabby'@'localhost';
REVOKE UPDATE, DELETE ON btabby.posted_fees         FROM 'btabby'@'127.0.0.1', 'btabby'@'localhost';
REVOKE UPDATE, DELETE ON btabby.posted_interest     FROM 'btabby'@'127.0.0.1', 'btabby'@'localhost';
REVOKE UPDATE, DELETE ON btabby.sent_notifications  FROM 'btabby'@'127.0.0.1', 'btabby'@'localhost';
```

Defense in depth on top of the triggers; know a future migration needing those
grants will fail loudly until you restore them.

---

## Step 2 — Deploy dir and secrets

```bash
mkdir -p ~/bittdocker/secrets && cd ~/bittdocker
# copy compose.fluidgrid.yaml here (from the repo)

head -c 32 /dev/urandom | base64 > secrets/tick_secret
printf 'btabby:STRONG-PASSWORD@tcp(127.0.0.1:3306)/btabby' > secrets/db_dsn

# Both containers run as uid 65532 and the app fails closed on an unreadable
# secret. Hand the files to 65532 and lock the mode.
sudo chown 65532 secrets/tick_secret secrets/db_dsn
chmod 600 secrets/tick_secret secrets/db_dsn
```

The DSN carries the password, so it is a file, never inline. The app adds
collation and `sql_mode` itself, so `user:pass@tcp(127.0.0.1:3306)/btabby` is
all it needs.

---

## Step 3 — Bring the stack up

```bash
cd ~/bittdocker
docker compose -f compose.fluidgrid.yaml up -d
docker compose -f compose.fluidgrid.yaml logs -f bittabby   # watch it migrate and bind
curl -s http://127.0.0.1:8091/healthz                       # -> ok v1.0.0
```

A healthy first start logs the migrations applying, then the server listening on
`127.0.0.1:8091`. It is reachable only on loopback — Caddy is next.

---

## Step 4 — Add the Caddy site

Append to `/etc/caddy/Caddyfile` alongside the other sites, then reload
(`sudo systemctl reload caddy`):

```caddy
btabby.fluidgrid.site {
	reverse_proxy 127.0.0.1:8091

	# The app sets its own security headers (CSP, HSTS, X-Frame-Options,
	# nosniff). Do not add a second, conflicting set here.

	encode gzip

	log {
		output file /var/log/caddy/btabby.access.log {
			roll_size 1MB
			roll_keep 5
		}
		format console
	}
}
```

Caddy obtains and renews the certificate automatically once DNS (Step 0)
resolves. Because the app runs with `BITT_SECURE_COOKIES=true`, its cookies carry
`Secure` and it emits HSTS — correct now that it is reached only over HTTPS.

Verify:

```bash
curl -sI https://btabby.fluidgrid.site/healthz
```

Then open `https://btabby.fluidgrid.site` and complete the one-time first-run
setup, which creates the first admin account.

---

## Step 5 — Confirm the PWA and reminders

- **PWA (UI-05).** On a phone, open the site and "Add to Home Screen." It
  installs with the BitTabby icon and opens to the cached shell; offline shows a
  styled offline page, never a stale balance.
- **Reminders.** The `reminders` sidecar posts to `127.0.0.1:8091/internal/tick`
  hourly. The Notifications screen in the app states whether delivery is armed.
  To drive from a host timer instead, remove the `reminders` service and add:

  ```cron
  0 * * * * curl -fsS -X POST -H "Authorization: Bearer $(cat ~/bittdocker/secrets/tick_secret)" https://btabby.fluidgrid.site/internal/tick >/dev/null 2>&1
  ```

- **Email.** The host uses smtp2go for actalog. To send BitTabby reminders by
  email, uncomment the SMTP block in the compose file (host `mail.smtp2go.com`,
  port `2525`) and add an `smtp_password` secret, or set it in-app under
  Notifications.

---

## Backup and restore

With MariaDB the backup is a database dump, consistent with the other sites:

```bash
# Backup (the root password is in ~/actadocker/.env)
mysqldump --single-transaction --routines --triggers btabby > btabby-$(date +%F).sql

# Restore into an empty database
mysql btabby < btabby-YYYY-MM-DD.sql
```

`--triggers` is essential — the append-only enforcement lives in triggers, and a
restore without them recreates the tables unprotected.

---

## Upgrading

1. Bump the pinned tag in `compose.fluidgrid.yaml` (`ghcr.io/johnzastrow/bitt:vX.Y.Z`).
2. `docker compose -f compose.fluidgrid.yaml pull bittabby`
3. `docker compose -f compose.fluidgrid.yaml up -d bittabby`

The app runs any new migrations on start (forward-only — take the backup first).
The PWA service worker invalidates its cache from the asset digest, so a browser
picks up the new shell on its next navigation.

---

## Troubleshooting

- **App exits immediately, log mentions a secret.** The secret file is not
  readable by uid 65532. Re-run the `chown 65532` / `chmod 600` from Step 2.
- **"Access denied for user 'btabby'".** The grant host does not match the
  connection. The app connects over TCP to 127.0.0.1; ensure the
  `'btabby'@'127.0.0.1'` grant exists (Step 1).
- **"connection refused" to 127.0.0.1:3306.** Confirm MariaDB is listening on
  127.0.0.1 (it is, for the other sites). With `network_mode: host` the container
  shares the host loopback, so 127.0.0.1 is the host's MariaDB.
- **Caddy not serving / cert errors.** Almost always DNS (Step 0). Confirm
  `getent hosts btabby.fluidgrid.site` returns `45.56.117.76`, then
  `sudo systemctl reload caddy`. Check `curl http://127.0.0.1:8091/healthz` to
  confirm the app itself is up.
- **Read-only filesystem error on start.** Something tried to write locally; with
  MariaDB nothing should. Flip `read_only: true` to `false` as a stopgap and
  report it.

---

## What this deployment carries

- The full app at v1, MariaDB backend, the PWA, reminders via the sidecar, TLS
  via the host Caddy, secrets as files, a read-only container, no local state.
- **Not** a migration from any existing SQLite instance — the two backends are
  not migrate-compatible; this is a fresh instance.
