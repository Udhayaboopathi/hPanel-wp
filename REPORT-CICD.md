# Own hPanel — Shared-DB rework + GitHub CI/CD report

Implemented locally on 2026-10-31 in `~/workspace/own-hpanel-build` on top of
commit `1806bf3` (working tree left uncommitted for review/commit by the
owner). **No connection of any kind was made to the production server**, no
GitHub push was performed, and **no real password or token appears anywhere
in the tree** — the DB credential exists only as secret/env *names*
(`DB_ADMIN_PASSWORD`, `DB_ADMIN_PASSWORD_FILE`, GitHub Secrets).

## User decisions implemented

1. **One MariaDB container only** — the pre-existing `construction-mariadb`
   (MariaDB 11.4). The panel no longer deploys a `wordpress-mariadb`
   container anywhere: not in compose, not in the installer, not in the
   agent. All WordPress sites **and the hPanel panel itself** use that
   single shared DB server.
2. **Endpoints** — external/testing: `db.krwebsolution.com:3024`
   (`DB_HOST`/`DB_EXTERNAL_PORT`). Internal: WP containers reach the DB
   container by name on their per-site Docker network, port 3306
   (`WORDPRESS_DB_HOST` = the container name, default `construction-mariadb`).
3. **Common admin user, per-site isolation by default** — the agent
   connects as the env-configured common user (default
   `WordPress_common_DB`, password only via `DB_ADMIN_PASSWORD_FILE`
   preferred / `DB_ADMIN_PASSWORD`) and creates **one database + one
   dedicated least-privilege user per site**. On a privilege error
   (CREATE USER/GRANT denied) it falls back to creating only the database
   and connecting that site as the common user, emitting a **warning job
   event** and a **panel audit entry** (`db.common_user_fallback`) — never
   silently.

## What changed

### A. Agent (`cmd/agent`) + new `internal/dbconfig`
- Hardcoded `wordpress-mariadb` and the MariaDB **root** credential are
  gone. All DB settings come from `internal/dbconfig.FromEnv()`:
  `DB_CONTAINER` (default `construction-mariadb`), `DB_HOST`,
  `DB_EXTERNAL_PORT`, `DB_ADMIN_USER`, `DB_ADMIN_PASSWORD_FILE` /
  `DB_ADMIN_PASSWORD`. Legacy `AGENT_MARIADB_ROOT_PASSWORD*` names are
  still honoured as a fallback during upgrade.
- `mariadbExec` now goes through `docker.Client.ExecDB` and authenticates
  as the common admin user — never root.
- `internal/docker`: new `ExecDB` is the narrow exec-only exception for the
  shared DB container: it refuses any argv that is not the `mariadb` SQL
  client, refuses per-site containers, and offers no lifecycle path. The
  managed-label `Guard` still refuses all stop/restart/remove operations
  on the (unmanaged) DB container.
- Agent-level enforcement `refuseDBContainer()` guards create/lifecycle/
  delete/apply-config: the DB container can never be a mutation target.
  The agent may only (a) exec the SQL client in it and (b)
  `docker network connect` it to a new per-site network. In `createSite`
  the old "must be labelled panel-managed" check was replaced with
  "must exist and be running" (the panel will not start it either).
- Delete: a fallback site drops **only its database** (`DropDatabaseOnlySQL`)
  — the common user is never dropped. Rotate-password refuses to rotate the
  common user (it would change every site's credential at once).
- Backup/restore one-shot `mariadb:11.4` client containers now target the
  configured internal DB host.
- Tests: `cmd/agent/main_test.go` (refuse-DB-container incl. custom names,
  common-user detection, argv uses admin user and never `-uroot`),
  `internal/dbconfig` tests (defaults, password-file precedence, bad port,
  privilege-error detection for MariaDB codes 1044/1045/1142/1227 + text),
  `internal/database` test that the database-only drop never drops a user.

### B. Panel store (`internal/store`, `cmd/panel`)
- Store is now driver-aware: `PANEL_STORE_DRIVER=sqlite|mysql`
  (SQLite stays the local-dev default). Production mode uses
  `github.com/go-sql-driver/mysql` (v1.8.1, the only new dependency).
- DSN: `PANEL_STORE_DSN` if set, otherwise built by `store.BuildMySQLDSN`
  from `DB_HOST` + `DB_EXTERNAL_PORT` + `PANEL_DB_NAME` (default
  `hpanel_panel`) + the common admin user/password envs.
- MySQL migration `migrations/001_init_mysql.sql` is the dialect
  translation of `001_init.sql` (AUTO_INCREMENT, bounded VARCHAR(191) for
  indexed/unique columns, DOUBLE for REAL, no literal defaults on TEXT
  columns). Table and column names are identical; all store queries use
  `?` placeholders and portable SQL. The only dialect-specific queries are
  `INSERT IGNORE` vs `INSERT OR IGNORE` and the settings upsert
  (`ON DUPLICATE KEY UPDATE` vs `ON CONFLICT`) — both branch on the driver.
- The MySQL migration runner splits statements and tolerates
  already-exists errors so panel restarts are safe.
- Tests: migration-lint asserts both migrations create the **same table
  set** and the **same columns** for core tables; statement-splitter, DSN
  builder, unknown-driver rejection, SQLite behaviour unchanged (all
  pre-existing store tests still pass).
- Panel site configs now take `WORDPRESS_DB_HOST` from `DB_CONTAINER`
  (was a hardcoded literal in two handlers).

### C. Compose / env / installer
- `compose.yaml`: `wordpress-mariadb` service, its volume, the `panel-db`
  network and the compose secret are **removed**. The panel is the only
  service; it receives the DB/store env vars by `${...}` substitution from
  the server `.env` (CI-written from GitHub Secrets), joins the external
  `redberyl-internal` network, and carries a header comment stating the DB
  is the pre-existing shared server, never managed by this stack.
- `.env.example` / `config/panel.env.example`: `PANEL_DOMAIN`, `SERVER_IP`,
  `DB_CONTAINER`, `DB_HOST`, `DB_EXTERNAL_PORT`, `DB_ADMIN_USER`,
  `DB_ADMIN_PASSWORD` (empty, marked SECRET), `PANEL_STORE_DRIVER=mysql`,
  `PANEL_DB_NAME`.
- `deployment/install.sh`: MariaDB secret generation removed; discovery
  now reports the shared DB container state and does a **warning-only**
  TCP check of `DB_HOST:DB_EXTERNAL_PORT`; prerequisites abort if the
  shared DB container is missing or not running (the panel never starts
  it). Before/after snapshot logic unchanged.
- `deployment/own-hpanel-agent.service`: DB env now points at
  `DB_ADMIN_PASSWORD_FILE=/opt/own-hpanel/secrets/db_admin_password.txt`
  with the shared-DB defaults. `deployment/compose.mariadb-reference.yaml`
  deleted (obsolete). README/SECURITY/DEPLOYMENT/RECOVERY docs and the
  dashboard/detail/settings templates updated to the shared-DB wording.

### D. GitHub CI/CD (`.github/workflows/`, `docs/CICD.md`)
- `ci.yml` — push to main + PRs, GitHub-hosted, zero secrets: unit tests,
  vet (both tag sets), both binary builds, `bash -n` on deployment
  scripts, YAML parse of compose + workflows, `docker compose config`
  sanity with dummy values only.
- `deploy.yml` — *Deploy Own hPanel*: push to main **and**
  `workflow_dispatch` with a required `confirm` choice; job declares
  `environment: production`; `runs-on: [self-hosted, linux]` (the runner
  is on the server itself) so there are **no SSH steps**; concurrency
  group `production-deploy` with `cancel-in-progress: false`. Steps:
  host guard (docker + Traefik + shared DB must be visible, passwordless
  sudo check) → BEFORE snapshot → prerequisite checks (network, Traefik,
  DB running, >5 GB disk; abort changes nothing) → rsync to
  `secrets.DEPLOY_DIR` (default `/opt/own-hpanel/app`, excluding `.git`/`.env`)
  → write `.env` from Secrets only (chmod 600, values never echoed) →
  setup-go 1.23 → test/vet/build → install agent binary + token (generated
  once, 0600) + DB password file (0600) + systemd unit, restart **agent
  only** → ensure panel DB via one-shot `mariadb:11.4` client container
  (`CREATE DATABASE IF NOT EXISTS` + usability check; privilege failure
  prints the exact GRANT to run and fails) → `docker compose build/up`
  (panel only) → container health wait + `curl https://$PANEL_DOMAIN/healthz`
  where non-200 with a healthy container is a **warning (SSL-pending)**,
  not a failure → AFTER snapshot + ID diff of the 10 protected production
  containers, **failing the deploy if any changed/disappeared** → artifact
  upload + step summary. No `docker prune`, no foreign `compose down`, no
  Traefik changes anywhere.
- `docs/CICD.md`: required-secrets table (standard names), repo-vs-
  `production`-environment secrets, runner setup (docker group,
  passwordless sudo, rsync; Go via `actions/setup-go`), first-deploy
  checklist, rollback (re-run previous commit / `git revert`), and the
  security warning that **port 3024 is for testing** — firewall/IP-restrict
  it in production — plus “rotate any credential ever shared in chat”.

## Verification (actual runs in this VM)

- `go test -count=1 ./...` — **all 13 packages pass** (was 11; added
  `internal/dbconfig` and agent tests now live in `cmd/agent`). New tests
  listed in sections A/B above.
- `go vet ./...` and `go vet -tags integration ./...` — clean. `gofmt -l`
  — clean.
- Binaries (static, stripped): panel 13,025,432 B (~12.4 MiB, grew due to
  the MySQL driver), agent 6,770,840 B (~6.5 MiB).
- Panel smoke test (SQLite default): boots, `/healthz` → `ok`,
  `/setup` → 200, templates parse.
- Workflows: both YAML files parse with PyYAML (the `on:` key shows as the
  usual YAML-1.1 boolean quirk; GitHub parses it correctly), and **every
  embedded `run:` script (19 blocks) passes `bash -n`** after substituting
  GitHub expressions. `bash -n` also passes for `deployment/install.sh`
  and `scripts/dev-agent.sh`; `compose.yaml` parses as YAML.

## Honest limitations

1. **The common DB user's privileges are unverified.** Whether
   `WordPress_common_DB` can CREATE DATABASE / CREATE USER / GRANT is only
   knowable against the real server, which this phase must not contact.
   The code handles both outcomes (dedicated per-site users, or the loud
   common-user fallback), and the deploy workflow's panel-DB step fails
   with actionable GRANT instructions if the account cannot create/use
   `hpanel_panel`.
2. **The panel-on-MySQL path was not live-tested.** There is no Docker or
   MySQL/MariaDB server in this VM and installing one was out of scope.
   What is verified: compilation, vet, the migration-lint (same tables +
   core columns), the statement splitter, the DSN builder, and the full
   shared query set against SQLite. Risks to watch on first deploy:
   MariaDB-specific behaviour of `CREATE INDEX IF NOT EXISTS` (supported
   on MariaDB ≥10.5; the runner additionally tolerates duplicate-object
   errors), FK clauses written as column `REFERENCES` (parsed by MariaDB;
   the app enforces relationships itself and relies mainly on unique
   constraints + cascades in SQLite), and DSN password characters
   (`PANEL_STORE_DSN` exists as the escape hatch for passwords containing
   `@`, `/` or `?`).
3. **Fallback-site credential bookkeeping:** if a site falls back to the
   common user, its site `.env` and WordPress work correctly, but the
   panel's stored per-site DB credential still names the never-created
   dedicated user, so later panel-initiated DB operations for that one
   site (backup dump credentials, DB size) would fail authentication
   until an operator reconciles it. The fallback is surfaced in the job
   events and audit log so this is visible, and `websites.db_user` is now
   an updatable field to make reconciliation possible.
4. **Port 3024 exposure** is a property of the server setup, not of this
   code: see the warning in docs/CICD.md. The panel metadata store uses
   that external endpoint from the panel container because the panel is
   not on a network shared with the DB container; closing the port later
   requires giving the panel an internal DB route.
5. Pinned image tags and the full live provisioning path remain
   unverified against a real Docker host, exactly as in REPORT.md.

## HPANEL_ENV + internal-DB fix (2026-10-31)

Follow-up to match how the GitHub repo was actually configured: all values
live in ONE repository secret, `HPANEL_ENV` (a whole `.env` blob), the
individual secrets do not exist, and the external DB endpoint
`db.krwebsolution.com` is Cloudflare-proxied while the host publishes a
different port — so no deploy step may depend on external DB TCP.

### Changes

- **`.github/workflows/deploy.yml`**
  - New early step **"Load configuration (HPANEL_ENV secret, or individual
    secrets)"** (after checkout/prerequisites, before the compose `.env`
    write). Blob mode: each line is split on the **first** `=` only
    (values may contain `$`, `=`, `)`, `+`); known keys
    (`PANEL_DOMAIN`, `SERVER_IP`, `DB_HOST`, `DB_EXTERNAL_PORT`,
    `DB_ADMIN_USER`, `PANEL_DB_NAME`, `DEPLOY_DIR`) are exported via
    `$GITHUB_ENV`; unknown keys and malformed lines warn and are skipped;
    only key **names** are printed. `DB_ADMIN_PASSWORD` is handled
    file-only: `printf '%s'` into
    `/opt/own-hpanel/secrets/db_admin_password.txt` (`install -d` + sudo,
    chmod 600) — never exported, never echoed. A blob without a password
    line fails the step with a clear error.
  - Fallback mode (empty `HPANEL_ENV`): the same known keys are exported
    from individual secrets and the password file is written from
    `secrets.DB_ADMIN_PASSWORD`; if neither source has a password the
    step fails naming both options.
  - **Step order:** the password file is written in the load step — before
    the agent install/restart step and before the ensure-panel-DB step.
    The agent step no longer handles the password at all (it only asserts
    the file exists) — its now-obsolete `secrets.DB_ADMIN_PASSWORD`
    write was removed.
  - **Compose `.env` write:** no longer contains `DB_ADMIN_PASSWORD` (nor
    `DB_CONTAINER`, which compose defaults). It contains exactly:
    `PANEL_DOMAIN`, `SERVER_IP` (default `130.210.42.20`), `DB_HOST`,
    `DB_EXTERNAL_PORT`, `DB_ADMIN_USER`, `PANEL_DB_NAME`,
    `PANEL_STORE_DRIVER=mysql`.
  - **Ensure-panel-DB step:** the external one-shot client containers
    (`docker run … mariadb -h $DB_HOST -P $DB_EXTERNAL_PORT`) were
    replaced by `docker exec -e MYSQL_PWD $DB_CONTAINER mariadb -u… -e
    "CREATE DATABASE IF NOT EXISTS …; SELECT 1;"`. `MYSQL_PWD` is loaded
    silently from the 0600 password file (command substitution, no echo,
    no `set -x`; passed to `docker exec` by name, not in argv). Failure
    prints the actionable privilege/GRANT error and fails the step. A new
    **warning-only** external reachability check (`timeout 3` `/dev/tcp`
    to `DB_HOST:DB_EXTERNAL_PORT`) explains that external exposure is for
    testing/admin only and never fails the deploy.
  - The `Verify panel health` and `Deploy summary` steps dropped their
    `secrets.*` `env:` blocks for `PANEL_DOMAIN`/`PANEL_DB_NAME` — those
    secrets do not exist in this setup and an empty `env:` value would
    have shadowed the parsed `HPANEL_ENV` values from `$GITHUB_ENV`.
  - No panel domain is hardcoded anywhere in the workflow; it comes from
    the secret. The unused `SERVER_IP`/`SERVER_USERNAME`/`SEVER_KEY`
    repository secrets are not referenced (no SSH by design).
- **`compose.yaml`** — panel service: `DB_ADMIN_PASSWORD: ${...}` removed;
  now `DB_ADMIN_PASSWORD_FILE: /opt/own-hpanel/secrets/db_admin_password.txt`
  plus a read-only volume `/opt/own-hpanel/secrets:/opt/own-hpanel/secrets:ro`.
  Panel code path verified: `cmd/panel` `openStore` (mysql) builds its DSN
  from `internal/dbconfig.FromEnv()`, which reads `DB_ADMIN_PASSWORD_FILE`
  before any env var — so the file-only password reaches the panel store
  unchanged (no Compose interpolation anywhere on the path). Everything
  else in compose is identical.
- **`docs/CICD.md`** — secrets section rewritten: primary = single
  `HPANEL_ENV` secret with the exact key template (placeholder password);
  file-only password handling and the `$`-interpolation rationale;
  individual-secrets fallback; explicit note that
  `SERVER_IP`/`SERVER_USERNAME`/`SEVER_KEY` secrets are unused; new
  Cloudflare notes (panel record Proxied is fine, use Full (strict) once
  the origin cert exists; the DB record must be DNS-only for direct
  MariaDB TCP because Cloudflare does not forward it; internal panel/site
  traffic does not use that record); "What a deploy does" list updated to
  the new step order and the docker-exec DB check.

### Validation (actual runs in this VM, fake values only)

- `python3` PyYAML parse: `deploy.yml`, `ci.yml`, `compose.yaml` — all OK.
- `bash -n` on every embedded `run:` script (after `${{ }}` substitution):
  **20 scripts, all clean** (14 deploy + 6 CI).
- **Functional parsing test** — the workflow's own extracted "Load
  configuration" and "Write compose .env" scripts were executed locally
  against a FAKE blob whose dummy password contains `$`, `=`, `)` and
  `+`:
  - password file content matches the fake password **byte-for-byte**,
    mode `600`;
  - `PANEL_DOMAIN` and `DEPLOY_DIR` parsed correctly into `$GITHUB_ENV`;
    unknown key ignored with a warning;
  - generated compose `.env` contains the correct `PANEL_DOMAIN`,
    `PANEL_STORE_DRIVER=mysql`, default `SERVER_IP`, and **no
    `DB_ADMIN_PASSWORD` key and no password value**;
  - fallback mode (empty blob + individual secret) also verified:
    password file byte-for-byte, `PANEL_DOMAIN` exported.
  Result: **ALL PARSING TESTS PASSED** (both modes).
- `go test ./...` — all packages pass (Go code untouched by this fix);
  `go vet ./...` and `go vet -tags integration ./...` clean.
- Tree scanned: no real password or token string anywhere; the
  only password-like strings are the previously documented placeholders
  in examples/docs and the run-time fake values used in /tmp test files
  (outside the tree; the local test password file under /opt was removed
  after the test).

### Limitations / unchanged risks

- The docker-exec DB check, agent restart and compose bring-up still
  cannot be exercised in this VM (no Docker); they are validated by
  YAML/bash syntax checks and the parsing test only. First real run is
  still the production runner — the workflow's guard + before/after
  protected-container diff remain the safety net.
- The panel's own MySQL store connection at runtime still uses
  `DB_HOST:DB_EXTERNAL_PORT` (documented in the previous section); this
  fix only changed how the *deploy* verifies/creates the panel database
  and how the password is delivered, per scope.

## Dedicated DB container in compose (supersedes the shared-DB wiring)

Per the user's explicit instruction ("ADD DB image download and run and
assign all config details in docker yaml file"), the design above —
agent/panel using the pre-existing `construction-mariadb` — is
**reversed**. The stack now runs its OWN database container, defined
completely in `compose.yaml`:

- **compose.yaml**: new service `wordpress-mariadb` (`mariadb:11.4`,
  pulled and started by `docker compose up`). ALL configuration is in
  the YAML: entrypoint env (`MARIADB_DATABASE=${PANEL_DB_NAME}`,
  `MARIADB_USER=${DB_ADMIN_USER}`, both passwords via
  `MARIADB_*_PASSWORD_FILE` pointing at compose file `secrets:` from
  `/opt/own-hpanel/secrets/` — file-based only, no literal or
  `${}`-interpolated passwords, because the common user's password
  contains `$`), tuned `command:` flags, published port
  `"${DB_EXTERNAL_PORT:-3024}:3306"` (the requested testing exposure —
  firewall it in production; nothing in-stack uses it), named volume
  `own-hpanel-wordpress-mariadb-data`, private `panel-db` network,
  healthcheck, resource limits, hardened caps, labels
  `own-hpanel.managed=true` / `own-hpanel.component=mariadb`.
- **Provisioning grants**: the entrypoint grants the common user rights
  only on the panel DB, so `deployment/mariadb-init/01-grants.sh`
  (bind-mounted into `/docker-entrypoint-initdb.d/`) grants it
  `ALL PRIVILEGES ON *.* WITH GRANT OPTION` — on THIS container only;
  the script documents that scope explicitly. Rationale: the agent
  provisions per-site databases/users with that account, and this
  container serves only hPanel + its WordPress sites.
- **Initdb caveat (and its cover)**: `/docker-entrypoint-initdb.d`
  scripts run ONLY on first initialization of an empty data volume.
  For already-initialized volumes the deploy workflow's ensure step
  repeats the same setup idempotently on every deploy (below).
- **Panel**: `depends_on: wordpress-mariadb (service_healthy)`, joins
  `panel-db`, and its DB endpoint is pinned to literals in the compose
  `environment:` block (`DB_HOST=wordpress-mariadb`,
  `DB_EXTERNAL_PORT="3306"`, `DB_CONTAINER=wordpress-mariadb`) so
  in-stack traffic always uses internal Docker DNS, overriding the
  external values in the server `.env`. This also supersedes the
  previous section's limitation: the panel store no longer connects via
  the external endpoint.
- **Code**: `dbconfig.DefaultContainer` is now `wordpress-mariadb`
  (tests updated). `docker.ExecDB` now REFUSES any container that is not
  the panel-managed MariaDB (requires `own-hpanel.managed=true` +
  `own-hpanel.component=mariadb`, still mariadb-argv-only, still no
  lifecycle path). The agent's `refuseDBContainer` is unchanged in
  behaviour — the agent still never stops/restarts/removes the DB
  container; its lifecycle belongs to Compose alone.
  `construction-mariadb` returns to being a purely protected, untouched
  production container: it stays in the workflow's `PROTECTED_CONTAINERS`
  before/after ID diff, but nothing execs into it, network-connects it,
  or depends on it. The systemd unit sets
  `Environment=DB_CONTAINER=wordpress-mariadb` explicitly.
- **Deploy workflow**: `DB_CONTAINER: wordpress-mariadb`. The guard no
  longer requires a pre-running DB container (compose creates it). A new
  step generates `/opt/own-hpanel/secrets/mariadb_root_password.txt`
  (`openssl rand -hex 24`, sudo tee, 600, never printed, never
  overwritten) before compose up — the root password is server-generated
  and stored only in that file; it is set on the volume's first
  initialization. `docker compose up -d` now starts the whole stack and
  the step waits for `wordpress-mariadb` to report healthy. The ensure
  step moved AFTER compose up: as **root** (MYSQL_PWD from the root
  password file, passed by name) it feeds a generated SQL file (0600
  mktemp, shredded via trap) via `docker exec -i … mariadb -uroot`
  stdin: `CREATE DATABASE IF NOT EXISTS`, `CREATE USER IF NOT EXISTS` /
  `ALTER USER … IDENTIFIED BY …` (syncs secret rotations) and the
  provisioning grants for `'%'` + `'localhost'`, then verifies with a
  `SELECT 1` as the common user. The user password is embedded as a
  single-quoted literal with single quotes doubled; CR/LF passwords are
  refused. External endpoint reachability stays warning-only.
- **install.sh** mirrors this: discovery reports the dedicated
  container (absent is fine — compose creates it), the hard prerequisite
  on a pre-existing DB is gone, and `--apply` generates
  `mariadb_root_password.txt` if missing.
- **Docs**: README, SECURITY.md, docs/CICD.md (incl. the root-password
  section), docs/DEPLOYMENT.md, docs/RECOVERY.md, `.env.example`,
  `config/panel.env.example`, the agent systemd unit, and the panel's
  dashboard/detail/settings templates were updated to the
  dedicated-container design and the reversal is called out.

### Validation (this change)

- `go test ./...` — all packages pass (dbconfig/agent/configgen tests
  updated to the `wordpress-mariadb` default); `go vet ./...` clean;
  `gofmt` clean.
- Python YAML parse: compose.yaml + ci.yml + deploy.yml — OK.
- `bash -n`: all workflow `run:` scripts (after `${{ }}` substitution),
  install.sh, mariadb-init/01-grants.sh, dev-agent.sh — clean.
- SQL-generation escaping test, run against the deploy workflow's OWN
  extracted ensure-step script (docker stubbed, secret paths redirected
  to /tmp): with a FAKE password containing a single quote, `$`, `=`,
  `)` and `+`, the generated SQL contains the password with its single
  quote doubled (`''`) and every other byte exact, both GRANTs present,
  and no raw unescaped form anywhere; a password containing a newline
  makes the step exit 1 with the rotation error BEFORE any SQL file is
  generated. The initdb grants script was likewise run against a stubbed
  `mariadb` binary: both GRANT statements emitted for the default user,
  and a non-identifier `MARIADB_USER` is refused (exit 1).
- Still NOT exercised here (no Docker in this VM): the actual compose
  bring-up, MariaDB first-init (entrypoint user/DB creation + the
  initdb grants script) and the root-exec ensure step against a real
  container. First real run remains the production runner, guarded by
  the before/after protected-container diff; if the ensure step's root
  login fails because a pre-existing volume was initialized with a
  different root password, the step fails loudly with that exact
  remediation.

## Scoped-sudo constraint + Docker staging (2026-10-31)

### What the real server taught us

The runner user's passwordless sudo on the production host is scoped to
specific command **forms**. Across real deploys, these forms are proven
to run: plain `sudo install -d -m <mode> <paths…>`,
`sudo install -m 755/644 <file> <dest>`, `sudo tee`, `sudo chmod`,
`sudo test -f/-s`, and `sudo systemctl daemon-reload|enable|restart`
(the earlier DEPLOY_DIR `install -d` with `-o`/`-g` set to the runner's
**own** user/group also worked). These forms **fail** with
`sudo: a password is required`: `sudo systemctl status/show …`,
`sudo install … -o root -g 10001 …`, and by extension `sudo chown …` /
`sudo cat` on the root-owned secret files.

The previous approach group-shared the host DB-password file with the
panel's gid (`install -m 640 -o root -g 10001`) so the panel container
could read it through a bind mount of `/opt/own-hpanel/secrets`. That
sudo form is rejected on the real host, so the approach died in the
"Load configuration" step even though every local validation passed —
the sudoers file lives only on the server and cannot be inspected from
here.

### The pattern now used

**Host-sudo calls in the workflow use ONLY the proven forms.** Every
other privileged operation goes through a **one-shot Docker container**
(`docker run --rm … alpine:3.20 sh -c '…'`): the runner is in the
`docker` group, and Docker needs no sudo. Concretely:

- `write_pw` reverted to the proven trio:
  `install -d -m 750` (plain) + `tee` + `chmod 600`. The host file
  `/opt/own-hpanel/secrets/db_admin_password.txt` is again 0600
  root-only and serves exactly two readers: the root agent and the
  MariaDB compose secret.
- New workflow step **"Stage panel credentials and shared dirs
  (docker, no sudo)"** (after source sync, before compose up):
  creates the `own-hpanel-panel-data` volume (the exact name
  compose.yaml declares for the panel, so compose adopts it) and
  stages the panel's **own copy** of the credential into it as
  `/data/db_admin_password.txt` (0600, uid/gid 10001) via
  `docker run -v own-hpanel-panel-data:/data
  -v /opt/own-hpanel/secrets:/secrets:ro alpine … install …`.
  The same step group-shares `websites` (2770) and `backups` (2750)
  with gid 10001. It runs on every deploy, so secret rotations
  propagate to the panel's copy.
- The agent-token copy to `/run/own-hpanel/agent.token` (640,
  root:10001) likewise moved from `sudo install -o/-g` to a one-shot
  Docker container.
- The ensure-DB step no longer `sudo cat`s the host password files
  (that form demanded a password in a real run). Both passwords are
  read out of the dedicated MariaDB container's **own** compose-secret
  mounts: `docker exec wordpress-mariadb cat
  /run/secrets/mariadb_root_password` and `…/db_admin_password`, with a
  loud failure on an empty read. The proven `$SUDO test -s` existence
  checks on the host files stay.
- **compose.yaml**: the panel's `DB_ADMIN_PASSWORD_FILE` is now
  `/data/db_admin_password.txt`, and the
  `/opt/own-hpanel/secrets` bind mount was **removed from the panel
  service entirely** — the panel no longer reads host secrets at all.
  (Its agent token still arrives via `/run/own-hpanel/agent.token`.)
- **deployment/install.sh** mirrors this for manual installs: it runs
  as root (no scoped sudo), so it may `chgrp 10001` the secrets
  directory directly, and it now performs the same Docker one-shot
  staging (volume create + credential install into `/data`,
  websites/backups group-share, token copy group 10001) before
  `docker compose up`.
- **docs/CICD.md** documents the constraint and the pattern in a new
  "Scoped sudo on the runner" section, and the deploy step list gained
  the staging step (steps renumbered to 14).

### Validation (actual runs in this VM)

- Python YAML parse: deploy.yml, ci.yml, compose.yaml — all OK.
- `bash -n` on every deploy/ci `run:` script after `${{ }}`
  substitution (22 scripts) and on install.sh — all clean.
- Grep proof over deploy.yml: no `$SUDO` line contains `chown`,
  `-g 10001`, `status` or `show`; the remaining distinct `$SUDO` forms
  are exactly the proven set (listed in the handoff for this change).
- `go test ./...` — unchanged Go surface; still green.

### Limitations / unchanged risks

- The sudoers drop-in itself is still not visible from here; the
  workflow now treats it as an opaque allowlist and stays inside the
  forms that production runs have demonstrated. If a future form is
  ever needed, it must be proven on the host first (or given the
  Docker treatment).
- The one-shot staging containers pull `alpine:3.20` on first use;
  that is a small public image pull on the runner (already the case
  for the MariaDB/Go images).
- The staged copy in the panel-data volume and the host secret can
  drift between deploys if the GitHub secret is rotated mid-cycle;
  the next deploy re-stages. The agent always reads the host file,
  so panel/agent credentials agree again after any deploy.

## Zero-sudo redesign — agent becomes a Compose service (supersedes the scoped-sudo design)

### What production taught us (the end of the sudo saga)

The "proven forms" approach above did not hold. The NEXT real deploy
(faa2e1d) failed again with `sudo: a password is required` — this time
in a step whose sudo forms had been classified as proven. The runner's
sudoers configuration accepts some command forms and prompts for
others, and *which* forms prompt is not predictable from the
repository: it changed as the workflow changed, because every step
that touched sudo was a fresh gamble against an opaque allowlist.
Iterating against it cost several deploy cycles.

**Decision: eliminate sudo from the deploy workflow entirely.** The
final `deploy.yml` contains zero sudo invocations (verified by grep,
count 0, including comments). The workflow runs start to finish as the
runner user, whose only privilege channel is Docker (docker group
membership). Anything that previously needed root on the host is now
done inside a one-shot `alpine:3.20` container with bind mounts.

### Changes

- **Agent is a Compose service** (`agent` in compose.yaml, container
  `own-hpanel-agent`, image built from the Dockerfile `agent` target):
  the ONLY service with `/var/run/docker.sock` mounted (with a loud
  warning comment), plus `/run/own-hpanel` (shared socket dir with the
  panel), the secrets dir read-only, and the websites/backups dirs.
  Env mirrors the old systemd unit (socket path, token file, DB
  container/host/port, common admin user + password file, Docker
  socket, site/backup dirs). No Traefik labels, no published ports,
  `panel-db` network only, 128 MB / 0.25 CPU, `cap_drop: ALL`,
  `no-new-privileges`. It binds its socket with mode 0666 as before;
  the token (0600 original in the secrets mount, 640 group-10001 copy
  staged into `/run/own-hpanel` by the workflow) remains the auth gate.
- **deploy.yml**: guard no longer exports or tests any escalation
  mechanism; `write_pw` stages the DB password via a Docker one-shot
  (runner temp file + container-side `install -m 600`); the whole
  systemd step (host binary build, smoke test, unit install,
  daemon-reload/enable/restart, socket polling via systemctl) is
  deleted — the host "Test and build" step is now just `go test` +
  `go vet`, since Compose builds both binaries; a new "Prepare host
  state" step creates the bind-mounted host dirs and `/run/own-hpanel`
  (755), generates the agent token and the MariaDB root password if
  missing (both staged via one-shots), and stages the panel's token
  copy — all before compose up; DEPLOY_DIR creation/ownership for the
  rsync step uses a Docker one-shot when the directory is not
  runner-writable; the ensure-DB step dropped its last host-file
  existence checks (the `docker exec … cat /run/secrets/…` reads with
  empty-checks already fail loudly and cover the missing-file case);
  a new "Verify the agent container and its socket" step polls
  `docker inspect` + `/proc/net/unix` for 30 s and prints the agent's
  logs either way.
- **deployment/own-hpanel-agent.service**: kept, but header-marked as a
  DEPRECATED alternative (manual installs only; never together with
  the compose agent — same socket path).
- **deployment/install.sh**: the systemd install/enable/status block is
  gone; the agent starts as part of `docker compose up`. bash -n clean.
- **docs/CICD.md**: the scoped-sudo section is replaced by "Zero-sudo
  deploys — the Docker-staging pattern" (history, pattern, the
  agent-as-container rationale, and the standing rule: new privileged
  operations get the Docker treatment, escalation is never
  reintroduced); runner setup now requires only docker-group
  membership; the deploy step list is rewritten (14 steps); rollback
  uses `docker compose stop panel agent`. README/SECURITY/
  DEPLOYMENT/RECOVERY got matching one-line corrections.

### Why the privilege story is unchanged

A Docker-socket holder is host-root equivalent whether it is a host
process or a container — the old systemd agent ran as root for the
same reason. Moving it into a container neither grants nor removes
meaningful privilege; it removes the *deployment mechanics* that
needed root on the host (copying a binary into /opt, installing a
unit, restarting a service), because Compose manages the container
through the Docker API the runner can already use. The containment
that matters is unchanged: the agent has no network listener, serves
only an authenticated Unix socket, `cap_drop: ALL`,
`no-new-privileges`, an allowlist with full re-validation, and the
production guard. The panel still never receives the Docker socket.

### Honest notes / limitations

- The systemd unit remains in the repo as a deprecated alternative;
  it is exercised by neither CI nor the deploy workflow anymore, and
  its sandboxing (ProtectSystem etc.) is not re-validated here.
- The compose agent and the panel both mount the host
  `/run/own-hpanel` directory; its creation/permissions are owned by
  the workflow's "Prepare host state" step (a manual `install.sh
  --apply` creates it as root, which also works). If the directory is
  deleted while containers run, the socket disappears until the agent
  container restarts — same failure mode as with the systemd
  RuntimeDirectory, no worse.
- As before, none of this could be run against a live Docker daemon in
  this VM; validation is static (YAML parse, bash -n on all run
  scripts, structural compose assertions, go test) plus the production
  runs recorded above. The first zero-sudo production run is the real
  end-to-end test, and it remains protected by the before/after
  container-ID diff.
