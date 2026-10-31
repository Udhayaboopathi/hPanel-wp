# CI/CD — GitHub Actions

Own hPanel ships two workflows:

| Workflow | File | Runner | Secrets | Purpose |
|---|---|---|---|---|
| CI | `.github/workflows/ci.yml` | GitHub-hosted `ubuntu-latest` | none | `go test`, `go vet` (incl. integration tags), build both binaries, shell + YAML + compose sanity |
| Deploy Own hPanel | `.github/workflows/deploy.yml` | **self-hosted, on the production server itself** | yes (below) | Deploy/redeploy the panel + agent, with production-protection checks |

## Why the deploy runner is on the server

The production host already runs GitHub Actions self-hosted runners. The
deploy job therefore executes **locally on the server**, using Docker
directly. There are **no SSH steps and no SSH secrets** — nothing
connects to the server from outside — and the workflow **never
escalates privileges on the host**: it runs start to finish as the
runner user, and everything that needs root-level effect is done inside
one-shot Docker containers (see "Zero-sudo deploys" below). A guard step
refuses to run anywhere else: it fails unless `redberyl-traefik` is
visible in `docker ps` on the runner host. (The stack's dedicated DB
container is created by compose during the deploy itself, so it is not
part of the host proof.)

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
**never** written into the compose `.env` file. The workflow stages its
value directly into
`/opt/own-hpanel/secrets/db_admin_password.txt` (0600, root-owned) via a
one-shot Docker container, before the stack starts and before the
panel-DB check runs. That host file serves two readers: the agent
container (it runs as root and gets the file through a read-only bind
mount) and the MariaDB container (via the compose `secrets:` mount).

The **panel does not read the host file** — it runs as uid/gid 10001
and cannot open a root-owned 0600 file, and the host secrets directory
is deliberately **not** mounted into the panel container. Instead, a
dedicated deploy step stages the panel its **own copy** at
`/data/db_admin_password.txt` inside the panel-data volume
(`own-hpanel-panel-data`), owned by 10001 with mode 0600; the panel's
`DB_ADMIN_PASSWORD_FILE` points there (see `compose.yaml`). The staging
runs on every deploy, so a rotated secret propagates on the next run.

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

The deploy also uses one non-secret constant: the DB container name
`wordpress-mariadb` (env `DB_CONTAINER` in the workflow) — the stack's own
dedicated MariaDB container, defined in `compose.yaml`.

### The MariaDB root password is server-generated — never a GitHub secret

The root password of `wordpress-mariadb` is **not** part of `HPANEL_ENV`
and never travels through GitHub. A deploy step generates it on the
server if missing (`openssl rand -hex 24`) into
`/opt/own-hpanel/secrets/mariadb_root_password.txt` (0600, value never
printed) — the same pattern as the agent token. It becomes the
container's root password on the data volume's **first initialization**
and is used by the deploy's ensure step (root `docker exec`) to create
the panel database and sync the common user's provisioning grants. Both
passwords reach MariaDB only as file-based compose `secrets:`; neither is
ever a compose literal or a `${...}` interpolation. The ensure step
reads both passwords back **out of the MariaDB container's own secret
mounts** (`docker exec … cat /run/secrets/…`) instead of reading the
host files — the host files are root-owned 0600 in a root-owned
directory the runner user cannot even traverse, and the container
already holds exactly the values MariaDB itself uses.

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
  The port itself **is** published — by the stack's own
  `wordpress-mariadb` container (`${DB_EXTERNAL_PORT:-3024}:3306` in
  compose.yaml) — so with a DNS-only record, external testing/admin
  tools reach the dedicated DB container directly. Note the panel and
  the WordPress sites do **not** use this record for their normal
  traffic anyway: they connect to `wordpress-mariadb` over internal
  Docker networks, and the deploy's panel-database step runs as root via
  `docker exec` into that container. The external endpoint is only for
  testing/admin tools — exactly the traffic that needs the record to be
  DNS-only.

## Zero-sudo deploys — the Docker-staging pattern

The deploy workflow contains **no privilege-escalation calls at all**.
It runs start to finish as the unprivileged runner user, and the
runner's **only** requirement is membership of the `docker` group. No
sudoers configuration is needed, expected, or used.

This was not the first design. The workflow originally escalated
privileges for a handful of host operations (installing the agent
binary and its systemd unit, writing root-owned secret files), based on
the assumption that the runner had passwordless escalation rights. In
production that assumption proved unreliable in ways that were not
visible from the repository: the server accepted some command forms
without a password and demanded one for others (plain file installs
worked; the same tool with owner/group flags for a foreign uid/gid did
not; some read-only service queries did not), and the failures changed
as the workflow evolved, because each new step risked using an
unaccepted form. Rather than keep probing an opaque configuration, the
dependency was eliminated:

- **Every privileged file operation runs inside a one-shot Docker
  container** (`docker run --rm … alpine:3.20 sh -c '…'`): writing all
  the root-owned secret files (DB admin password, agent token, MariaDB
  root password), creating the host directories the stack bind-mounts,
  staging the panel's credential copy into its data volume, staging the
  panel's agent-token copy into `/run/own-hpanel`, group-sharing
  `websites`/`backups` with the panel gid, and fixing `DEPLOY_DIR`
  ownership when the sync directory is not runner-writable.
- **Read-only container queries need no privilege** and are run
  directly (`docker ps/inspect/logs`, plus the agent's bound-socket
  entry in `/proc/net/unix` for liveness).
- **The agent itself is a Docker Compose service now** (see below), so
  nothing needs to be installed into the host's binary directories or
  service manager at all.

**Rule for the workflow: if a new step needs a privileged host
operation, give it the Docker treatment.** Do not reintroduce host
privilege escalation in any form — that is the failure mode this design
exists to avoid.

### The agent as a container

The provisioning agent is the `agent` service in `compose.yaml`
(container `own-hpanel-agent`). It is the **only** container with
`/var/run/docker.sock` mounted — the panel, the database and every
WordPress container must never get it — and it is exactly as privileged
as the host root systemd service it replaces: Docker socket access *is*
host-root equivalence, by design, because provisioning WordPress sites
means creating containers, networks and volumes. The containment story
is unchanged in substance:

- the agent has **no network listener at all** — it serves only its
  Unix socket in `/run/own-hpanel` (bind-mounted into both the agent
  and the panel containers), and every call needs the agent token;
- the socket is mode 0666 so the panel's uid 10001 can connect, but the
  token file itself stays 0600 (agent's copy) / 640 group-10001
  (panel's staged copy);
- it runs with `cap_drop: ALL` and `no-new-privileges`, with tight
  memory/CPU limits, and it re-validates every request against its
  allowlist, exactly as the host service did.

The old systemd unit (`deployment/own-hpanel-agent.service`) is kept as
a **deprecated alternative** for manual installs that deliberately
prefer a host process; the deploy workflow and `install.sh` no longer
use it, and it must never run at the same time as the compose agent
(both bind the same socket path).

## Runner setup (on the server, one time)

1. Install a GitHub self-hosted runner for this repository on the server
   (Settings → Actions → Runners → New self-hosted runner), with labels
   `self-hosted, linux` (the defaults). Run it as a user that can use
   Docker.
2. That user must be in the `docker` group. **Nothing else**: the
   workflow uses no sudo at all (see "Zero-sudo deploys" above), so no
   sudoers setup is needed — and any sudoers entries left over from the
   earlier design are simply never invoked.
3. Install `rsync`, `git`, `curl`, `openssl` (`apt install rsync curl openssl`).
4. Go does **not** need to be installed on the runner: the workflow uses
   `actions/setup-go` (1.23), which provisions the toolchain on self-hosted
   runners too. The panel container itself is built by Docker from the
   repo `Dockerfile`.
5. Make sure the runner's working area and `/opt/own-hpanel` have >5 GB
   free disk (the workflow aborts below that).

## What a deploy does (in order)

1. Guard: correct host (existing Traefik visible), docker/rsync
   available. The workflow runs entirely as the runner user — there is
   no privilege-escalation step anywhere in the run.
2. Snapshot `docker ps` + the IDs of the 10 pre-existing production
   containers (the protection baseline).
3. Prerequisite checks: `redberyl-internal` network exists, Traefik
   running, >5 GB disk. Abort changes nothing. (No database prerequisite:
   the stack creates its own DB container in step 11.)
4. Load configuration: parse the `HPANEL_ENV` secret (or the individual-
   secret fallback), export the known non-secret keys for later steps,
   and stage the DB admin password file-only into
   `/opt/own-hpanel/secrets/db_admin_password.txt` (0600) via a one-shot
   Docker container. This happens before the stack starts (compose
   mounts the file as a secret, and the agent container reads it
   through its own read-only mount).
5. `rsync` the checkout to `DEPLOY_DIR` (excluding `.git` and `.env`).
   If the directory is missing or not runner-writable, a one-shot
   Docker container creates it and hands it to the runner user first.
6. Write `DEPLOY_DIR/.env` (chmod 600; key names only in the log). It
   contains `PANEL_DOMAIN`, `SERVER_IP`, `DB_HOST`, `DB_EXTERNAL_PORT`,
   `DB_ADMIN_USER`, `PANEL_DB_NAME`, `PANEL_STORE_DRIVER=mysql` — and
   **no password** (see the file-only note above).
7. `go test ./...`, `go vet ./...`. That is all the host-side build
   there is: the panel and agent binaries are built by Docker Compose
   from the repo `Dockerfile` in step 11 — nothing is compiled into,
   or installed onto, the host.
8. Prepare host state — **Docker one-shots only**: create the
   `/opt/own-hpanel` directories the stack bind-mounts and
   `/run/own-hpanel` (the shared socket directory, 755); verify the DB
   admin password file from step 4 exists; generate the **agent
   token** if missing (0600, never printed) and stage the panel's copy
   to `/run/own-hpanel/agent.token` (640, group 10001); generate the
   MariaDB **root** password file if missing
   (`/opt/own-hpanel/secrets/mariadb_root_password.txt`, 0600, never
   printed, never overwritten — see the root-password section above).
9. Stage the panel's credentials and shared directories — **Docker
   only**: create the `own-hpanel-panel-data` volume if needed and
   copy the DB admin password into it as `/data/db_admin_password.txt`
   (0600, owned by the panel's uid/gid 10001) — the copy the panel
   actually reads; group-share `/opt/own-hpanel/websites` (2770) and
   `…/backups` (2750) with the panel gid. Runs on every deploy, so
   secret rotations propagate.
10. `docker compose build` + `docker compose up -d` — the whole stack
    defined in compose.yaml, i.e. exactly three panel-owned services:
    `wordpress-mariadb` (image pulled by compose; on first boot of its
    empty volume the entrypoint creates the panel database and the
    common user from the file secrets, and
    `deployment/mariadb-init/01-grants.sh` upgrades that user to full
    provisioning admin of this container), `own-hpanel` (starts once
    the DB is healthy) and `own-hpanel-agent` (the provisioning agent;
    the only service with the Docker socket mounted). The step then
    waits until `wordpress-mariadb` reports healthy.
11. Verify the agent: the `own-hpanel-agent` container must be running
    and its socket path must appear as bound in `/proc/net/unix`; the
    container's last 50 log lines are printed either way, and the step
    fails if the agent is not up within 30 s.
12. Ensure the panel database and provisioning grants — **after** the
    stack is up: `docker exec` **into `wordpress-mariadb` as root**
    (`MYSQL_PWD` loaded silently — both passwords are read out of the
    container's own `/run/secrets/…` mounts via `docker exec … cat`,
    because the host copies are root-owned in a directory the runner
    cannot traverse, and the container holds exactly the values
    MariaDB itself uses) feeds it a
    generated SQL file (0600 temp file, shredded afterwards) that runs
    `CREATE DATABASE IF NOT EXISTS …`, `CREATE USER IF NOT EXISTS …` /
    `ALTER USER … IDENTIFIED BY …` (syncing the common user's password
    with the current secret) and the provisioning `GRANT … WITH GRANT
    OPTION` for `'%'` and `'localhost'`, followed by a `SELECT 1` check
    **as the common user**. This repeats the first-init setup
    idempotently because initdb scripts run only on an empty volume.
    The common user's password is embedded in the SQL file as a
    single-quoted literal with single quotes doubled (SQL-standard
    escaping); passwords containing CR/LF are refused outright. Nothing
    connects over external TCP for this — external endpoint reachability
    remains a warning-only note in the same step.
13. Health: wait for the panel container to be running/healthy, then
    `curl https://$PANEL_DOMAIN/healthz`. A non-200 here with a healthy
    container is reported as a **warning** (usually SSL-pending: Let's
    Encrypt not yet issued or DNS not yet pointing at the server), not a
    failed deploy.
14. Snapshot again and diff the 10 protected container IDs — **the deploy
    fails if any changed or disappeared**. Snapshots are uploaded as the
    `deploy-snapshots-<run>` artifact, and a summary is written to the run.

The workflow never runs `docker prune`, never runs `compose down` for
anything except during a manual rollback, never modifies Traefik, and
never touches the pre-existing production MariaDB (`construction-mariadb`)
in any way — it only appears in the protected before/after ID comparison.

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
- **Panel offline only**: on the server, `docker compose stop panel
  agent` — websites and the dedicated DB container are unaffected
  (docs/RECOVERY.md).
- Rollback never needs to touch Traefik, the dedicated MariaDB's data
  volume, or any pre-existing container.

## Security notes

- **DB external port 3024 is a testing convenience.** The published port
  (`${DB_EXTERNAL_PORT:-3024}:3306` on `wordpress-mariadb`) exists for
  outside testing/admin tools only — **nothing in the stack uses it**:
  the panel, the agent and every WordPress container reach MariaDB
  internally by container name on 3306. A MariaDB port reachable from
  the internet is a real attack surface: in production, firewall it to
  your own IPs or close it entirely once testing is done (remove the
  `ports:` mapping in compose.yaml — in-stack behaviour is unchanged).
- **Rotate any credential that has ever been shared in chat** (including
  the DB admin password) and update the GitHub Secret afterwards — the
  workflows read only the current Secret value.
- The common DB account is powerful **on `wordpress-mariadb` only**: it
  holds full grants there (with grant option) so the agent can create
  databases/users for provisioning — it has no rights on any other
  database server. Keep its password only in GitHub Secrets and the 0600
  file on the server; the agent falls back to using it directly for
  a site's connection only when it cannot create a dedicated user, and
  records that loudly (job warning event + panel audit entry). The
  container's root password is a separate, server-generated secret
  (also a 0600 file, never in GitHub) used only by deploys and recovery.
