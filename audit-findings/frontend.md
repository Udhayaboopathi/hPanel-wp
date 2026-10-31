# Frontend / Backend Wiring Audit — Own hPanel

Scope: every route registered in `cmd/panel/main.go`, every form/button/link in
`web/templates/*.html`, the handlers behind them, the provisioning runner they
call, and whether template fields are actually populated. Read-only audit; no
source files were modified.

Snapshot note: the working tree contained uncommitted fixes from a parallel
workstream that landed *during* this audit (job retry re-execution, failed-job
progress, Repair Website). All findings below were re-verified against the
final tree state after those changes settled; items that the in-tree fixes
resolved are listed under "Verified correct", not as findings.

**Totals: 13 findings — 1 high, 6 med, 6 low.**

---

## Findings

### HIGH

**H1 | Cancel Job does not cancel a job that is already running**
- Location: `internal/provisioning/provisioning.go:221-231` (Runner.Cancel) + `internal/provisioning/provisioning.go` execute() (no status re-check after the agent call starts); button at `web/templates/job.html:15`.
- Problem: Cancel only writes `status='cancelled'` to the job row. The executing goroutine runs the agent call on a `context.Background()`-derived context that nothing cancels, and its subsequent `UpdateJob` calls (progress events, then the terminal succeeded/failed write) overwrite the "cancelled" status. The code comment claims "cancellation is cooperative: the agent checks the request context between provisioning steps" — no such linkage exists in the code.
- Evidence: `jobs.go` transitions allow `running → cancelled`, so the button is offered and accepted for running jobs; execute() checks the job status only once, before starting (ValidateTransition gate). Cancellation only has an effect while the job is still waiting on the runner semaphore, not while it is actually executing.

### MED

**M1 | Retry-scheduled jobs are stranded in "queued" forever if the panel restarts during the backoff**
- Location: `internal/provisioning/provisioning.go:139` (`time.AfterFunc(backoff, …)` re-execution); `internal/store/store.go:85-88` (boot crash recovery).
- Problem: Transient-failure retries are re-executed by an in-memory timer after setting the job's status to "queued". Boot recovery only flips `running → interrupted`; a job left in "queued" by a panel restart during the backoff window is never executed, never marked interrupted, and the job page's Retry handler rejects "queued" jobs — only Cancel remains available.
- Evidence: store recovery SQL updates only `WHERE status='running'`; no worker anywhere polls the jobs table for queued rows (execution is exclusively via goroutines started at enqueue time or by the AfterFunc timer).

**M2 | Restore runs without the pre-restore safety backup the code promises**
- Location: `cmd/panel/handlers_sites.go:485` (comment) vs `:492` (the restore enqueue).
- Problem: `siteRestore` carries the comment "Safety: always take a fresh backup before a restore" and then enqueues `restore_site` directly — no backup job is created anywhere in the handler. A restore overwrites the site's files/database with no fresh fallback artifact, contrary to the stated safety behaviour.
- Evidence: the handler body between the comment and the redirect contains only agentSpec construction, audit, and the single restore enqueue.

**M3 | Backup completion is reconciled by guessing the artifact from a directory glob; the agent's authoritative result is discarded**
- Location: `cmd/panel/handlers_sites.go` reconcileBackup (glob at `:432`: `filepath.Glob(BackupsDir/<siteID>-*.tar.gz)`, newest by lexical sort wins).
- Problem: The agent returns the exact artifact path, SHA-256 and size in its response (`cmd/agent/main.go:915-916` sets `resp.BackupPath` / `resp.BackupSHA256`), but nothing in the panel or runner ever reads those fields (grep: no consumer of `Response.BackupPath/BackupSHA256/BackupSize` outside the agent). The panel instead re-derives the artifact as "the newest `<siteID>-*.tar.gz` in the backups directory". If that file is not the one this job produced (e.g. overlapping backups for the same site — possible because EnqueueSimple has no per-site guard, see L2 — or a job that reports success without a new file), the backup record is marked `ready` pointing at the wrong artifact with a self-computed hash, and a later Restore would restore that wrong artifact.
- Evidence: reconcileBackup's own comment admits "The agent response is not persisted; reconciliation therefore re-derives the artifact from the backups directory naming convention".

**M4 | Post-job watcher goroutines die with the panel; nothing reconciles their records on boot**
- Location: `cmd/panel/handlers_sites.go` siteDelete (status-polling goroutine) and reconcileBackup; `internal/store/store.go:85-88`.
- Problem: Both the backup-record reconciler and the delete-site row remover are fire-and-forget goroutines with multi-minute poll loops. If the panel restarts while they run, they are gone and no boot-time reconciliation exists for backups or websites (crash recovery touches only jobs). Result: backups stuck in `creating` and websites stuck in `deleting` indefinitely. Additionally, the delete watcher does not handle a job ending as `interrupted` (exactly what a restart produces), and on delete failure it resets the website status to `ready` unconditionally — a previously `stopped`/`failed` site is resurrected as `ready` in the UI even though its container state did not change.
- Evidence: delete watcher loop handles only `succeeded` / `failed` / `cancelled`; boot recovery updates only the `jobs` table.

**M5 | Monitoring page's "Live per-container stats" section does not show stats**
- Location: `cmd/panel/handlers_misc.go:117` (calls `OpDiscovery`); `web/templates/monitoring.html:19` (heading "Live per-container stats (from the provisioning agent)"); agent implementation `cmd/agent/main.go:336-358`.
- Problem: The panel fetches the agent's Discovery op, whose output is a container/network *inventory* (`name image state managed=true`, `network: …` lines) — no CPU or memory figures. That inventory is rendered under a heading that promises live per-container statistics. (The real stats op, OpStats, exists and is used correctly by the website detail page.)
- Evidence: discovery() builds lines from `ListContainers`/`ListNetworks` only; `StatsOneShot` is never invoked on this path.

**M6 | Several Settings fields are write-only: saved, never consumed; the S3 offsite-backup path is entirely unwired**
- Location: `cmd/panel/handlers_misc.go` settingsPage/settingsSave (only reader of `GetSetting` in the codebase, besides its definition); `web/templates/settings.html`; `web/templates/create.html:24`; `internal/backups/s3.go:19`.
- Problem: `backup_retention`, `default_profile`, `alert_email`, `s3_endpoint`, `s3_region`, `s3_bucket` are persisted by the Settings form but have no consumer anywhere: retention is never enforced (no pruning code reads it), the create wizard ignores `default_profile` (its profile `<select>` hardcodes `lightweight` as selected), no alert code reads `alert_email`, and `backups.UploadS3` has zero callers — backups always stay local even though the website Backups tab tells the user to "configure a genuinely external S3 destination in Settings for off-server copies" (detail.html, backups tab).
- Evidence: repo-wide grep — `GetSetting` is called only by settingsPage itself; `UploadS3` is defined and never invoked.

### LOW

**L1 | WP-CLI tab comment claims disruptive updates "get a backup first"; the handler takes none**
- Location: `cmd/panel/handlers_sites.go` siteWP (comment above the enqueue).
- Problem: The comment says "Potentially disruptive updates get a backup first (operator can watch both jobs)", but the handler enqueues only the wpcli job. Impact is limited because the WP tab itself instructs the operator to back up first and provides a manual "Backup website first" button — the code comment, not the UI, overstates the behaviour.

**L2 | Lifecycle/backup/delete actions have no per-site conflict guard against duplicate submission**
- Location: `internal/provisioning/provisioning.go` EnqueueSimple (used by start/stop/restart/backup/delete/restore/wpcli/apply-config/repair) vs Enqueue (create_site only).
- Problem: Only `create_site` checks `HasActiveJobForSite`. Every other operation can be double-submitted (double-click, list page + detail page) and runs as parallel jobs against the same site; the runner semaphore caps *global* concurrency at 2 but does not serialize per site. Backup/delete races aggravate M3/M4.

**L3 | Create wizard discards everything the operator typed on any server-side validation failure**
- Location: `cmd/panel/handlers_sites.go` createSubmit `fail()` (passes `Data.Input` back); `web/templates/create.html` (no binding of `.Data.Input` anywhere).
- Problem: The handler carefully returns the submitted input for re-rendering, but the template never populates a single field from it, so any failure (duplicate domain, capacity check, DB-name clash) returns a blank step-1 form, including the generated admin password the operator may not have saved.

**L4 | Websites list: filter offers a status that can never occur, misses one that now can, and the sort select loses its value**
- Location: `web/templates/websites.html:8` and `:10`.
- Problem: The status filter includes `installing`, which no code path ever assigns to a website (statuses actually set: provisioning, ready, dns_pending, stopped, failed, deleting, missing). Conversely the backend now sets status `missing` (provisioning execute, on agent not-found for lifecycle ops) but the filter has no `missing` option, so such sites cannot be filtered for. Separately, the sort `<select>` has no `selected` retention (unlike the status select), so after filtering by "Domain A–Z" the control displays "Newest first" while the list is domain-sorted.

**L5 | Audit log "Next →" pagination link is always rendered**
- Location: `web/templates/audit.html:7`.
- Problem: Unlike the websites list (which guards both links), the audit page renders "Next →" unconditionally; on the last page it links to an empty page with no indication the log ended.

**L6 | htmx is loaded on every page but never used**
- Location: `web/templates/layout.html` (`<script src="/static/htmx.min.js" defer>`); `web/static/app.js` (only a no-op `htmx:configRequest` listener).
- Problem: No template contains a single `hx-*` attribute (repo-wide grep). The library is dead payload on every page load. Informational only.

---

## Verified correct (checked in code, current tree)

- **Route ↔ control mapping is complete.** All 62 route registrations in `cmd/panel/main.go` have handlers, and every form action / link target in all 19 templates resolves to a registered route with the correct method. No form posts to a non-existent path; all POST redirects land on pages that exist (job detail, website detail tabs, /jobs, /websites, /account, /settings, /login).
- **Job Retry now genuinely re-executes.** `POST /jobs/{id}/retry` (`cmd/panel/handlers_misc.go` jobRetry) enqueues a *fresh* job via EnqueueSimple for kinds whose request is reconstructable from the website record (start/stop/restart/repair) and redirects to the new job; kinds carrying unstored operator input (create_site, wpcli, backup_site, restore_site) are refused with precise guidance instead of a fake "queued" state. `Runner.Retry` in the provisioning package now returns an explanatory error rather than flipping status without an executor.
- **Failed jobs no longer display 100%.** The runner tracks the furthest progress event actually reached and fails the job at that value; 100 is written only on success (provisioning execute()).
- **Repair Website is fully wired end to end.** Route `POST /websites/{site}/repair` → `siteRepair` handler (accepts the optional new admin password, min length 12, matching the form) → a conditional "Repair website" card rendered on the website page exactly when status is `failed` or `missing` (`web/templates/detail.html`), and lifecycle ops that hit an agent not-found set the website status to `missing` so the card appears. The create_site retry guidance in the job page flash text points at this real control.
- **Create wizard JS is real.** Step navigation triggers a genuine `POST /websites/preview` fetch and renders the server's validated response; the preview's `dns_records` shape (`[][3]string`) matches the indexing in `app.js`; the static DNS block's `{{.ServerIP}}` is populated because `render()` sets `pageData.ServerIP` for every page. Generate-password buttons call `GET /api/generate-password` and consume its plain-text response exactly as implemented.
- **Dashboard widgets are backed.** Metric polling targets (`#m-cpu`, `#m-mem`, `#m-disk`) exist in dashboard.html and every key read by `app.js` is returned by `GET /api/metrics`; counts, recent events, agent status and backup counts all come from the store/host sampler. (The unused `MariaOK`/`TraefikOK` fields in dashData are not referenced by the template, which explains their absence honestly in prose.)
- **WP-CLI controls are real.** Every action button in the WordPress tab exists in `wordpress.Build`'s allowlist (core_version, core_check_update, core_update, plugin_list, plugin_update_all, theme_list, theme_update_all, cache_flush, health, plugin_activate/deactivate/update), invalid actions are rejected by the panel before enqueueing, and reset-admin-password's `user_reset_password` action is special-cased by the agent ahead of `Build` (cmd/agent/main.go) using `ResetPasswordArgs`.
- **Website detail tabs are populated.** The handler fills EnvMap (from the latest config version), Configs/Latest, Backups, Jobs, LatestJob, SSL (latest check), DBSize and live container stats parsed from the agent's OpStats output in exactly the format the agent emits; the overview's failed/running job banner links to the real job page.
- **Websites list actions are real.** Open Website / Open Admin / Manage / Restart / Backup on every row map to existing routes; pagination links preserve q/status and are correctly conditional.
- **Auth and account flows match.** setup/login/totp/recover/account form field names match their handlers one-for-one; TOTP setup/enable/disable, recovery regeneration, password change (with session revocation) and logout-all all perform the stated operation; setup_done receives RecoveryCodes/TOTPSecret/TOTPURL as the template expects.
- **Backup download is guarded.** It requires the backup to belong to the site and be `ready`, and enforces path containment under the backups directory before serving.
- **Settings page fields match the save handler** (all six editable keys round-trip), and backup_retention input is range-validated server-side — the defect in M6 is the absence of consumers, not the form wiring.
