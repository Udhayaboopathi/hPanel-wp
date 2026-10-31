# Remaining Issues — known, documented, not fixed in this release

Nothing below is hidden or silently skipped: each item states its impact
and why it was not folded into this release. None of them affect the
wp-1c46776b repair path.

## Code / platform

1. **Versioned migrations (ST-A7).** The store still applies a single
   idempotent `001` migration script on boot (SQLite executes it whole;
   MySQL tolerates duplicate-object errors). Acceptable while the schema
   never evolves; a `schema_migrations` table and numbered transactional
   migrations are required before the first schema change.
2. **CreateWebsite transaction wrap (ST-A8, partial).** Insert errors are
   now propagated instead of ignored (the dangerous part — silent domain
   collisions — is fixed), but the three inserts are not yet one
   transaction, so a mid-failure can still leave a website row without
   its database record.
3. **Config version allocation race (ST-A9).** `SaveConfigVersion`
   computes `MAX(version)+1` outside a transaction; two concurrent saves
   for one site collide on the unique index and one fails with a raw DB
   error. Rare (single admin, saves are deliberate) and self-evident.
4. **Deployments derived-row dual write (ST-A11).** `AddEvent` writes the
   event and the derived `deployments` row in separate statements; the
   derived row snapshots job state at event time and can drift. The `jobs`
   table is the source of truth everywhere it matters.
5. **State machine not enforced inside `UpdateJob` (ST-A4, partial).** The
   transition map was aligned with every transition the code performs
   (including `running → queued` for retries and `queued → interrupted`
   for boot recovery), but `UpdateJob` remains a low-level writer; boot
   recovery and cancellation intentionally bypass per-call validation.
6. **Settings consumers (FE-M6, partial).** `default_profile` is now
   honoured by the create wizard. Still unwired: `alert_email` (no alert
   sender exists), S3 offsite backup upload (`backups.UploadS3` has no
   caller — backups are local-only, and the UI copy now says so honestly),
   and `backup_retention` enforcement (no automatic pruning of backup
   artifacts yet).
7. **Restore chaining across panel restarts (FE-M4, residual).** The
   restore's safety-backup → restore chain runs in a goroutine; a panel
   restart mid-chain leaves the safety backup in place and the restore
   unstarted (boot recovery marks stale records honestly, and the
   operator can start the restore again). Same residual pattern as the
   backup reconciler fallback.
8. **WP-CLI helper argv (SEC-L9).** The WordPress admin password appears
   in the one-shot WP-CLI helper's argv for its seconds-long lifetime.
   Documented deviation with rationale in SECURITY-REPORT.md.
9. **Sites share the Traefik network (ST-B note).** Every site container
   joins `redberyl-internal` (required for Traefik routing) in addition
   to its own per-site network; database isolation is per-site and real,
   but site-to-site traffic over the shared network is not firewalled.
   A future hardening step, not a defect in the identity model.

## Operator-side items (unchanged by this release)

10. **`db.krwebsolution.com` Cloudflare record is still Proxied** — set
    it to DNS-only if direct external MariaDB access on port 3024 is
    wanted (internal panel↔DB traffic does not use it).
11. **Legacy systemd agent unit** — run
    `sudo systemctl disable --now own-hpanel-agent` on the server so the
    deprecated unit can never compete with the Compose agent for the
    socket or delete `/run/own-hpanel`.
12. **Credential hygiene** — the GitHub classic PAT and the MariaDB
    password that passed through chat on 2026-10-31 should be
    revoked/rotated; neither is stored anywhere in this repository.
13. **Live end-to-end proof** — a successful WordPress creation on the
    real server is still outstanding; the post-deploy Repair Website run
    on wp-1c46776b (DEPLOYMENT-REPORT.md) is that proof.
