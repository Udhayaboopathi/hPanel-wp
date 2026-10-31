# CI/CD — GitHub Actions

Own hPanel ships two workflows:

| Workflow | File | Runner | Secrets | Purpose |
|---|---|---|---|---|
| CI | `.github/workflows/ci.yml` | GitHub-hosted `ubuntu-latest` | none | `go test`, `go vet` (incl. integration tags), build both binaries, shell + YAML + compose sanity |
| Deploy Own hPanel | `.github/workflows/deploy.yml` | **self-hosted, on the production server itself** | yes (below) | Deploy/redeploy the panel + agent, with production-protection checks |

## Why the deploy runner is on the server

The production host already runs GitHub Actions self-hosted runners. The
deploy job therefore executes **locally on the server**: it uses Docker and
systemd directly. There are **no SSH steps and no SSH secrets** — nothing
connects to the server from outside. A guard step refuses to run anywhere
else: it fails unless `redberyl-traefik` and the shared MariaDB container
are visible in `docker ps` on the runner host.

Triggers:

- **Push to `main`** — automatic deploy, gated by the `production`
  environment (below), so environment protection rules (e.g. required
  reviewers) and environment secrets apply.
- **Manual** — Actions → *Deploy Own hPanel* → *Run workflow* (requires the
  `confirm = deploy` choice).

Deploys are serialised with the `production-deploy` concurrency group
(`cancel-in-progress: false`): a second deploy queues instead of
interleaving with a running one.

## GitHub Secrets — one secret: `HPANEL_ENV` (primary)

Create ONE secret in **Settings → Secrets and variables → Actions** (a
repository secret, or a secret of the **`production` Environment** — the
deploy job declares `environment: production`, and environment secrets
override repository secrets of the same name). Its value is a whole
`.env`-style blob, one `KEY=value` per line:

```ini
PANEL_DOMAIN=<the panel hostname, e.g. panel.example.com>
DB_HOST=db.krwebsolution.com
DB_EXTERNAL_PORT=3024
DB_ADMIN_USER=WordPress_common_DB
DB_ADMIN_PASSWORD=<the WordPress_common_DB password>
PANEL_DB_NAME=hpanel_panel
DEPLOY_DIR=/opt/own-hpanel/app
```

Rules the workflow applies when parsing the blob:

- A line is split on the **first** `=` only, so values may contain `=`,
  `$`, `)`, `+`, etc. Unknown keys are ignored with a warning; malformed
  lines (no `=`) are skipped with a warning.
- Only key **names** are ever printed (`HPANEL_ENV keys loaded: …`); no
  value is ever echoed.
- Recognised keys: `PANEL_DOMAIN`, `SERVER_IP`, `DB_HOST`,
  `DB_EXTERNAL_PORT`, `DB_ADMIN_USER`, `PANEL_DB_NAME`, `DEPLOY_DIR`
  (all optional except `PANEL_DOMAIN` and the password — defaults exist
  for the rest), plus `DB_ADMIN_PASSWORD`, which is special — see below.
- `SERVER_IP` may be added to the blob if needed; default
  `130.210.42.20`.

### The DB password is file-only — never in the compose `.env`

`DB_ADMIN_PASSWORD` is **never** exported to the job environment and
**never** written into the compose `.env` file. The workflow writes its
value directly (and only) to
`/opt/own-hpanel/secrets/db_admin_password.txt` (0600, via sudo), before
the agent is installed/restarted and before the panel-DB check runs. The
panel container receives it through `DB_ADMIN_PASSWORD_FILE` and a
read-only mount of `/opt/own-hpanel/secrets` (see `compose.yaml`).

Why: the real password contains `$` characters. Docker Compose
interpolates `$…` sequences in `.env` values and in compose `${...}`
substitutions, which would silently corrupt the password. Keeping it in
a file sidesteps interpolation entirely. The compose `.env` the workflow
writes contains only: `PANEL_DOMAIN`, `SERVER_IP`, `DB_HOST`,
`DB_EXTERNAL_PORT`, `DB_ADMIN_USER`, `PANEL_DB_NAME`,
`PANEL_STORE_DRIVER=mysql`.

### Fallback: individual secrets

If the `HPANEL_ENV` secret is empty/absent, the workflow falls back to
individual secrets of the same key names (`PANEL_DOMAIN`,
`DB_ADMIN_PASSWORD`, and optionally `SERVER_IP`, `DB_HOST`,
`DB_EXTERNAL_PORT`, `DB_ADMIN_USER`, `PANEL_DB_NAME`, `DEPLOY_DIR`). The
password still goes file-only to the same path, and the compose `.env`
still excludes it. A deploy with neither `HPANEL_ENV` nor
`DB_ADMIN_PASSWORD` set fails early with an error naming both options.

### Secrets that are NOT used

`SERVER_IP`, `SERVER_USERNAME` and `SEVER_KEY` may exist as repository
secrets from other setups — **this workflow does not read any of them**
(the `SERVER_IP` *key inside the `HPANEL_ENV` blob* is a different thing
and is used). The deploy runner runs on the server itself, so there is
no SSH step and no SSH credential of any kind. They can be left in place
or deleted; neither affects the deploy.

The deploy also uses one non-secret constant: the shared DB container name
`construction-mariadb` (env `DB_CONTAINER` in the workflow).

## Cloudflare notes (panel + DB records)

- **Panel record (e.g. `hpanel.example.com`): Proxied (orange cloud) is
  fine** — it is ordinary HTTPS website traffic. Once the origin has a
  valid Let's Encrypt certificate (Traefik issues it), set the
  Cloudflare SSL/TLS mode to **Full (strict)**; until then the deploy's
  HTTPS health check may warn (SSL-pending) without failing.
- **DB record (`db.krwebsolution.com`): must be DNS-only (grey cloud)**
  for direct MariaDB TCP access. Cloudflare's proxy forwards HTTP(S)
  only — it does **not** forward MariaDB traffic, so a proxied DB record
  can never serve direct database connections, whatever port is used.
  Note the panel and the WordPress sites do **not** use this record for
  their normal traffic anyway: the deploy's panel-database check runs
  inside the DB container via `docker exec`, and WordPress sites connect
  over their internal Docker networks. The external endpoint is only for
  testing/admin tools — which is exactly the traffic that needs the
  record to be DNS-only (and the host to publish the matching port).

## Runner setup (on the server, one time)

1. Install a GitHub self-hosted runner for this repository on the server
   (Settings → Actions → Runners → New self-hosted runner), with labels
   `self-hosted, linux` (the defaults). Run it as a user that can use
   Docker.
2. That user must be in the `docker` group and have **passwordless sudo**
   (needed only to install `/opt/own-hpanel/bin/agent` and the
   `own-hpanel-agent` systemd unit) — or run the runner as root.
3. Install `rsync`, `git`, `curl`, `openssl` (`apt install rsync curl openssl`).
4. Go does **not** need to be installed on the runner: the workflow uses
   `actions/setup-go` (1.23), which provisions the toolchain on self-hosted
   runners too. The panel container itself is built by Docker from the
   repo `Dockerfile`.
5. Make sure the runner's working area and `/opt/own-hpanel` have >5 GB
   free disk (the workflow aborts below that).

## What a deploy does (in order)

1. Guard: correct host, docker/rsync/sudo available.
2. Snapshot `docker ps` + the IDs of the 10 pre-existing production
   containers (the protection baseline).
3. Prerequisite checks: `redberyl-internal` network exists, Traefik
   running, shared MariaDB running, >5 GB disk. Abort changes nothing.
4. Load configuration: parse the `HPANEL_ENV` secret (or the individual-
   secret fallback), export the known non-secret keys for later steps,
   and write the DB admin password file-only to
   `/opt/own-hpanel/secrets/db_admin_password.txt` (0600). This happens
   before the agent is installed/restarted and before the DB check.
5. `rsync` the checkout to `DEPLOY_DIR` (excluding `.git` and `.env`).
6. Write `DEPLOY_DIR/.env` (chmod 600; key names only in the log). It
   contains `PANEL_DOMAIN`, `SERVER_IP`, `DB_HOST`, `DB_EXTERNAL_PORT`,
   `DB_ADMIN_USER`, `PANEL_DB_NAME`, `PANEL_STORE_DRIVER=mysql` — and
   **no password** (see the file-only note above).
7. `go test ./...`, `go vet ./...`; build panel + agent binaries.
8. Install the agent binary to `/opt/own-hpanel/bin/agent`, generate the
   agent token if missing (0600, never printed), install/refresh
   `own-hpanel-agent.service`, `daemon-reload`, restart **the agent
   only** (verifying the password file written in step 4 exists first).
9. Ensure the panel database exists: `docker exec` **into the shared DB
   container** (`construction-mariadb`) runs
   `CREATE DATABASE IF NOT EXISTS …; SELECT 1;` as the common account,
   with `MYSQL_PWD` loaded silently from the password file. This is
   intentionally *not* an external TCP connection — the external record
   is Cloudflare-proxied and the published port differs, so external
   connectivity is a warning-only reachability note in the same step,
   never a deploy requirement. If the common account lacks the
   privileges, the step fails with the exact GRANT to run — it never
   guesses or escalates.
10. `docker compose build panel` + `docker compose up -d` — panel-owned
    container only.
11. Health: wait for the panel container to be running/healthy, then
    `curl https://$PANEL_DOMAIN/healthz`. A non-200 here with a healthy
    container is reported as a **warning** (usually SSL-pending: Let's
    Encrypt not yet issued or DNS not yet pointing at the server), not a
    failed deploy.
12. Snapshot again and diff the 10 protected container IDs — **the deploy
    fails if any changed or disappeared**. Snapshots are uploaded as the
    `deploy-snapshots-<run>` artifact, and a summary is written to the run.

The workflow never runs `docker prune`, never runs `compose down` for
anything except during a manual rollback, never modifies Traefik, and
never creates/stops/restarts the shared MariaDB container.

## First deploy

1. Create the `HPANEL_ENV` secret (template above) and the DNS A record
   for `PANEL_DOMAIN`. (If the DB record should serve direct external
   MariaDB access, set it to DNS-only — see the Cloudflare notes.)
2. Register the self-hosted runner (above).
3. Actions → *Deploy Own hPanel* → *Run workflow* → confirm `deploy`.
4. Watch the run; download the snapshot artifact and confirm the
   protection check passed.
5. Open `https://<PANEL_DOMAIN>` → first-run administrator setup (save the
   recovery codes and TOTP secret — shown once).
6. Create a test WordPress site on a staging subdomain and verify it
   end-to-end before real sites (see docs/DEPLOYMENT.md §4).

## Rollback

- **Re-run a previous state**: check out the previous good commit and run
  the workflow manually on it (Actions → Run workflow → select the branch/
  commit's ref where available), or simpler: `git revert <bad-commit>` and
  push to `main` — the normal deploy then rolls the panel forward to the
  reverted code.
- **Panel offline only**: on the server, `docker compose stop panel` and
  `systemctl stop own-hpanel-agent` — websites and the shared DB are
  unaffected (docs/RECOVERY.md).
- Rollback never needs to touch Traefik, the shared MariaDB, or any
  pre-existing container.

## Security notes

- **DB external port 3024 is a testing convenience.** A MariaDB port
  reachable from the internet is a real attack surface: in production,
  firewall it to your own IPs (or close it entirely once testing is done —
  WordPress containers reach the DB over their Docker network on 3306, and
  only the panel's metadata store uses the external endpoint; if you close
  the port, point `DB_HOST`/`PANEL_STORE_DSN` at an internal route
  instead).
- **Rotate any credential that has ever been shared in chat** (including
  the DB admin password) and update the GitHub Secret afterwards — the
  workflows read only the current Secret value.
- The common DB account is powerful: it can create databases/users for
  provisioning. Keep its password only in GitHub Secrets and the two
  0600 files on the server; the agent falls back to using it directly for
  a site's connection only when it cannot create a dedicated user, and
  records that loudly (job warning event + panel audit entry).
