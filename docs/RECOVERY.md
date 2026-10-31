# Recovery and rollback

## Roll back the panel itself (leaves every website running)

Websites are independent containers + the stack's dedicated wordpress-mariadb; removing the panel does **not** stop them (Traefik keeps routing by label).

```bash
cd hPanel-wp
docker compose stop panel          # panel UI offline; sites unaffected
docker compose stop agent          # provisioning paused; sites unaffected
```

Full removal of panel-owned resources only (explicit, no prune):

```bash
docker compose down                # removes the panel-owned containers of this
                                   # stack only (panel + agent +
                                   # wordpress-mariadb); NOT the named volumes,
                                   # NOT any other stack's containers, and
                                   # NEVER the pre-existing production containers
# (If you ever used the deprecated systemd unit instead of the compose
# agent: systemctl disable --now own-hpanel-agent && rm
# /etc/systemd/system/own-hpanel-agent.service.)
# Data remains under /opt/own-hpanel and in the named volumes until you
# deliberately delete them after taking a backup.
```

Never run `docker system prune`, `docker volume prune` or similar generic cleanup as part of rollback — they endanger unrelated production data.

## Panel database recovery

Panel metadata in production lives in the panel's own database (`hpanel_panel`) on the dedicated wordpress-mariadb container (`PANEL_STORE_DRIVER=mysql`); locally it is the panel container volume's `panel.db` (SQLite WAL). Recover the MySQL copy from your backups of that database (dump/restore with the admin account; container root credentials are in `/opt/own-hpanel/secrets/mariadb_root_password.txt`). The AES encryption key for stored per-site DB passwords lives in the panel volume (`/data`, file created by the panel with 0600) — back it up with the metadata. Without that key, stored per-site DB passwords cannot be decrypted (sites keep running; rotate each site's DB password afterwards).

## Website recovery

- **Site down**: website page → Start/Restart. Check the Jobs page and container logs (website → View logs).
- **Bad config change**: YAML tab → Configuration history → Rollback to the previous version → Apply Configuration.
- **Bad WordPress update**: Backups tab → Restore the pre-update backup (type `RESTORE`). Restores files and the database dump from the verified archive.
- **Failed provisioning job**: Jobs page shows the step and error. Fix the cause (e.g. pull a missing image, free disk) and re-run creation from the website page — creation is idempotent: the network, database, storage and container are reused rather than duplicated. Partial resources created by a failed job belong only to that site ID and can be removed with the site's Delete action (domain confirmation required).

## MariaDB recovery

All WordPress databases (and the panel's own `hpanel_panel` database) live in the stack's dedicated MariaDB container (`wordpress-mariadb`) — its data lives in the named volume `own-hpanel-wordpress-mariadb-data`, owned by Own hPanel. Back that volume up (or dump the databases with the common admin account / container root from `/opt/own-hpanel/secrets/mariadb_root_password.txt`) as part of the hPanel backup routine. Per-site recovery does not need volume-level access: import the site's `db.sql` dump from its panel backup (Backups → Restore handles this per site). The agent never stops/restarts/recreates `wordpress-mariadb` — its lifecycle is Compose's; if the container itself is down, bring the stack up (`docker compose up -d`) and check `docker logs wordpress-mariadb`. (The pre-existing production `construction-mariadb` holds none of this data and is never involved.)
