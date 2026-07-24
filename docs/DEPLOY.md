# Deploying BitTabby

Everything here has been run, not just written. Where a step looks odd, there is
a note saying why — those are the places people "fix" it back and break their
deployment.

BitTabby is one static Go binary with an embedded SQLite database. It has no
runtime dependencies: no interpreter, no external database, no message queue, no
background scheduler. That is the whole design, and it is why deploying it is
short.

- [Quick start with Docker](#quick-start-with-docker)
- [Configuration](#configuration)
- [Choosing a database](#choosing-a-database)
- [Secrets](#secrets)
- [Delivering reminders](#delivering-reminders)
- [Running without Docker](#running-without-docker)
- [Backup](#backup)
- [Restore](#restore)
- [Upgrading](#upgrading)

---

## Quick start with Docker

```bash
git clone https://github.com/johnzastrow/bitt.git && cd bitt

mkdir -p secrets
head -c 32 /dev/urandom | base64 > secrets/tick_secret
sudo chown 65532 secrets/tick_secret && chmod 600 secrets/tick_secret

docker compose up -d
```

Open <http://localhost:8080> and complete first-run setup. That screen appears
once and then locks permanently — the account it creates is the administrator.

### Why that `chown`

It is the one surprising step, and it is worth understanding rather than
copying.

Docker bind-mounts a secret file into a container with the **host's** ownership,
unchanged. Both containers in `compose.yaml` run as uid **65532** (the non-root
user in the image). So a secret file owned by you with mode `0600` is one that
**neither container can read** — the app refuses to start rather than run
without its secret, and the reminder caller sends an empty token and gets a 401.

Handing the file to 65532 is what makes `0600` work everywhere at once: on the
host, in the app, and in the reminder caller.

If you would rather not `chown`, the alternative is `chmod 700 secrets` and
`chmod 644 secrets/tick_secret` — the directory keeps other accounts on the host
out, and the file bits only decide whether the container's user can read it. The
app will not warn about that arrangement, because it checks the directory as
well as the file.

### What you get

| | |
|---|---|
| Image size | ~25 MB |
| Base | `gcr.io/distroless/static-debian12:nonroot` — no shell, no package manager, no userland |
| User | 65532:65532, `no-new-privileges`, all capabilities dropped |
| Data | A **named volume** at `/data`, files mode `0600` |
| Health | `HEALTHCHECK` runs `bittabby --healthcheck`, which probes `/healthz` |

The image has no `curl` and no shell, which is why the binary probes itself.
Shipping either one to satisfy a healthcheck would put a whole userland into an
image that currently holds a single static binary.

### Bind mount instead of a named volume

A named volume inherits `/data`'s ownership from the image. A bind mount does
not — it arrives owned by whoever owns the host directory. If you replace the
volume with a bind mount, chown it first:

```bash
mkdir -p ./data && sudo chown 65532:65532 ./data
```

Otherwise the container cannot create its database and exits.

---

## Configuration

Every setting comes from the environment (DEPLOY-06). Nothing is compiled in and
nothing sensitive is logged. `.env.example` lists them all with commentary.

The ones that matter on a first deployment:

| Variable | Default | Notes |
|---|---|---|
| `BITT_ADDR` | `:8080` | Listen address |
| `BITT_DB_DRIVER` | `sqlite` | `sqlite` or `mariadb` (see [Choosing a database](#choosing-a-database)) |
| `BITT_DB_PATH` | `data/bitt.db` | SQLite file; `/data/bitt.db` in the image |
| `BITT_DB_DSN` | unset | MariaDB connection string; required when the driver is `mariadb` |
| `BITT_TIMEZONE` | `America/New_York` | Seeds first-run setup; after that the stored value is authoritative |
| `BITT_SECURE_COOKIES` | `true` | **Set false only for plain HTTP on localhost** |
| `BITT_BASE_URL` | unset | External origin, used for the link in a reminder |
| `BITT_LEDGER_TRIGGERS` | `true` | Append-only database triggers; disable only for a deliberate manual repair |

`BITT_BASE_URL` is deliberately not derived from a request's `Host` header. The
only caller that needs it arrives on a cron request whose `Host` is whatever the
cron sent, and a link built from that is a phishing vector. Unset means
reminders omit the link rather than guessing one.

### Notification settings: environment or the app

SMTP server, port, username, from address, and the ntfy URL can be set **either**
in the environment **or** in the app under **Notifications** (administrators
only).

**The environment wins**, field by field. A setting present in the environment
is shown read-only in the app, so a deployment behaves the same way every time
it starts whatever is in its volume. If you want to manage a setting in the app,
leave its variable unset.

Reminder lead times and message text resolve the other way — a tab's own
reminders beat the instance defaults, which beat the environment. A message is
content, where the most specific author should win; a delivery setting is
deployment, where the thing under version control should win.

---

## Choosing a database

**SQLite is the default and the recommendation.** For a household instance it is
the right answer and it is not close: one file, no server process, and a backup
is a single file you can copy. Everything above assumes it.

**MariaDB (or MySQL) is supported (DEPLOY-03)** for a deployment that already
runs one, or that wants the database off the application's disk. Select it with:

```bash
BITT_DB_DRIVER=mariadb
BITT_DB_DSN=file:/run/secrets/db_dsn    # user:pass@tcp(host:3306)/dbname
```

The DSN carries a password, so supply it as a `file:` secret rather than inline,
and it is never written to a log. The database must exist; the app creates its
own tables on first start but not the schema that holds them:

```sql
CREATE DATABASE bitt CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
CREATE USER 'bitt'@'%' IDENTIFIED BY '...';
GRANT ALL PRIVILEGES ON bitt.* TO 'bitt'@'%';
```

The `utf8mb4_bin` collation is not optional. MySQL's default is
case-*insensitive*, under which two idempotency keys or two period keys
differing only in case would collide — and those keys are what stop a double
charge. The app pins the collation and `STRICT_ALL_TABLES` on its own
connections regardless, but creating the database with the right collation keeps
everything consistent.

**Hardening MariaDB can go one step SQLite cannot.** The ledger's append-only
rule is enforced by triggers on both backends, but on MariaDB you can also give
the application role no ability to break it in the first place:

```sql
REVOKE UPDATE, DELETE ON bitt.entries FROM 'bitt'@'%';
REVOKE UPDATE, DELETE ON bitt.entry_items FROM 'bitt'@'%';
-- and likewise posted_periods, posted_fees, posted_interest, sent_notifications
```

Do this only if you are comfortable that a future migration needing those grants
will fail loudly until you restore them.

**The two backends are not migrate-compatible.** There is no built-in path that
copies a live SQLite database into MariaDB; choose one at deployment. The schema
is identical table-for-table, so a bespoke export/import is possible, but it is
not a supported button.

---

## Secrets

Three values are credentials, and they come from the environment or a file and
nowhere else. They are **never** stored in the database, never shown in the app,
and never carried in a backup.

| Variable | For |
|---|---|
| `BITT_TICK_SECRET` | Authenticates the cron caller to `/internal/tick` |
| `BITT_SMTP_PASSWORD` | SMTP, if your server wants authentication |
| `BITT_NTFY_TOKEN` | Only for a private ntfy server |

Any `BITT_*` value accepts the form `file:/run/secrets/name`, which reads the
value from a mounted file instead. **A file that cannot be read stops the app
from starting** — it fails closed rather than degrading to a blank secret and
running in a state you did not ask for.

The app warns if a secret file is readable by other accounts *and* sits in a
directory they can enter. Both conditions have to hold; a `0644` file inside a
`0700` directory is not exposed and does not warn.

### Why credentials are not in the app

They could have been. The reasoning is in migration `0010`, and it is short: a
secret in the database has to be encrypted at rest, the key for that has to come
from the environment anyway, so the result is the same number of environment
secrets plus a key to manage and re-wrap — and it would put live credentials
into every backup taken with the instructions below.

The tick secret has a further reason. It is what makes `/internal/tick` fail
closed, and "unset refuses everything" is a posture worth keeping out of reach
of a compromised admin session.

---

## Delivering reminders

**Nothing sends until something external calls `/internal/tick`.** There is no
timer inside the server, deliberately: an app that only acts when asked cannot
quietly bill someone at 3am, and a background scheduler is what stalled this
project's predecessor.

The endpoint **fails closed**. With `BITT_TICK_SECRET` unset it refuses every
request, so a deployment that has not configured it cannot be driven at all. The
Notifications screen in the app tells you which state you are in.

`compose.yaml` includes a `reminders` sidecar that does this hourly. Anything
that can make an authenticated HTTP request works instead:

**Host crontab**

```cron
0 * * * * curl -fsS -X POST -H "Authorization: Bearer $(cat /path/to/secrets/tick_secret)" http://localhost:8080/internal/tick
```

**systemd timer** — a `.service` running the same `curl`, plus a `.timer` with
`OnCalendar=hourly`.

Sending is idempotent per (tab, event, channel), so an hour that fires twice
sends nothing twice, and an hour that is missed sends late rather than never.
Hourly is ample; reminders fire on day boundaries.

---

## Running without Docker

```bash
make build
BITT_SECURE_COOKIES=false ./bittabby
```

The binary embeds its templates, stylesheets, migrations, and the IANA timezone
database, so it runs with no files beside it other than the database it creates
(DEPLOY-04).

For a real deployment, run it behind a reverse proxy that terminates TLS, leave
`BITT_SECURE_COOKIES` at its default of `true`, and run it as a dedicated
unprivileged user. It never needs root.

---

## Backup

The database is a single SQLite file. What makes a backup correct is catching it
in a consistent state, and the reliable way to do that is to stop the app first
— on a clean shutdown SQLite checkpoints its write-ahead log into the main file,
which is why a cold backup contains `bitt.db` alone with no `-wal` sidecar
beside it.

The downtime is a few seconds.

```bash
docker compose stop bittabby

docker run --rm \
  -v bitt_bitt-data:/data:ro \
  -v "$PWD:/backup" \
  alpine tar czf /backup/bitt-$(date +%F).tgz -C /data .

docker compose start bittabby
```

The volume name is `<project>_bitt-data`; `docker volume ls` will show yours.

Also copy `secrets/` somewhere safe — separately, and not into the same archive.
The database and the credentials that reach it should not travel together.

**Without Docker:** stop the service and copy `data/bitt.db`. If you cannot
stop it, use SQLite's own online backup rather than `cp`, which can catch a
torn write:

```bash
sqlite3 data/bitt.db ".backup 'bitt-$(date +%F).db'"
```

### Verify the backup

A backup you have not restored is a hypothesis. Check that the archive contains
what you think:

```bash
tar tzf bitt-2026-07-24.tgz
```

---

## Restore

**This procedure has been exercised**: the volume was destroyed outright and the
data came back. That is the only reason it is written down as fact.

```bash
# 1. Stop everything and remove the volume you are replacing.
docker compose down -v

# 2. Recreate the volume and unpack the backup into it, with the ownership the
#    container runs as. That chown is required -- tar restores the archive's
#    ownership, and a mismatch leaves the app unable to open its own database.
docker volume create bitt_bitt-data
docker run --rm \
  -v bitt_bitt-data:/data \
  -v "$PWD:/backup" \
  alpine sh -c 'tar xzf /backup/bitt-2026-07-24.tgz -C /data && chown -R 65532:65532 /data'

# 3. Start.
docker compose up -d
```

Then sign in and check that a tab you recognise is there with the right balance.

**Restoring an older database is safe**, and forward-only migrations are why: on
startup the app applies any schema migrations the backup predates. Restoring a
0.7.2 backup into a 0.7.4 binary works. Going the other way — an old binary
against a newer database — does not, so keep the image tag that matches a
backup you might restore.

---

## Upgrading

```bash
docker compose pull && docker compose up -d
```

**Take a backup first.** Migrations are forward-only: starting the server
applies any pending ones, and there is no down migration. That is a deliberate
simplification, and the backup is what makes it safe.

The version in use is on the footer of every page and at `/healthz`.

---

## Health and monitoring

`GET /healthz` returns `ok v0.7.4` and requires no authentication. It reports
that the process is serving; it does not touch the database, so it is safe to
poll.

The container's own `HEALTHCHECK` uses `bittabby --healthcheck`, which probes
that endpoint on loopback and reports through its exit code. You can run it by
hand:

```bash
docker compose exec bittabby /bittabby --healthcheck && echo healthy
```
