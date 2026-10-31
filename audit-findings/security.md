# Own hPanel — Security Audit Findings (read-only)

Scope: `cmd/panel`, `cmd/agent`, `internal/store`, `internal/docker`, `internal/security`,
`internal/database`, `internal/backups`, `internal/provisioning`, `internal/wordpress`,
`internal/secrets`, `internal/domains`. Method: manual code review + targeted greps.
No source files were modified. Counts: **HIGH 1 · MEDIUM 3 · LOW 11 · total 15 findings.**

---

## Findings

### HIGH

**H1 | cmd/panel/handlers_auth.go:132 | TOTP second factor has no attempt throttling — online brute force after password compromise**
Evidence: `loginSubmit` gates on the failure counter before checking the password
(handlers_auth.go:98, `RecentFailedLogins(...) >= maxFailedLogins`), but `totpSubmit`
never calls `RecentFailedLogins` — it only *records* failures. The pending-login token
lives 10 minutes (cmd/panel/main.go:439-447) and is not rotated or invalidated on failed
attempts, so an attacker who knows the admin password gets unlimited guesses at a
10^6 code space (3 codes valid at any moment with the library's default ±1 window)
inside each pending window, and can mint a fresh window by logging in again.
Suggested fix: enforce the same `RecentFailedLogins` check (and a small per-pending attempt cap, e.g. 5) in `totpSubmit` before validating the code.

### MEDIUM

**M1 | cmd/panel/handlers_misc.go:240,291 | Live secrets placed in redirect URLs (TOTP seed, all recovery codes)**
Evidence: `accountTOTPSetup` redirects to `/account?totp_secret=<seed>&totp_url=<otpauth-url>`
and `accountRecovery` redirects to `/account?recovery=<code1,code2,…>` with all 10
*plaintext* recovery codes; `accountPage` reads them back from the query string. URLs are
recorded in browser history and in reverse-proxy (Traefik) access logs, so valid 2FA seeds
and unused recovery credentials leak to anyone with log/history access.
Suggested fix: carry one-time secrets via server-side flash storage (or render them directly in the POST response) instead of URL query parameters.

**M2 | cmd/agent/main.go:305 | MariaDB admin password passed as `-p<password>` in exec argv**
Evidence: `mariadbArgv` builds `mariadb -u<user> -p<adminPassword> -e <sql>` for every
provisioning/DB operation. The argv is visible in the DB container's process list and in
the Docker exec-instance configuration while the call runs. This password is the common
provisioning credential for *all* sites — the highest-value secret in the system.
Suggested fix: pass the password via the exec's environment (`MYSQL_PWD`) or a defaults-extra-file inside the exec instead of argv.

**M3 | internal/security/yamlpolicy.go:36-46,82 | Compose-policy validator leaves several dangerous fields unchecked**
Evidence (composite): (a) `security_opt` is parsed (line 46) but never validated —
`seccomp=unconfined`, `apparmor=unconfined`, `label=disable` pass; (b) namespace checks
reject only the exact string `"host"` (line 82) — `network_mode`/`pid`/`ipc` set to
`container:<name>` or `service:<name>` pass, which would share a namespace with another
container; (c) the top-level `networks:`/`volumes:` maps are parsed but never validated,
so a volume entry using `name:`/`external: true` could reference another stack's named
volume and an external network entry could join the internal DB network, bypassing
per-site isolation; (d) the `build:` section is not modeled or rejected; (e) the
validator requires `own-hpanel.managed=true` but does not pin `own-hpanel.site-id` to the
site being edited, although agent container resolution trusts that label.
Mitigating fact: today the agent never executes this YAML — `applyConfig`
(cmd/agent/main.go:1138) re-validates it and then regenerates the container from the
validated spec — so these are defense-in-depth gaps in the file that is stored as the
site's `compose.yaml`, not a currently exploitable path; they become live if compose is
ever run on that file (e.g. manual recovery).
Suggested fix: reject non-empty `security_opt` except `no-new-privileges`, reject any `network_mode`/`pid`/`ipc` value other than `""`/`bridge`-equivalents, validate top-level networks/volumes (no `external`, no `name`), reject `build:`, and force `own-hpanel.site-id` to equal the edited site.

### LOW

**L1 | cmd/panel/main.go:158-164 | No CSRF token on the four unauthenticated POST routes**
Evidence: `POST /setup`, `POST /login`, `POST /login/totp`, `POST /login/recover` are
registered without `requireAuth` (whose central check is at main.go:395-408), and their
templates contain no `csrf_token` field (grep count 0 in login/setup/totp/recover.html).
Impact is login-CSRF and, during the one-shot first-run window, a cross-site setup
submission from the operator's browser.
Suggested fix: add a pre-session CSRF cookie+field pair for the unauthenticated forms, at minimum for `/setup`.

**L2 | cmd/panel/main.go:342-346 | `clientIP` trusts the first `X-Forwarded-For` value unconditionally**
Evidence: the first XFF hop is used for login rate-limit attribution and audit IPs; a
client that can reach the panel without Traefik stripping/appending XFF controls it.
Impact is bounded because the failure counter matches username **OR** ip
(internal/store/store.go:279-284), so same-account brute force still trips the username
side; residual risk is audit-log pollution and lockout games.
Suggested fix: only honor XFF when the immediate peer is the known proxy, or use the last Traefik-appended hop.

**L3 | cmd/panel/main.go:106 | Session-cookie `Secure` flag defaults to false in code**
Evidence: `SecureCookies: envBool("PANEL_SECURE_COOKIES", false)`; production compose
sets it (`compose.yaml:115` `PANEL_SECURE_COOKIES: "true"`), but any non-compose run
serves session and pending cookies without `Secure`, exposing them on any plain-HTTP path.
Suggested fix: default `PANEL_SECURE_COOKIES` to true and require an explicit opt-out for local development.

**L4 | internal/auth/auth.go:123-137 + cmd/panel/handlers_auth.go:162 | Recovery codes: 40-bit entropy, no attempt throttling**
Evidence: codes are 5 random bytes (line 127) rendered as 10 hex characters (~40 bits),
and `recoverSubmit` consumes a code without first checking `RecentFailedLogins`.
Storage is safe (SHA-256 hashes, single-use via atomic `UPDATE … WHERE used_at IS NULL`,
internal/store/store.go:224-231), which keeps online guessing slow, but 40 bits is below
modern recovery-code norms.
Suggested fix: raise codes to ≥10 random bytes and apply the login rate-limit check in `recoverSubmit`.

**L5 | cmd/agent/main.go:915,987 | SQL literal escaper reused to quote a password inside `sh -c` strings**
Evidence: backup dump/import build shell strings with `-p'%s'` filled by
`database.EscapeLiteral(...)`, an escaper written for MySQL string literals
(backslash-escaping) — that does not safely quote inside POSIX shell single quotes.
It is safe today only because panel-generated DB passwords use an alphabet with no `'`
or `\` (auth.go password alphabet; cmd/panel/handlers_sites.go `genDBPassword`).
Suggested fix: pass the dump/import password via the helper container's environment (`MYSQL_PWD`) and drop password interpolation from shell strings entirely.

**L6 | cmd/panel/handlers_detail.go:494-527 | File upload has no total size limit**
Evidence: `fileUpload` caps only the in-memory multipart envelope
(`ParseMultipartForm(32 << 20)`, line 500) and then streams `io.Copy(out, file)`
(line ~525) with no byte cap — an authenticated admin (or stolen session) can fill the
disk with one request.
Suggested fix: wrap the copy in `io.LimitReader` with an explicit per-file cap (e.g. 512 MB) and reject larger uploads.

**L7 | cmd/agent/main.go:959 | Restore backup-path check is a raw prefix match without a path boundary**
Evidence: `strings.HasPrefix(req.BackupPath, a.backupsDir)` accepts sibling paths such
as `<backupsDir>-evil/…`. Reachability is limited (the panel separately verifies the
backup record belongs to the site, cmd/panel/handlers_sites.go `siteRestore`), so this
is defense-in-depth at the agent boundary.
Suggested fix: compare against `filepath.Clean(a.backupsDir)+string(os.PathSeparator)` (the panel's `backupDownload` already does exactly this).

**L8 | cmd/agent/main.go:1001 + internal/backups/backups.go (ExtractTarGz) | Archive extraction has no total-size cap**
Evidence: both extractors enforce per-entry containment and skip links (good), but copy
each entry up to its declared `hdr.Size` with no cumulative limit — a crafted artifact
placed in the backups directory can fill the disk on restore (decompression bomb).
Suggested fix: enforce a cumulative extracted-bytes ceiling in both extraction loops.

**L9 | internal/wordpress/wordpress.go:63-77 | WordPress admin password passed in container argv**
Evidence: `InstallArgs` and `ResetPasswordArgs` embed `--admin_password=` /
`--new-pass=` in the one-shot WP-CLI container's `Cmd`, visible via `docker inspect`
and the container process list until the helper is removed after the run.
Suggested fix: pass the password to the helper via its environment instead of argv.

**L10 | internal/store/store.go:172,190 | TOTP seed stored in plaintext**
Evidence: `administrators.totp_secret` is inserted/selected raw, while per-site DB
passwords get AES-256-GCM encryption (internal/secrets). A panel-database disclosure
therefore hands out the second factor for every admin.
Suggested fix: encrypt `totp_secret` with the existing secrets.Box (with the same legacy-plaintext read fallback Decrypt already supports).

**L11 | cmd/agent/main.go:263-282 | `guardExisting` site-ownership check is an empty stub**
Evidence: the function refuses non-managed name collisions, but the site-id check is
literally an empty block with the comment "Caller checks the site id match where
relevant." Impact is contained today — a cross-site name collision fails safely at the
daemon's name-conflict on create, and lifecycle ops never resolve by name.
Suggested fix: make `guardExisting` take the requesting site ID and refuse when the existing container's `own-hpanel.site-id` label differs.

---

## Verified OK (checked, no finding)

- **Password hashing** — Argon2id with OWASP interactive parameters (t=3, m=64 MiB,
  p=2, 32-byte key, 16-byte random salt; internal/auth/auth.go:17-24), constant-time
  comparison, 10-char minimum enforced in `HashPassword`.
- **Sessions** — 256-bit random tokens, only SHA-256 hashes stored, absolute 12 h
  expiry enforced in the session lookup SQL (internal/store/store.go `GetSession`),
  fresh token per login (no fixation), cookie `HttpOnly` + `SameSite=Lax` (+`Secure`
  in the production compose), and password change / recovery-code use revoke **all**
  sessions (handlers_auth.go `recoverSubmit`, handlers_misc.go `accountPassword`).
- **CSRF (authenticated surface)** — every one of the 33 authenticated POST routes in
  cmd/panel/main.go:165-212 is wrapped in `requireAuth`, which centrally validates
  `csrf_token` / `X-CSRF-Token` against the per-session token with
  `subtle.ConstantTimeCompare` (main.go:395-408); forms emit the token via the shared
  `csrf` template.
- **Login throttling (password step)** — 10 failures per 15 min keyed on username OR
  IP, checked *before* password verification (handlers_auth.go:93-103;
  internal/store/store.go:279-284).
- **Recovery codes (storage)** — SHA-256 hashed at rest, single-use enforced by one
  atomic conditional UPDATE (internal/store/store.go:224-231); use disables TOTP and
  revokes all sessions.
- **SQL injection** — the panel store is parameterized throughout; the one dynamic
  UPDATE builds its SET clause from a hard-coded column allowlist
  (internal/store/store.go:428). All agent-side DDL is built only in
  internal/database from identifiers that must match `^[a-z][a-z0-9_]{2,63}$` /
  `{2,31}$` regexes (internal/security/validate.go) and are backtick-quoted; passwords
  enter SQL only as escaped literals. Repo-wide grep for string-built SQL found no
  other interpolation paths.
- **Path traversal (file manager)** — view/edit/download/upload/mkdir/rename/delete
  all resolve through `security.SafeJoin` (internal/security/files.go:11-26: rejects
  absolute paths and `..` escapes); sensitive files (`.env`, `wp-config.php`,
  compose files, `*.key`, `*.pem`) are blocked from viewing, download, edit and
  upload; saves are capped at 1 MB and restricted to an editable-extension list.
- **Tar safety** — both extractors (agent `extractAll`, backups `ExtractTarGz`)
  verify each entry stays under the cleaned destination, skip symlink/hardlink/
  device entries entirely, and mask modes (fixed 0640 / `&0o755`); backup *creation*
  skips symlinks, so archives cannot smuggle host files out of a site directory.
  `copyTree` on restore likewise skips symlinks.
- **Agent guard / container identity** — start/stop/restart/delete/logs resolve
  containers **only** by labels (`FindBySiteID`: `own-hpanel.managed=true` AND
  matching `own-hpanel.site-id`, ephemeral helpers excluded, duplicates →
  `AmbiguousError`, name-only lookalikes refused, never adopted —
  internal/docker/docker.go:185-212; cmd/agent `resolveSiteContainer`). At the
  client layer, `Start`/`Stop`/`Restart`/`Remove`/`Exec` all re-run `Guard`
  (managed label) before mutating. `ExecDB` additionally requires argv[0] ==
  `mariadb` and a container labelled `component=mariadb` with no site-id, and the
  agent refuses all lifecycle ops on the DB container by name
  (`refuseDBContainer`). The two unguarded primitives (`RemoveAny`,
  `StartUnchecked`) are only used on container IDs returned by `CreateContainer`
  in the same operation (agent-created one-shot helpers), never on
  request-supplied IDs.
- **YAML validator (what it does check)** — exact-match image allowlist (no tag or
  digest substitution), `privileged` rejected, devices rejected outright, *any*
  published ports rejected, `cap_add` limited to a six-capability allowlist
  (no SYS_ADMIN/NET_ADMIN), bind-mount sources confined to the site directory,
  string tripwires for `docker.sock`, `own-hpanel.managed=true` label mandatory —
  and the agent re-validates independently on apply (cmd/agent/main.go:1138).
- **WP-CLI execution** — fixed action allowlist, argv arrays only (never a shell
  string), plugin/theme slugs regex-validated (internal/wordpress/wordpress.go).
- **Secret handling in jobs/logs** — persisted job payloads have `DBPassword` and
  `AdminPassword` stripped before marshaling (internal/provisioning/
  provisioning.go:54-57); the WP admin password exists only in panel memory and is
  deleted after the run; other job kinds persist `"{}"` payloads. Audit-log
  details pass through `store.Redact` (password=/secret= markers,
  internal/store/store.go:777-799). S3 credentials are never accepted into or
  stored in the panel DB (handlers_misc.go `settingsSave`). Per-site DB passwords
  are AES-256-GCM encrypted at rest with a 0600 key in a 0700 directory
  (internal/secrets/secrets.go).
- **Agent channel** — unix socket plus a 256-bit bearer token compared in constant
  time (cmd/agent/main.go:133-141), a closed op allowlist, request bodies capped at
  2 MB, and an 11-minute per-op timeout. The panel process never opens the Docker
  socket (only the agent does).
- **TLS/status checks** — `domains.CheckTLS` verifies certificates properly
  (ServerName set, TLS ≥ 1.2, no `InsecureSkipVerify`).
- **Panel hardening basics** — security headers on all responses (CSP
  `default-src 'self'`, `X-Frame-Options: DENY`, `nosniff`), `ReadHeaderTimeout`
  set, and account mutations (password change, TOTP disable) require re-entering
  the current password.
