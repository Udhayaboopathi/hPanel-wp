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
