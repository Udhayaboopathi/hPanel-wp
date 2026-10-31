# Security Report — audit remediation (31 Oct 2026)

Source: the read-only security audit (`audit-findings/security.md`,
1 HIGH · 3 MEDIUM · 11 LOW) plus the compose-policy items from the
store/config audit. Dispositions below; "Fixed" means code in this commit.

## Fixed

| ID | Severity | Finding | Fix |
|---|---|---|---|
| SEC-H1 | HIGH | TOTP second factor had no attempt throttling — unlimited code guesses inside each 10-minute pending window after password compromise | `totpSubmit` enforces the same `RecentFailedLogins` limit as the password step and drops the pending login when it trips |
| SEC-M1 | MEDIUM | Live secrets (TOTP seed, all recovery codes) placed in redirect URLs → browser history + Traefik access logs | One-time server-side display storage (`setOnce`/`popOnce`); URLs carry no secrets anymore |
| SEC-M2 | MEDIUM | MariaDB admin password (the all-sites provisioning credential) passed as `-p<password>` in exec argv, visible in process lists | Password travels as `MYSQL_PWD` in the exec environment (`ExecDBEnv`); argv contains no secret — asserted by unit test |
| SEC-M3 / ST-B1 | MEDIUM | Compose policy validator left field classes unchecked: `security_opt`, namespace sharing (`container:`/`service:`), top-level networks/volumes, named volumes, `build:`, forged `own-hpanel.site-id`, Traefik host hijack | Validator rewritten: any namespace-mode value rejected; `security_opt` allowlist = `no-new-privileges` only; top-level volumes forbidden; top-level networks pinned to the site's two generated entries with exact names; service networks pinned likewise; named/relative volume sources rejected; `build:` rejected; `own-hpanel.site-id` must equal the site being edited; Traefik `Host()` rules pinned to the site's own domains at all four call sites (17 rejection cases in tests) |
| SEC-L1 | LOW | No CSRF token on the four unauthenticated POST routes (login CSRF; cross-site setup in the first-run window) | Double-submit pre-auth CSRF (cookie + hidden field, constant-time compare) on setup, login, TOTP and recovery forms, GET and POST sides |
| SEC-L2 | LOW | `clientIP` trusted the first, spoofable X-Forwarded-For hop | Uses the last hop — the value Traefik itself appends (username-side rate limiting remains the primary control) |
| SEC-L3 | LOW | Session cookie `Secure` flag defaulted to false in code | `PANEL_SECURE_COOKIES` now defaults to true; plain-HTTP operation is an explicit opt-out |
| SEC-L4 | LOW | Recovery codes ~40-bit entropy, no throttling on the recovery endpoint | Codes are now 10 random bytes (80 bits, shown in 4 groups); `recoverSubmit` enforces the login rate limit |
| SEC-L5 | LOW | SQL literal escaper reused to quote DB passwords inside `sh -c` dump/import strings | Passwords removed from the shell strings; the one-shot helper receives `MYSQL_PWD` via its environment |
| SEC-L6 | LOW | File upload had no total size limit | Hard 512 MB per-file cap (`io.LimitReader` + overflow deletes the partial file) |
| SEC-L7 | LOW | Restore backup-path check was a raw prefix match (sibling-path bypass) | Boundary-aware check on the cleaned backups dir (same pattern the panel already used) |
| SEC-L8 | LOW | Tar extraction had no cumulative size cap (decompression bomb) | 15 GiB cumulative cap in both extractors (agent restore + `backups.ExtractTarGz`) |
| SEC-L10 | LOW | TOTP seed stored in plaintext in the panel DB | Seeds stored encrypted with the existing AES-256-GCM secrets box at setup and at account TOTP setup; reads transparently accept legacy plaintext rows |
| SEC-L11 | LOW | `guardExisting` site-ownership check was an empty stub | Implemented: a name collision with a container whose `own-hpanel.site-id` names a different site (or no site) is refused |

## Deliberate deviation (documented, not silently skipped)

- **SEC-L9** — WordPress admin password in the one-shot WP-CLI helper's
  argv (`--admin_password=`). The suggested env-var fix requires wrapping
  the helper in `sh -c` with shell-escaped interpolation of user-influenced
  values (site title), which would trade a seconds-long, docker-privileged
  visibility window for a command-injection risk. The helper container is
  created, used and removed within seconds, and both `docker inspect` and
  the process list require host/docker privileges. Kept as-is; recorded in
  REMAINING-ISSUES.md for a future redesign of the helper interface.

## Verified OK by the audit (no change)

Password hashing (Argon2id, OWASP interactive parameters), 256-bit session
tokens stored hashed with absolute expiry and full revocation on password
change/recovery, session CSRF coverage on all 33 authenticated POST routes,
agent bearer-token auth + allowlisted ops + production guard, per-site
database users with least-privilege grants, AES-256-GCM encryption of site
DB passwords, traversal-safe file manager and backup extraction
(per-entry), and the panel container holding no Docker socket (agent only).

## Production safety

No fix in this set changes the deployment's blast radius: the workflow is
unchanged, the protected-container guard still snapshots all running
containers before/after and fails the deploy on any change, and the
application changes are confined to the panel and agent containers.
