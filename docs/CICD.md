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

## Required GitHub Secrets

Create these in **Settings → Secrets and variables → Actions** — either as
repository secrets or, preferably, as secrets of the **`production`
Environment** (Settings → Environments → production), since the deploy job
declares `environment: production`. Environment secrets override repository
secrets of the same name. Do not put any of these values in the repository,
in workflow files, or in chat.

| Secret | Required | Meaning |
|---|---|---|
| `PANEL_DOMAIN` | yes | Public hostname of the panel, routed by the existing Traefik (e.g. `panel.example.com`). Needs a DNS A record → server IP. |
| `DB_ADMIN_PASSWORD` | yes | Password of the common MariaDB admin account. Written only into the server-side `.env` (chmod 600) and `/opt/own-hpanel/secrets/db_admin_password.txt` (0600). Never echoed by the workflow. |
| `SERVER_IP` | optional | Public IPv4 shown in DNS instructions. Default `130.210.42.20`. |
| `DB_HOST` | optional | External DB endpoint host. Default `db.krwebsolution.com`. |
| `DB_EXTERNAL_PORT` | optional | External DB endpoint port. Default `3024`. |
| `DB_ADMIN_USER` | optional | Common MariaDB admin account. Default `WordPress_common_DB`. |
| `PANEL_DB_NAME` | optional | The panel's own database on the shared server. Default `hpanel_panel`. |
| `DEPLOY_DIR` | optional | Where the source is synced on the server. Default `/opt/own-hpanel/app`. |

The deploy also uses one non-secret constant: the shared DB container name
`construction-mariadb` (env `DB_CONTAINER` in the workflow).

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
4. `rsync` the checkout to `DEPLOY_DIR` (excluding `.git` and `.env`).
5. Write `DEPLOY_DIR/.env` from Secrets (chmod 600; values never printed).
6. `go test ./...`, `go vet ./...`; build panel + agent binaries.
7. Install the agent binary to `/opt/own-hpanel/bin/agent`, generate the
   agent token if missing (0600, never printed), write the DB admin
   password file (0600), install/refresh `own-hpanel-agent.service`,
   `daemon-reload`, restart **the agent only**.
8. Ensure the panel database exists: one-shot `mariadb:11.4` client
   container against `DB_HOST:DB_EXTERNAL_PORT` runs
   `CREATE DATABASE IF NOT EXISTS …` and a `SELECT 1` usability check. If
   the common account lacks the privileges, the step fails with the exact
   GRANT to run — it never guesses or escalates.
9. `docker compose build panel` + `docker compose up -d` — panel-owned
   container only.
10. Health: wait for the panel container to be running/healthy, then
    `curl https://$PANEL_DOMAIN/healthz`. A non-200 here with a healthy
    container is reported as a **warning** (usually SSL-pending: Let's
    Encrypt not yet issued or DNS not yet pointing at the server), not a
    failed deploy.
11. Snapshot again and diff the 10 protected container IDs — **the deploy
    fails if any changed or disappeared**. Snapshots are uploaded as the
    `deploy-snapshots-<run>` artifact, and a summary is written to the run.

The workflow never runs `docker prune`, never runs `compose down` for
anything except during a manual rollback, never modifies Traefik, and
never creates/stops/restarts the shared MariaDB container.

## First deploy

1. Create the Secrets (table above) and the DNS A record for
   `PANEL_DOMAIN`.
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
