# Recovery and rollback

## Roll back the panel itself (leaves every website running)

Websites are independent containers + a shared MariaDB; removing the panel does **not** stop them (Traefik keeps routing by label).

```bash
cd hPanel-wp
docker compose stop panel          # panel UI offline; sites unaffected
systemctl stop own-hpanel-agent    # provisioning paused; sites unaffected
```

Full removal of panel-owned resources only (explicit, no prune):

```bash
docker compose down                # removes own-hpanel + wordpress-mariadb containers
                                   # (NOT the named volumes, NOT any other service)
systemctl disable --now own-hpanel-agent
rm /etc/systemd/system/own-hpanel-agent.service
# Data remains under /opt/own-hpanel and in the named volumes until you
# deliberately delete them after taking a backup.
```

Never run `docker system prune`, `docker volume prune` or similar generic cleanup as part of rollback — they endanger unrelated production data.

## Panel database recovery

Panel metadata lives in the panel container's `panel-data` volume (`panel.db`, SQLite WAL) plus `/opt/own-hpanel/panel/panel.key` in the documented layout. Restore by stopping the panel, replacing `panel.db` with a backup copy, and starting the panel again. Without `panel.key`, stored per-site DB passwords cannot be decrypted (sites keep running; rotate each site's DB password afterwards).

## Website recovery

- **Site down**: website page → Start/Restart. Check the Jobs page and container logs (website → View logs).
- **Bad config change**: YAML tab → Configuration history → Rollback to the previous version → Apply Configuration.
- **Bad WordPress update**: Backups tab → Restore the pre-update backup (type `RESTORE`). Restores files and the database dump from the verified archive.
- **Failed provisioning job**: Jobs page shows the step and error. Fix the cause (e.g. pull a missing image, free disk) and re-run creation from the website page — creation is idempotent: the network, database, storage and container are reused rather than duplicated. Partial resources created by a failed job belong only to that site ID and can be removed with the site's Delete action (domain confirmation required).

## MariaDB recovery

`wordpress-mariadb` data is in the `own-hpanel-wordpress-mariadb-data` volume. Restore from a volume backup or by importing per-site `db.sql` dumps from panel backups (Backups → Restore handles this per site). Do not touch `construction-mariadb` — it is a different, unrelated production database.
