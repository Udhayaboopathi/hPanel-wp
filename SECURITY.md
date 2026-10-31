# Security model

## Trust boundaries

- **Panel** (internet-facing via Traefik): unprivileged. No Docker socket, no host root access. Holds the SQLite metadata DB and an AES-256-GCM key (0600 file) used to encrypt per-site DB passwords at rest.
- **Agent** (host systemd service): the only Docker client. Docker access is effectively host-root, so the agent is treated as trusted code with a minimal surface: Unix socket only (never TCP), bearer token from a 0600 file with constant-time comparison, allowlisted operations, full re-validation of every request (the panel's validation is never trusted), and a production guard that refuses to mutate containers lacking `own-hpanel.managed=true`.
- **WordPress containers**: `cap_drop: ALL` + a minimal cap allowlist, `no-new-privileges`, no Docker socket, no published ports, private per-site network + the shared Traefik network only, CPU/RAM limits.
- **MariaDB**: dedicated `wordpress-mariadb` instance, no published ports, per-site database + user with least-privilege grants on that database only.

## Authentication

Argon2id password hashing (64 MiB, t=3, p=2), optional TOTP MFA (pquerna/otp), server-side sessions stored as SHA-256 token hashes with HttpOnly + SameSite=Lax cookies (Secure in production), CSRF tokens on every authenticated POST, login rate limiting (10 failures / 15 min per username/IP, recorded in `login_attempts`), single-use hashed recovery codes (use revokes all sessions and disables TOTP until re-enabled), logout-from-all-sessions, audit log with secret redaction.

## Input & execution safety

- Domains, database names/users and site IDs are whitelist-validated in both panel and agent before use; SQL identifiers are never interpolated unvalidated and grants are always scoped to one database.
- WP-CLI operations are an allowlist; commands are built as argv slices, never shell strings from user input. (Two internal exceptions run `sh -c` inside disposable, labelled helper containers for `mariadb-dump`/import, built only from validated identifiers and escaped generated passwords.)
- Compose/YAML editing is schema- and policy-validated twice (panel + agent): privileged mode, host network/PID/IPC, devices, published ports, Docker socket mounts, host bind mounts outside the site directory, unapproved images, dangerous capabilities and missing ownership labels are all rejected.
- The file manager is rooted per-site with traversal/symlink protections; sensitive files (`.env`, `wp-config.php`) are hidden from view/download/edit; uploads are streamed; deletes require an explicit confirmation value.
- Secrets never appear in Traefik labels, audit logs (redacted), deployment event messages, or the jobs payload stored in the DB. The WP admin password exists only in panel memory during the create job.
- Site deletion requires re-typing the exact domain; restore requires typing `RESTORE`.

## Hardening recommendations for production

- Put the panel behind Cloudflare Access (or equivalent) in addition to its own login.
- Keep `PANEL_SECURE_COOKIES=true`, serve only over HTTPS via Traefik.
- Rotate the agent token and MariaDB root password if they are ever exposed; both live in 0600 files under `/opt/own-hpanel/secrets/`.
- Back up `/opt/own-hpanel` (including `panel.key` — without it, stored DB passwords cannot be decrypted) to a genuinely off-server destination.
