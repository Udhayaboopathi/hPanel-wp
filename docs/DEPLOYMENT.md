# Production deployment guide

Target: the existing Ubuntu 24.04 VPS (4 vCPU, 8 GB RAM) that already runs Traefik (`redberyl-traefik`, network `redberyl-internal`, entrypoints `web`/`websecure`, resolver `letsencrypt`) and several unrelated production applications. Own hPanel coexists with them; it does not modify them.

> Status note: this guide was written and tested locally. It has **not** been executed against the production server yet — deployment is a separate, later phase performed with the server owner's credentials and supervision.

## 0. Before you start

1. Take a full backup of the server (or at minimum of existing Docker volumes you care about).
2. Choose the panel hostname, e.g. `panel.example.com`, and create its DNS A record pointing at the server IP (DNS-only in Cloudflare initially).
3. Pull the approved images on the server: `docker pull wordpress:6.7.2-php8.3-apache wordpress:cli-2.11.0-php8.3 mariadb:11.4` (the mariadb image is used only for disposable one-shot client containers — backups/dumps and the CI panel-DB check; no MariaDB server container is deployed by this project. Note: `wordpress:cli-2.11.0-php8.3` — verify the exact current CLI tag when deploying and update `internal/security.ApprovedImages` + agent constants together if it differs). Also confirm the pre-existing shared MariaDB container (default `construction-mariadb`) is running and that the common admin account credentials are available as GitHub Secrets (CI) or a 0600 file — see docs/CICD.md.

## 1. Read-only discovery (changes nothing)

```bash
git clone https://github.com/Udhayaboopathi/hPanel-wp.git
cd hPanel-wp
chmod +x deployment/install.sh
sudo ./deployment/install.sh
```

Review the snapshot it writes under `/opt/own-hpanel/discovery-*.txt`: every pre-existing container, Traefik presence, the `redberyl-internal` network, ports 80/443 owned by Traefik, and free RAM/disk headroom. The script aborts if the network or Traefik is missing or free disk is under 5 GB.

## 2. Install (panel-owned resources only)

```bash
echo "panel.example.com" # your PANEL_DOMAIN
sudo PANEL_DOMAIN=panel.example.com ./deployment/install.sh --apply
```

This creates `/opt/own-hpanel/{websites,backups,secrets,logs}`, generates the agent token (0600, printed never), installs `own-hpanel-agent.service`, builds the agent binary to `/opt/own-hpanel/bin/agent` — **build step**: run `make build && sudo install -m 755 bin/agent /opt/own-hpanel/bin/agent && sudo install -m 755 bin/panel /opt/own-hpanel/bin/panel` before `--apply` — verifies the pre-existing shared MariaDB container is running, and starts the compose stack (panel container only). There is no panel-owned MariaDB: the shared DB admin password is supplied by you (see `.env.example` / docs/CICD.md), never generated or printed by the installer. For the normal path prefer the GitHub Actions deploy (docs/CICD.md); this script remains the manual fallback.

## 3. Verify — production protection test

```bash
docker ps -a --format '{{.Names}} {{.Status}}'   # compare with the BEFORE snapshot
curl -fsS https://panel.example.com/healthz      # -> ok
systemctl status own-hpanel-agent
```

Every pre-existing container must be present, running and unchanged. Then open the panel URL: first-run setup creates the administrator (save the recovery codes + TOTP secret shown once).

## 4. Create the first test website

1. Panel → **+ Create New WordPress Site** → domain (use a test subdomain first), admin details, Lightweight profile.
2. Watch the job on the Jobs page; it should end `ready` or `dns_pending`.
3. Add the two A records shown in the preview (DNS-only), wait for propagation, then on the site's Domain tab run **Check DNS & SSL**.
4. Open `https://<domain>` and `https://<domain>/wp-admin/`; log in with the credentials from the wizard.
5. Confirm `wpadmin.<domain>` redirects to the admin URL.

## 5. Ongoing operations

- Backups: per-site from the website page; artifacts land in `/opt/own-hpanel/backups` (configure external S3 in Settings for off-server copies — the on-server RustFS is not off-server storage).
- Updates: rebuild with `make build`, replace binaries / `docker compose up -d --build panel`, re-run the verification in §3.
- Migration of the 7 existing sites: use each site's **Backups → Restore** path with a backup made from the old host (files + SQL dump in the panel's archive format: `files/…` plus `db.sql` at the archive root), or stream files into the new site directory and import the dump — always test on a staging subdomain before DNS cutover, and never keep two full copies of ~25 GB on the disk at once: check `df -h` first.
