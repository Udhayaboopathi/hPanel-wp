# Bug-Fix Report — audit remediation (31 Oct 2026)

All fixes below are in this commit. `go build ./...`, `go vet ./...` and the
full `go test ./...` suite pass (see TEST-REPORT.md). Findings referenced as
FE = frontend audit, SEC = security audit, ST = store/config audit
(full texts in `audit-findings/`).

## A. The incident: container identity, reconciliation, repair

1. **Label-based container resolution** (`internal/docker` `FindBySiteID`,
   `cmd/agent` `resolveSiteContainer`). Containers are resolved by the
   ownership labels `own-hpanel.managed=true` + `own-hpanel.site-id=<id>`
   (WP-CLI helper containers carrying `own-hpanel.ephemeral=true` are
   excluded). The stored name is only a cross-check: a container squatting on
   the expected name with foreign/absent labels is refused (guard category),
   two real matches produce an explicit ambiguity error, and Docker/daemon
   failures stay uncategorised transport errors. Site containers now also
   carry `own-hpanel.resource=wordpress`.
2. **Categorised errors** (`internal/agentproto` `Response.ErrorCategory`:
   `not_found`, `ambiguous`, `guard`, `data_missing`). A missing container
   now reports exactly that, states that database and files are untouched,
   and points at Repair Website; the panel sets the website status to
   `missing` so the repair UI appears.
3. **Repair Website operation** (new agent op `RepairSite`, panel route
   `POST /websites/{site}/repair`, repair card on the website page for
   `failed`/`missing` sites). Repair first verifies the site's files exist
   and its database still exists in MariaDB — if either is gone it aborts
   with `data_missing` instead of inventing an empty replacement — then
   recreates only the container via the idempotent create path
   (`RepairMode`: the original WordPress admin password is never stored, so
   it is optional; if WordPress was already installed the step is skipped,
   if not and no password is supplied, the operator finishes via the
   WordPress setup wizard). If the container already exists, repair is a
   no-op success.
4. **Identity reconciliation.** Successful create/start/stop/restart/repair
   persist the *actual resolved* container ID into the website record.
5. **State-aware lifecycle.** Start on a running container and stop on a
   stopped one are no-op successes (Docker 304s no longer surface as
   failures); restart of a stopped container starts it; restart never
   recreates a missing container. Delete tolerates a missing container and
   still cleans up database/files; logs and apply-config use the same
   resolver.

## B. Job lifecycle truthfulness

6. **Progress on failure (D2).** The runner tracks the furthest progress
   event actually reached and fails the job at that value. 100% is written
   only on success.
7. **Real retry (D4).** The job page's Retry now re-executes reconstructible
   operations (start/stop/restart/repair) as a fresh job and redirects to
   it, linking the old job via an event. Kinds whose inputs are not stored
   (create, WP-CLI, backup, restore, rotate, apply-config) get precise
   guidance instead of a fake queue — for create jobs it directs the
   operator to Repair Website and explains why. `Runner.Retry` no longer
   pretends.
8. **Bounded automatic retry (D5).** Transport-level failures only (agent
   unreachable / call error) retry automatically while
   `attempts < max_attempts`, with `attempts² × 10s` backoff; operational
   failures the agent reported are never blindly retried.
9. **Real cancellation (FE-H1).** Cancel now aborts the in-flight agent call
   via a registered per-job context; the executor re-checks the row when
   the call unwinds and discards the late result instead of overwriting the
   cancellation.
10. **One active job per site (ST-A3 / FE-L2).** `EnqueueSimple` (and create)
    refuse to start while any job for the same site is queued/running —
    across kinds, so restart/delete/backup can no longer race each other.
11. **Crash recovery (ST-A1/A2, FE-M1/M4).** At boot, jobs left `queued` as
    well as `running` become `interrupted`; websites stuck in
    `provisioning`/`deleting` become `failed`; backup rows stuck in
    `creating` become `failed`. The delete watcher also handles
    `interrupted` and resets failed deletions to `failed` (previously
    `ready`, which lied about container state).
12. **State machine alignment (ST-A4).** `jobs.transitions` now includes the
    `running → queued` (retry scheduled) and `queued → interrupted` edges
    the code actually uses.
13. **Authoritative backup records (FE-M3).** On backup success the runner
    records the agent-returned exact path, SHA-256 and size
    (`Store.CompleteBackup`); the panel reconciler keeps its directory glob
    only as a fallback.
14. **Restore safety (FE-M2).** Restore now genuinely takes a fresh backup
    first and chains the restore behind its success (aborting with an audit
    entry if the safety backup fails); the agent verifies the recorded
    SHA-256 of the target artifact **before** extracting anything.

## C. Frontend / wiring (FE)

15. Monitoring page now calls `OpStats` (real CPU/memory figures) instead of
    `OpDiscovery` inventory under a "live stats" heading (FE-M5).
16. DB size is actually measured: the website page calls the agent's
    `OpDBSize`, displays it and persists it via `UpdateDBSize` (ST-A5; it
    previously showed 0 forever).
17. Create wizard keeps the operator's input on validation failure (all
    fields rebound except the password, which is never echoed back into
    HTML), and the resource-profile default now comes from Settings
    `default_profile` (FE-L3, FE-M6 partial).
18. Websites filter: the impossible `installing` status replaced by
    `missing`; the sort select retains its value (FE-L4).
19. Audit-log "Next →" renders only when a next page exists (FE-L5).
20. Removed the unused htmx script (loaded on every page, used nowhere)
    (FE-L6). Corrected two comments that promised behaviour the code did
    not have (FE-L1, restore safety — the latter is now real, see 14).

## D. Store / config (ST)

21. **Admin-domain collisions fail loudly (ST-A6).** Create validates the
    admin alias and primary domain against every site's domain *and* alias;
    `CreateWebsite` no longer discards insert errors (`INSERT OR IGNORE`
    removed), so a collision can never again silently produce two Traefik
    routers for one host.
22. **Redaction (ST-A12).** `Redact` is now a case-insensitive regex over
    `password|passwd|secret|token|api_key|db_password` in both `key=value`
    and JSON forms, applied to *every* occurrence, and job events are
    redacted too.
23. **History retention (ST-A10).** Housekeeping prunes SSL checks, login
    attempts, job events and terminal jobs older than 90 days.
24. **Title injection (ST-B).** Titles with line breaks are rejected at
    input validation, and `GenerateEnv` strips CR/LF from every value as
    defence in depth.
25. **Compose policy hardening (ST-B1 / SEC-M3)** — see SECURITY-REPORT.

## E. Security (SEC)

26. TOTP throttling (SEC-H1), recovery throttling + 80-bit codes (SEC-L4),
    secrets out of URLs (SEC-M1), TOTP seeds encrypted at rest (SEC-L10),
    MariaDB password via `MYSQL_PWD` env instead of argv (SEC-M2), dump /
    import passwords via env (SEC-L5), pre-auth CSRF on setup/login/TOTP/
    recovery (SEC-L1), `Secure` cookies default on (SEC-L3), X-Forwarded-For
    last-hop attribution (SEC-L2), 512 MB upload cap (SEC-L6), backup-path
    boundary check (SEC-L7), 15 GiB extraction caps in both tar extractors
    (SEC-L8), `guardExisting` site-id ownership check implemented (SEC-L11).
    Details and the one deliberate deviation (SEC-L9) in SECURITY-REPORT.md.
