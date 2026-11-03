# Full Line-by-Line Audit — KR Host (Own hPanel)

**Date:** 2026-11-03 · **Scope:** the complete codebase — 63 non-test Go source
files, 9,610 Go non-test lines, 14,731 lines total including tests, templates,
JS/CSS and infrastructure (workflows, compose, Dockerfile, deployment scripts).
**Method:** six independent read-only reviewers, one per subsystem (agent,
panel core, panel detail/misc, importer+jobs, store/data layer,
generation/frontend/infra), followed by personal re-verification of every
critical/high finding against the code before any fix was applied.
**Result:** no critical findings. All confirmed high findings and the great
majority of medium/low findings are fixed in this release, with regression
tests. Full suite green: `go build`, `go vet`, `go test ./...` (16 packages).

---

## HIGH — all fixed

1. **Keep-original-admin imports always failed.** The import path delegates
   to the create path, which refuses an empty admin password — so the
   "keep the backup's own WP admin" migration mode (the user's stated model)
   could never succeed. Import now delegates in repair mode.
2. **Change Domain never actually moved the site.** The apply-config path
   reused the running container, so routing, environment and limits stayed
   on the old domain. The site container is now explicitly stopped and
   removed before recreation, and the handler moves the domain registry in
   the same transaction as the site row, with a rollback if the apply job
   cannot be queued.
3. **Deleting a website leaked rows and burned its domain forever.**
   MariaDB ignores the column-level `REFERENCES` clauses the MySQL
   migration used (SQLite enforces them, which hid this in tests), and
   `DeleteWebsite` relied on cascades. Deletion now cleans every child
   table explicitly in one transaction; the MySQL migration was rewritten
   with table-level foreign keys and defaults matching SQLite (verified by
   a cross-dialect schema/defaults test covering all 16 tables).
4. **First-run setup was racy** (check-then-create allowed duplicate
   admins under concurrent requests) and silently fell back to storing
   the TOTP seed in plaintext if encryption failed. Setup is now atomic
   in-process and fails loudly instead of storing a plaintext seed.
5. **Job cancel-at-start race:** a job cancelled in the same moment it
   started was overwritten back to "running". Job start is now an atomic
   compare-and-swap (`ClaimJob`); a pre-start cancel is honoured, and new
   regression tests pin the behaviour.
6. **Request bodies had no total size limit.** Added a body-capping
   middleware (2 MB normal, ~3.25 GB for multipart imports) and idle
   timeouts on both HTTP servers.
7. **WordPress password reset always failed:** it used a WP-CLI option
   (`--new-pass`) that does not exist. Now uses `user update --user_pass`
   (regression-tested).
8. **Docker exec could report success on failure:** an inspection error
   was discarded and exit code defaulted to 0. Exit code now starts at −1
   and inspection errors propagate.
9. **A failed database import destroyed the site's database** (fresh
   tables were dropped before the dump was loaded). Imports now load
   into a scratch database and swap tables only on success; the original
   database survives any dump failure.
10. **Import size accounting was per-file, not cumulative:** each part
    of a multi-part set was individually allowed the full 2 GB. Parts
    now stream against a shared remaining budget, and the part merge
    preflights declared uncompressed sizes against the 15 GB ceiling
    (decompression-bomb guard, regression-tested with forged headers).
11. **TOTP codes were replayable** within their validity window. Every
    successful verification now atomically consumes the time step
    (`ConsumeTOTPStep`); account TOTP setup/recovery-code regeneration
    additionally require step-up authentication (current password, plus
    current TOTP code when 2FA is already enabled). The otpauth link in
    templates was unusable (the HTML sanitiser rewrites the scheme) and
    is now shown as copyable text.
12. **Restore accepted unfinished backups.** Restore now requires a
    `ready` backup with a path and checksum, and the agent's restore is
    a true file swap with rollback on failure (previously it copied over
    the live tree with no way back).
13. **Delete-site left the Docker network behind**, which later blocked
    recreation. The network is now disconnected and removed.

## MEDIUM — fixed

- **Login timing oracle:** unknown accounts skipped password
  verification; now verified against a dummy hash at equal cost.
- **Account recovery swallowed errors** while the audit log claimed
  success; failures now abort the flow.
- **Job API leaked internals:** the JSON response serialised the raw job
  row including payload data; it is now an explicit projection.
- **Collision checks scanned only the first 1,000 sites** (domains, DB
  names) in create/import/change-domain. Replaced with indexed lookups
  in both directions (domain vs admin alias).
- **Capacity checks measured the wrong filesystem** ("/") and silently
  passed when capacity was unknown. They now sample the websites volume
  and fail closed.
- **Site list "sort by domain" sorted only the visible page.** Sorting
  now happens in SQL before pagination.
- **`job` state machine allowed resurrection transitions**
  (failed/interrupted → queued) that the retry path itself refuses;
  removed.
- **Import staging files (full SQL dumps with password hashes) were
  left on disk** after terminal job outcomes; they are now scrubbed.
  A separately uploaded DB file silently lost to a backup set's own
  `-db` part; the separate file now wins, and the job log says so.
- **Site-overview page made two sequential agent calls** (~16 s worst
  case); they now run concurrently.
- **Applying config to an imported site dropped its table prefix**
  (`WORDPRESS_TABLE_PREFIX`), which would have broken it; the apply
  path now carries the prefix, the post-size setting and title env.
- **DB password rotation could strand a site** (database rotated, panel
  record not updated, error ignored). It now encrypts first and rolls
  the database back if the record update fails.
- **`multiStatements=true` removed** from the runtime MySQL DSN.
- **Audit-log redaction leaked the tail of quoted secrets** containing
  spaces; quoted forms are now redacted first.
- **Search text was interpolated into LIKE patterns** unescaped
  (`%`/`_` acted as wildcards); now escaped.
- **Compose policy validator had blind spots** (volumes-from, device
  cgroup rules, user, entrypoint/command overrides, runtime, sysctls,
  group-add, label files); all are now modelled and rejected.
- **`SafeJoin` allowed symlink escapes** (lexical check only). It now
  resolves the deepest existing ancestor and re-verifies containment —
  shared by the file manager and the agent (regression-tested).
- **Email validation accepted spaces/control characters** in the local
  part; SQL literal escaping now uses standard quote-doubling (safe
  under `NO_BACKSLASH_ESCAPES`).
- **Agent hardening:** refuses to start with a missing/short token;
  health-wait now enforces its deadline instead of proceeding past a
  never-ready container; helper container names are randomised (a
  leftover helper previously collided with the next operation);
  WP-CLI operations validate the full site spec, not just the ID;
  Docker log reads are status-checked and size-capped; stats parsing
  can no longer panic on partial Docker responses.
- **Frontend:** create-preview inserted server strings into `innerHTML`
  unescaped (now escaped); pages are served `Cache-Control: no-store`
  (setup-complete carries recovery codes); the DNS help block showed
  hardcoded `@`/`wpadmin` names instead of the site's real domains —
  the generator now returns the site's actual FQDNs.
- **Deploy workflow:** manual dispatch now requires `main`; deploy
  directory must be exactly `/opt/own-hpanel/app` (the sync uses
  `rsync --delete`); staged password temp files are removed via a trap
  on every outcome; the protected-container guard now fails if any
  protected container is missing *before* the deploy and compares ID,
  image and running state (previously a container missing on both sides
  passed); the public health check now **fails the deploy** after a
  bounded retry instead of warning; the previous panel/agent images are
  retained as `:rollback` and restored automatically when a
  post-deploy verification step fails; GitHub Actions are pinned to
  full commit SHAs (verified against the actions repositories).
- **CI now builds both production Docker images**, so Dockerfile
  failures surface in CI rather than at deploy time.
- **`.dockerignore` added** (the build context previously included the
  generated `.env` and staged secrets); `.gitignore` extended to
  `.env.*`, keys and credential files.
- **Installer:** a supplied `DB_ADMIN_PASSWORD` is now actually written
  to the 0600 secret file (before, it only suppressed a warning and
  the stack started without a usable credential); compose is started
  from the checkout's own compose file, not the caller's directory;
  the MariaDB grants script passes the root password via `MYSQL_PWD`,
  not argv.

## LOW — fixed (selection)

- Resource validation accepted NaN/infinity and ignored its own 0.1 CPU
  minimum (could emit `NaN` into a site compose file).
- Site IDs had 32 bits of entropy with ignored random failures; now 64
  bits with a safe fallback. `CreateInput.Validate` now actually
  validates PHP version, profile and custom resources.
- Argon2 verification ignored the PHC version field and accepted absurd
  parameters; both are now checked/clamped.
- Monitoring cache returned another filesystem's disk stats within the
  cache window; the cache is now path-aware.
- Dead code removed: an unused in-memory "secrets" map in the job
  runner, the unused 50 KB htmx bundle reference, a no-op htmx check.
- Secrets key file is written via temp-file + rename (a crash mid-write
  previously risked orphaning every encrypted secret).
- Import page text claimed a WP-CLI URL-rewrite tool the panel does not
  have; corrected to point at Change Domain.

## Reviewed and deliberately NOT changed

- **MariaDB published port (3024) and the provisioning account's broad
  grants** on the dedicated container: deliberate, documented design
  (the container serves only this panel; the grants script header and
  the README explain why). Recorded here as accepted risk.
- **Login throttle check-then-act:** a determined parallel burst inside
  one window can record slightly fewer attempts than made; the
  10-per-15-minute bound still applies. Rewriting it atomically was not
  judged worth the regression risk in this pass.
- **Deploy rollback covers panel/agent images only.** Database schema
  and site data are not rolled back automatically (migrations are
  additive and probe-first by design).
- **WordPress image referenced by tag, not digest** (`wordpress:7.1.4-…`)
  — pinning by digest remains a follow-up; the version itself is pinned.
- **WP-CLI helper containers receive the admin password via argv** —
  visible only to the host's Docker inspection during the helper's
  short life; a stdin/secret-file channel is a follow-up.
- Two reviewer claims were checked and **refuted**: resource samples
  *are* pruned (7-day retention in housekeeping), and no panel route
  mismatches were found between templates/JS and the registered routes.

## Verification

- `go build ./...`, `go vet ./...`, `gofmt` — clean.
- `go test ./...` — all 16 packages pass, including new regression
  tests: job claim CAS, TOTP step replay, delete-frees-domain,
  create rollback, SafeJoin symlink escape, resource NaN validation,
  WP-CLI reset argv, WPvivid declared-size bomb, cross-dialect schema
  columns **and** defaults for all 16 tables.
