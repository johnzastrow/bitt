# Handoff — Phase 6 (ship), complete, at 0.8.2

> **Update (0.8.2):** Phase 6 is done. UI-05, the PWA, landed — manifest,
> home-screen icons, a root-scoped service worker (network-first navigations,
> a styled offline page, cache versioned off the asset digest), verified
> headless including the offline path. **v1 is feature-complete, 54/54.** The
> "next task" section below is kept for the record but is no longer outstanding.
> What remains: deploy to `btabby.fluidgrid.site` (see
> [DEPLOY-FLUIDGRID.md](DEPLOY-FLUIDGRID.md)) and release polish, then tag
> `v1.0.0`.

---

# Handoff — Phase 6 (ship), 4 of 5 done, at 0.8.1

Written 2026-07-24, to be read cold. Covers what shipped in Phase 6 so far, the
one task left in v1, and the traps around it.

The main [HANDOFF.md](HANDOFF.md) still holds the project's shape, the ledger
invariants, and the 0.5/0.6 traps. [HANDOFF-PHASE5.md](HANDOFF-PHASE5.md) covers
notifications. This file is the Phase 6 delta and is the one to start from.

---

## The next task (start here): UI-05, the PWA — the last thing in v1

**This is the only requirement left.** 53 of 54 are done; UI-05 is the whole
remaining scope of v1.

**What it asks (from REQUIREMENTS.md):** "The app installs to a phone home screen
as a PWA and opens to a cached shell; all data operations require a connection."

Read that last clause carefully — it is the design constraint, not an
afterthought. This is a **shell-only** PWA. The service worker caches the static
shell (stylesheet, htmx, logo, the app frame) so the app opens instantly and
looks installed, but **every data operation still hits the network**. There is
no offline ledger, no background sync, no cached balances. That is deliberate and
matches the whole architecture: the balance is always derived server-side and
must never be shown stale (LEDGER-03), so caching tab data would be a way to show
a wrong balance. Do not build offline data. If you find yourself writing a cache
for anything under `/tabs`, stop.

**What it needs, concretely:**

1. **A web app manifest** (`manifest.webmanifest`): name, short_name, icons,
   `display: standalone`, `start_url: /`, theme/background colours matching the
   chalk palette in `app.css`. Served from `internal/web/static/`, linked in
   `layout.templ`'s `<head>`.
2. **Icons.** At least 192px and 512px PNGs, plus a maskable variant. The logo is
   `internal/web/static/logo.svg` — rasterise from it. These are the one new
   binary asset; commit them like the vendored htmx.
3. **A service worker** (`sw.js`, served from `/static/` or root — note the scope
   rule below): on `install`, precache the shell assets; on `fetch`, serve the
   shell from cache and pass everything else to the network. A
   **network-first-then-offline-page** strategy for navigations is the ceiling;
   cache-first for the versioned static assets only.
4. **Registration**: a tiny inline-free script (CSP is `script-src 'self'`, so it
   is a file, like `counter.js`) that calls `navigator.serviceWorker.register`.

**Traps specific to this app:**

- **CSP.** `script-src 'self'` means the registration script is a file, not
  inline. The service worker itself is exempt from the page CSP but is worth
  keeping to the same discipline. Check the browser console after — the htmx
  indicator-stylesheet CSP noise (see "Known cosmetics" below) is pre-existing;
  anything new is yours.
- **Service-worker scope.** A worker served from `/static/sw.js` controls only
  `/static/` by default. To control the whole origin it must be served from the
  root (`/sw.js`) or sent with a `Service-Worker-Allowed: /` header. There is a
  static handler in `server.go`; the cleanest route is a dedicated
  `GET /sw.js` handler that serves the file with the right header, rather than
  bending the static path.
- **Cache versioning.** The app already has an `AssetVersion` digest
  (`layout.templ`, `Page.asset`) appended as `?v=...` to every static URL. Use
  the same digest as the service-worker cache name, so a deploy invalidates the
  precache. Do not hand-maintain a version string in `sw.js`; derive it. There is
  a lesson already in the tree (0.5.2, the stylesheet cache) about a stable URL
  hiding a shipped change — the digest is how that was solved and the SW must not
  reintroduce it.
- **Testing.** The webapp-testing skill (Playwright, headless Chromium) can
  assert the manifest is linked, the icons resolve, and the SW registers. It
  cannot meaningfully test the installed-app experience, so verify the manifest
  and SW mechanics and note that the home-screen install itself was checked by
  reason, not by a real phone (the standing pattern — and be honest about it in
  the commit, as prior phases were).

**Rough shape of the work:** one migration-free feature, all in `internal/web`
(static assets + a handler or two + `layout.templ`). No store changes, no config
changes. Smaller than any phase before it.

---

## What Phase 6 delivered (0.8.0 and 0.8.1)

| Area | What | Where |
|---|---|---|
| Container | ~25MB distroless image, non-root (uid 65532), self-probing healthcheck | `Dockerfile`, `bittabby --healthcheck` in `cmd/bittabby/main.go` |
| Compose | Named volume, file secrets, hourly reminder sidecar | `compose.yaml` |
| CI/CD | CI (vet/test/race/image + templ-current check + a MariaDB service job); release workflow → ghcr.io multi-arch, provenance, tag/version guard | `.github/workflows/` |
| Secrets | env-or-file, fail-closed; the loose-file warning now considers the directory | `internal/config` |
| Backup/restore | Documented **and exercised** (volume destroyed, data restored) | `docs/DEPLOY.md` |
| MariaDB | Second backend behind a `dialect` seam; one suite run against both | `internal/store/sqldb` |

**Run it:**
```
make build && ./bittabby            # SQLite, :8080
make image && make up               # container; needs secrets/tick_secret, see DEPLOY.md
```

---

## The MariaDB work (0.8.1) — the part most worth understanding

`internal/store/sqlite` became `internal/store/sqldb`. **No query call site
changed** — DEPLOY-02's repository boundary held, which was the phase's stated
risk and it did not materialise. Only three things differ per backend, all behind
the `dialect` interface (`dialect.go`):

- **DDL** — separate migration sets, `migrations/sqlite/` and
  `migrations/mariadb/`, with matching version stems. Same table/column/trigger
  names, so the Go code and `applyTriggerPolicy`'s name list are backend-blind.
- **Trigger syntax** — `RAISE(ABORT, msg)` vs `SIGNAL SQLSTATE '45000' SET
  MESSAGE_TEXT = msg`, same message text so `translate` catches both.
- **Row locking** — `dialect.lockRows()` is `" FOR UPDATE"` on MariaDB, empty on
  SQLite.

**Two SQLite-only assumptions MariaDB surfaced, both now fixed and pinned — and
both are the kind of thing to watch for in any new store code:**

1. **Check-then-act needs a lock on a parallel server.** The last-admin guard
   (`accounts.go`, `SetUserActive`) counted other admins then wrote, which was
   atomic only because SQLite pins one writer. On MariaDB two concurrent
   deactivations both passed the check. Fixed by locking the active-admin rows
   `FOR UPDATE` in id order (deadlock-free). **Any new "count then decide then
   write" in the store has this bug latent until it locks.**
2. **STRICT mode rejects what SQLite's dynamic typing accepted.** `VARCHAR(64)`
   was too narrow for the suffixed idempotency keys; MariaDB errored rather than
   truncating (which is correct — a truncated idempotency key drops a charge).
   Widened to 100. **When adding a column, size it for the real data and know
   that MariaDB will enforce it.**

**Session settings the app pins on every MariaDB connection** (`mariaDSN` in
`sqldb.go`): `utf8mb4_bin` collation (MySQL's case-insensitive default would let
two keys differing only in case collide — those keys are the double-charge
guard) and `STRICT_ALL_TABLES` (error, don't truncate). Do not remove these.

**Testing both backends:** `BITT_TEST_MARIADB_DSN` makes the whole store suite
run on a real MariaDB, each test on its own throwaway database (`maria_test.go`).
The SQLite run is the default and needs nothing. To run MariaDB locally:
```
docker run -d --name m -e MARIADB_ROOT_PASSWORD=r -e MARIADB_DATABASE=bitt_test \
  -e MARIADB_USER=bitt -e MARIADB_PASSWORD=p -p 13306:3306 mariadb:11
docker exec m mariadb -uroot -pr -e \
  "GRANT ALL ON \`bitt_test%\`.* TO 'bitt'@'%'; FLUSH PRIVILEGES;"
BITT_TEST_MARIADB_DSN="bitt:p@tcp(127.0.0.1:13306)/bitt_test" go test ./internal/store/sqldb/
```
It takes ~2 minutes (per-test create/drop). The SQLite suite is seconds.

---

## Ledger/notification rules that still bind (do not regress)

These carried from earlier phases and are as load-bearing as ever:

- **The balance is `SUM(entries)`, never a column.** A schema-introspection test
  enforces it. The PWA must not cache it.
- **Entries and claim tables are append-only**, triggers on both backends.
- **Notifications stay off the ledger path.** Send-then-claim, at-least-once, the
  claim written after a confirmed send in its own transaction.
- **Secrets are env-or-file only** — SMTP password, ntfy token, tick secret have
  no database column and no form field, by the argument in migration `0010`.
- **`/internal/tick` fails closed** — no `BITT_TICK_SECRET`, no delivery.

Full detail: [HANDOFF.md](HANDOFF.md), [HANDOFF-PHASE5.md](HANDOFF-PHASE5.md),
[../DATA-MODEL.md](../DATA-MODEL.md), [../DEPLOY.md](../DEPLOY.md).

---

## Known cosmetics (not bugs, do not "fix" blindly)

- **htmx injects an indicator stylesheet at load**, which the CSP
  (`style-src 'self'`) blocks, logging one console error per page. Nothing breaks;
  the app uses no `htmx-indicator`. Left alone deliberately. If the PWA work adds
  its own console noise, that noise is yours and worth chasing — this one is not.

---

## State at handoff

- `main` at `e26b8d2`, **tree clean**, version **0.8.1**.
- Schema at migration `0010_instance_notify`, on both backends.
- Full suite green including `-race` (SQLite); full store suite green against a
  real MariaDB.
- Running locally: v0.8.1 on `:8080` over plain HTTP against `data/bitt.db` (real
  accounts — do not test against it; use a scratch DB on another port, per the
  standing rule).
- **Not yet done, and it is all that is left for v1:** UI-05, the PWA.
- After UI-05: v1 is feature-complete. The remaining work is release polish — a
  milestone audit against the original intent, then tag `v1.0.0` and let the
  release workflow publish the first image.
