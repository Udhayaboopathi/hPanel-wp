# KR Host Backend Integration

Every screen is wired to the pre-existing backend (agent over the
Unix socket, store, provisioning runner). New backend code is
limited to three small, read-only additions; no operation changed
semantics.

## Additions

1. `store.ListResourceSamples(scope, siteID, limit)` — the read
   side of the existing `resource_samples` table (samples were
   written but never read). Powers the Performance history and is
   available to the Server page.
2. Per-site sampling: when the panel reads a site's live stats
   (Overview/Performance), it records a sample — the same pattern
   the dashboard already used for host samples.
3. `agentproto.ProtectedContainers` — the display copy of the
   deploy guard list, used only to badge containers on the
   inventory page. (Real protection remains the agent's label
   guard: it refuses every unmanaged container.)

## Screen → operation map (all real)

| UI | Backend |
|---|---|
| Login / Setup / TOTP / Recovery | existing auth (Argon2id, TOTP throttled, recovery codes) |
| Dashboard stats & alerts | store counts + live host sample + failed-job scan |
| Websites list/search/filter | `ListWebsites` |
| Create wizard + preview | `websites.CreateInput.Validate`, config generation, `create_site` job |
| Site overview | store + agent `OpStats` + `OpDBSize` + latest SSL check |
| Start/Stop/Restart/Backup | existing job-enqueueing actions |
| Repair Website | audit-added `OpRepairSite` (verify DB+files, recreate container only) |
| WordPress tab | WP-CLI allowlist via one-shot helper; admin password reset job |
| Domains & SSL | stored DNS instructions + live TLS check job (`OpCheckDomain`-backed handler) |
| Performance | agent `OpStats` + `resource_samples` history + store DB size |
| Files | panel-side traversal-safe file manager over the site root |
| Environment | allowlisted env editor → new config version |
| Compose | YAML policy validator + diff + apply-config job |
| Database | DB info, dump download, SQL import (agent one-shot) |
| Backups / Restore | backup jobs; restore = safety-backup-first chain + SHA-256 gate (audit) |
| Deployments / Job details | jobs store + events; Retry genuinely re-executes (audit), Cancel aborts the live call (audit) |
| Logs | agent `OpLogs` for the selected managed site; fallback to job events |
| Server | host sampler (real /proc data) + alerts |
| Containers | agent `OpDiscovery`; protected badged, managed linked, everything else read-only |
| Settings | existing system settings (create defaults) |
| Account & Security | password change, TOTP enrol/reset, recovery codes, sessions |
| Audit Logs | audit store with pagination |
| Migration | guided composition of create + file manager + DB import (no fake importer) |

## Deliberately not wired

Stitch demo elements with no backend — cluster sync, tenants, SSO/
WebAuthn, object-cache panels — are omitted rather than faked.
Settings that exist in the store but have no consumer yet (S3
backup target, alert email, retention) keep the audit's honest
copy; see KR-HOST-REMAINING-ISSUES.md.
