# Stitch → KR Host UI Mapping

How each Stitch screen maps onto the application. "Shell" means the
shared KR Host layout (252 px sidebar, topbar with breadcrumbs,
content ceiling 1440 px) defined once in `web/templates/layout.html`
and styled by the token system in `web/static/app.css`.

## Global mapping decisions

- **Branding:** every visible "NexHost"/"Own hPanel" becomes
  **KR Host**. Internal identifiers (Docker labels
  `own-hpanel.*`, container names, DB names, env vars) are unchanged.
- **Theme:** white/light only, brief tokens (primary `#2563EB`,
  background `#F8FAFC`, card `#FFFFFF`, text `#0F172A`, secondary
  `#64748B`, border `#E2E8F0`, success `#16A34A`, warning `#D97706`,
  error `#DC2626`, info `#0284C7`), Inter, 8 px radius. No dark mode,
  no theme toggle (the old toggle is removed).
- **Navigation (sidebar):** Dashboard `/dashboard` · Websites
  `/websites` · Deployments `/deployments` · Backups `/backups` ·
  Logs `/logs` · Server `/server` · Containers
  `/server/containers` · Audit Logs `/audit-logs` · Settings
  `/settings` · Security `/settings/security`, plus a primary
  **Create Website** button and the signed-in admin + Logout in the
  sidebar footer. Legacy paths (`/`, `/jobs`, `/monitoring`,
  `/audit`, `/account`) keep working.
- **Website context:** website subpages share one site header
  (domain, status badge, Open Website / Open WP Admin, Start/Stop/
  Restart) and a sub-navigation: Overview, WordPress, Domains & SSL,
  Performance, Files, Environment, Compose, Database, Backups,
  Restore, Settings. Legacy `?tab=` URLs redirect to these paths.

## Per-screen mapping

| Screen | Route(s) | Implementation |
|---|---|---|
| Login | `/login`, `/login/totp`, `/recover` | Existing auth handlers; Stitch split-card layout; only real auth methods (password, TOTP, recovery codes) |
| First-Time Admin Setup | `/setup`, `/setup/done` | Existing setup handler; same card layout |
| Dashboard | `/dashboard`, `/` | Existing dashboard data (site counts, host metrics, failed jobs, recent activity) in Stitch card/table layout |
| Websites | `/websites` | Existing list + search/filter; card-table hybrid per design |
| Create steps 1–3 | `/websites/new` | Existing single-page wizard with a 3-step stepper matching the three Stitch screens; same fields and validation |
| Provisioning Progress | `/deployments/{jobId}` | The real job page (progress bar, events, error + recovery actions); create redirects to the new site, whose header shows the live job banner |
| Site Overview | `/websites/{siteId}` | Existing simplified overview (open site/admin, monitoring card, controls) in the new shell |
| WordPress Manager | `/websites/{siteId}/wordpress` | Existing WP tab: core/plugin/theme info + allowlisted WP-CLI actions |
| Domains & SSL | `/websites/{siteId}/domains` | Existing domain tab: DNS instructions + live TLS check |
| Performance | `/websites/{siteId}/performance` | NEW: live container stats (agent), resource-sample history, DB size, profile limits |
| File Manager | `/websites/{siteId}/files` | Existing file tab (browse/edit/upload/download/delete, traversal-safe) |
| Environment | `/websites/{siteId}/environment` | Existing env tab (allowlisted keys, masked secrets, apply) |
| Compose | `/websites/{siteId}/compose` | Existing YAML tab (validate/diff/apply with policy checks) |
| Database | `/websites/{siteId}/database` | Existing DB tab (info, dump download, import) |
| Backup Manager | `/websites/{siteId}/backups`, `/backups` | Existing backups tab + global backups list |
| Restore Wizard | `/websites/{siteId}/restore` | Restore screen: pick a backup → panel first creates a fresh safety backup → restore runs as a job (existing backend chain) |
| Migrate WordPress | `/websites/import` | Guided migration page for the supported real path: create site → upload files (File Manager) → import SQL (Database) → adjust env; no fake one-click importer |
| Deployments | `/deployments`, `/jobs` | Existing jobs list |
| Job Details | `/deployments/{jobId}`, `/jobs/{jobId}` | Existing job page: events, error category, Retry/Cancel/Repair actions where supported |
| Logs Viewer | `/logs` | NEW: site picker → live container logs via the agent (managed sites only) |
| VPS Server Overview | `/server`, `/monitoring` | Existing monitoring page (host CPU/RAM/disk, samples) |
| Docker Inventory | `/server/containers` | NEW: agent discovery inventory; `protected` containers badged read-only, managed sites link to their pages |
| Site Settings & Lifecycle | `/websites/{siteId}/settings` | Site facts, resource profile, Repair Website, lifecycle controls, danger-zone delete (typed confirmation) |
| System Settings | `/settings` | Existing settings (create defaults; honest notes for unwired options) |
| Account & Security | `/settings/security`, `/account` | Existing account page (password change, TOTP, recovery codes, sessions) |
| Audit Logs | `/audit-logs`, `/audit` | Existing audit list with pagination |

## Not reproduced from the mockups (no backend capability)

Cluster/multi-node status and sync controls, SSO/WebAuthn sign-in,
tenant switchers, object-cache service panels, demo users and any
sample metrics. Where a Stitch screen showed such elements, the
layout was preserved with the panel's real equivalents (single-host
metrics, real sites/jobs, the real admin account).
