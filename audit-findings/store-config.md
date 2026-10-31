# Audit Findings — Store/Jobs, Config Generation, Backups

Read-only audit of `internal/store`, `internal/jobs`, `internal/provisioning`,
`internal/configgen`, `internal/security/yamlpolicy.go`, `internal/domains`,
`internal/websites`, `internal/backups` in `~/workspace/own-hpanel-build`.

**Concurrency note:** sibling agents were editing this tree during the audit
(`internal/provisioning/provisioning.go`, `internal/jobs/jobs.go`,
`internal/configgen/configgen.go`, `cmd/*` changed at 12:36–12:37 UTC).
Every finding below was re-verified against the final state of the tree.
Defects found in the earlier snapshot that are already fixed (zombie
job-page retry, failed jobs displaying 100%, `MaxAttempts` never enforced,
missing container-ID reconciliation) are listed under "Verified OK /
already fixed" instead of as findings.

Format: `SEVERITY | file:line | problem | evidence | fix`

---

## Scope A — Store / Jobs

### A1 — MEDIUM
`internal/store/store.go:83` + `internal/provisioning/provisioning.go:133-141` |
Jobs left in `queued` are orphaned forever by a panel restart. |
`finishOpen` crash recovery converts only `status='running'` →
`'interrupted'`. Execution is driven by in-process goroutines and
`time.AfterFunc` backoff timers (auto-retry re-queues a job and re-enters
`execute` via `time.AfterFunc`); nothing scans for queued jobs at boot
(`grep` for a resume/requeue path in `cmd/panel/main.go` returns nothing).
A restart during backoff, or while a job waits for the 2-slot semaphore,
leaves a permanent `queued` row: the job-page retry refuses `queued`
("only failed, interrupted or cancelled"), and the row would also block
any future `create_site` enqueue for that site via `HasActiveJobForSite`.
| Fix: at boot, fail or re-dispatch `queued` jobs (interrupted is the
honest state for kinds whose request cannot be reconstructed).

### A2 — MEDIUM
`internal/store/store.go:83` |
Websites stuck in transient statuses are never reconciled on boot. |
Crash recovery touches only `jobs`. A panel crash mid-provisioning leaves
`websites.status='provisioning'` (set at `provisioning.go:126`) or
`'deleting'` (set at `cmd/panel/handlers_sites.go:338`) forever, even
though the corresponding job becomes `interrupted`. |
Fix: in `finishOpen`, reset websites in `provisioning`/`deleting` whose
latest job is `interrupted` to `failed`/previous stable status.

### A3 — MEDIUM
`internal/provisioning/provisioning.go:40-51,78-98` +
`internal/store/store.go:592` |
No mutual exclusion for conflicting per-site jobs. |
`Enqueue` checks `HasActiveJobForSite` only for `create_site`, and that
check matches the *same kind only*
(`WHERE site_id=? AND kind=? AND status IN ('queued','running')`).
`EnqueueSimple` — used for start/stop/restart/delete/backup/restore/
wpcli/apply_config — performs **no** active-job check at all, so a
double-click or a delete racing a backup enqueues two jobs that execute
concurrently against the same container (bounded only by the global
2-slot semaphore). The create check itself is check-then-insert with no
transaction, so concurrent creates can both pass (TOCTOU). |
Fix: enforce one active job per site (any conflicting kind) inside a
transaction or with a partial unique index on `(site_id)` where
`status IN ('queued','running')`.

### A4 — MEDIUM
`internal/store/store.go:577` + `internal/jobs/jobs.go:5-12` +
`internal/provisioning/provisioning.go:138` |
The declared job state machine is neither enforced nor complete. |
`Store.UpdateJob` writes any status string with no `ValidateTransition`
call — the machine in `jobs.go` is advisory only (used by `execute`'s
entry check and `Cancel`). The runner's auto-retry path writes
`running → queued`, an edge the map does not contain (`running` →
{`succeeded`,`failed`,`cancelled`,`interrupted`}), so code and spec
disagree about a transition that now happens routinely. |
Fix: add the `running → queued` (retry-scheduled) edge to
`jobs.transitions` and validate inside `UpdateJob` (needs the current
status — read-then-write in one statement/transaction).

### A5 — MEDIUM
`internal/store/store.go:441-449` |
Database size is never measured; the cached value is permanently 0. |
`UpdateDBSize` has **zero callers** in the repo and the panel never calls
the agent's `OpDBSize` (`grep` over `cmd/panel` returns nothing), yet
`website_databases.size_bytes` is inserted as `DEFAULT 0` at site
creation and `Store.DBSize` is rendered on the website page and detail
views. The housekeeping comment in `cmd/panel/main.go` ("refresh DB
sizes is agent-driven") describes a refresh that does not exist — the
ticker only expires sessions and prunes resource samples. |
Fix: have the panel call `OpDBSize` (e.g. from the detail page load or
the 10-minute housekeeping loop) and persist via `UpdateDBSize`.

### A6 — MEDIUM
`internal/store/store.go:321-345` + `cmd/panel/handlers_sites.go:174` |
Admin-domain collisions are silently swallowed while the site still
claims the alias. |
The create handler checks duplicates only for the primary domain
(`GetWebsiteByDomain(in.Domain)`); `websites.admin_domain` has no UNIQUE
constraint, and the `website_domains` insert for the admin domain uses
`INSERT OR IGNORE` — a collision with another site's domain is dropped
silently, yet the site row keeps the `admin_domain` and configgen still
publishes a Traefik admin router for it. Two sites can therefore emit
routers for the same `Host()` with nondeterministic Traefik selection. |
Fix: validate the admin domain against `website_domains` (any kind) at
create time and fail loudly; stop ignoring the insert error.

### A7 — LOW
`internal/store/store.go:44-100` + `internal/store/migrations/001_init.sql` |
Migrations are unversioned and non-transactional. |
There is a single `001` migration and no `schema_migrations` table; the
SQLite path executes the whole script on every boot and the MySQL path
re-runs split statements tolerating only duplicate-object errors
(1050/1060/1061). Idempotency relies entirely on `CREATE TABLE IF NOT
EXISTS`, so a future column addition or index change can never be
applied to existing databases, and a failure mid-script leaves a
half-applied schema (self-healing only for whole missing tables). |
Fix: add a version table and numbered, transactional migrations
(SQLite: wrap in `BEGIN…COMMIT`; the current single-file approach is
acceptable only while the schema never evolves).

### A8 — LOW
`internal/store/store.go:321-360` |
`CreateWebsite` is a non-transactional multi-insert with ignored errors. |
The website row, `website_domains` rows and the `website_databases` row
are written by separate statements, the latter two with `_, _ =`
(error discarded). A failure part-way leaves a website with no database
record (size lookups, rotation flows then misbehave) and no signal. |
Fix: wrap the three inserts in a transaction and propagate errors.

### A9 — LOW
`internal/store/store.go:453` |
`SaveConfigVersion` version allocation races. |
`SELECT COALESCE(MAX(version),0)` then `INSERT version+1` is not atomic;
two concurrent saves for one site collide on
`UNIQUE(website_id, version)` and one save fails with a raw DB error. |
Fix: allocate the version inside a transaction (or retry on the unique
violation).

### A10 — LOW
`internal/store/migrations/001_init.sql` (tables `ssl_checks`,
`login_attempts`, `deployment_events`, `jobs`) |
Unbounded table growth. |
No code anywhere deletes from `ssl_checks`, `login_attempts`,
`deployment_events` or `jobs` (`grep` for the DELETEs returns nothing);
only `resource_samples` (7 days) and expired sessions are pruned. On the
production MariaDB these tables grow without limit. |
Fix: add retention pruning to the existing 10-minute housekeeping loop.

### A11 — LOW
`internal/store/store.go:598` |
`AddEvent` performs a non-atomic dual write. |
The event insert and the derived `deployments` insert
(`INSERT … SELECT … FROM jobs`) are separate statements; the second
silently inserts zero rows if the job row vanished in between, and the
`deployments` row snapshots the job's kind/status at event time, so it
drifts from the job's final state. `deployment_events.job_id` also has
no foreign key, so events can orphan. |
Fix: wrap both inserts in one transaction; treat `jobs` as the source of
truth and drop or regenerate the derived row.

### A12 — LOW
`internal/store/store.go:784` |
Audit-log redaction is a small first-match denylist. |
`Redact` handles only `password=`, `PASSWORD=`, `db_password=`,
`secret=` — and only the **first** occurrence of each (`strings.Index`).
`token=`, `api_key=`, JSON forms (`"password":"…"`) and any second secret
in the same detail string pass through into `audit_logs` unredacted.
Job *events* (`AddEvent`) are not redacted at all. |
Fix: redact with a case-insensitive regex over repeated key/value
patterns and apply it in `AddEvent` too.

---

## Scope B — Config generation

### B1 — MEDIUM
`internal/security/yamlpolicy.go:15-47,55-118` |
Compose policy validation is allowlist-by-omission: whole field classes
are never checked. |
The decoder struct silently ignores unknown fields
(`dec.KnownFields(false)`), and several decoded fields are never
validated:
- **Networks** — top-level `networks:` is decoded (`map[string]any`,
  line 29) and never inspected; the service struct has no `networks`
  field at all. Edited YAML can attach a site to arbitrary existing
  Docker networks (another site's per-site network, the stack's
  `own-hpanel-db`).
- **Labels** — only `own-hpanel.managed=true` presence is asserted
  (line 114). A forged `own-hpanel.site-id` of another site, or Traefik
  labels with a `Host()` rule for a victim/production domain (router
  hijack), pass.
- **Volumes** — only sources starting with `/` are prefix-checked
  (lines ~99-112). Named
...[truncated 11467 chars]