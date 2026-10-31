# RK Host Deployment Report

Status: **built, tested and committed locally; deployment pending
the same GitHub push as the audit fixes** (the push needs a fresh
GitHub token from the owner — earlier tokens were chat-exposed and
never stored).

## What deploys

One push to `main` ships three stacked commits: the audit
remediation (`7a7f025`), the WordPress 7.1.4 pin (`7fb4283`) and
the RK Host UI (this release). The existing self-hosted workflow
rebuilds the panel image and force-recreates only the panel and
agent containers; `wordpress-mariadb` and all protected production
containers are untouched (the workflow's before/after container-ID
guard still applies). No database migration is introduced by the
UI release (the `resource_samples` table already exists; the new
code only reads it). No configuration or secret changes are
required.

## Rollback

Revert the RK Host commit (and, if desired, the two beneath it)
and push; the workflow redeploys the previous image. Templates and
CSS are embedded in the binary, so a rollback is a pure image
swap. One caution carried over from the audit release: TOTP seeds
encrypted by the new build cannot be read by the pre-audit build —
an operator rolling back across `7a7f025` should expect to
re-enrol TOTP (recovery codes keep working).

## Post-deploy verification (operator, ~5 minutes)

1. Open https://hpanel.krwebsolution.com — the RK Host sidebar
   shell, white theme, no theme toggle.
2. Log in; Dashboard shows real counts and host metrics.
3. Open the existing site (sudoinnovation.tech): sub-navigation
   renders; Overview, Performance (history starts accumulating),
   Files, Database, Backups all load; Restart works (audit fix).
4. Visit /server/containers: the 10 protected containers are
   badged "protected · read-only"; the site's container links to
   its page.
5. Visit /logs, pick the site: live container logs render.
6. Confirm the 10 protected containers kept their IDs (workflow
   guard output) and other production sites respond as before.
