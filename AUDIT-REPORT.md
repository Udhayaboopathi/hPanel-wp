# Audit Report — Own hPanel end-to-end audit (31 Oct 2026)

## Trigger and scope

Commissioned after the failed `restart_site` jobs on site wp-1c46776b.
Scope per the brief: the complete request path (browser → router → handler
→ job → runner → agent → Docker), container identity and reconciliation,
the job state machine, every route/template/handler wiring, security
(auth, sessions, CSRF, injection, path traversal, secret handling, agent
operation guard), the metadata store, configuration generation, backups,
and the deployment pipeline. Production inspection was read-only evidence
supplied by the operator (a full `docker ps`); per the project's standing
rule the assistant has no direct server access.

## Method

1. Incident-first code investigation of the restart path (agent lifecycle,
   provisioning runner, job store).
2. Three parallel read-only audits by independent reviewers, each
   re-verified against the final tree state:
   - Frontend/backend wiring — `audit-findings/frontend.md`
   - Security — `audit-findings/security.md`
   - Store/jobs, config generation, backups — `audit-findings/store-config.md`
     (delivery of this file was truncated mid-B1 in the audit tooling; its
     remaining findings were taken from the auditor's completion summary —
     title .env injection, restore checksum gap, S3 dead code, shared
     Traefik network note — and each was re-verified in code before fixing.)
3. Every fix implemented in code, then the full test suite plus new
   regression tests (TEST-REPORT.md).

## Findings totals

| Audit | HIGH | MEDIUM | LOW | Total |
|---|---|---|---|---|
| Incident investigation (identity/jobs) | — | — | — | 5 defects (D1–D5, ROOT-CAUSE-ANALYSIS.md) |
| Frontend wiring | 1 | 6 | 6 | 13 |
| Security | 1 | 3 | 11 | 15 |
| Store/config/backups | 0 | 10 | 13 | 23 |
| **Total** | **2** | **19** | **30** | **51 + 5 incident defects** |

## Outcome summary

- **Fixed in this commit:** both HIGH findings (TOTP throttling; job
  cancellation), all MEDIUM findings except the settings-consumers item
  (FE-M6, partially fixed — see REMAINING-ISSUES), and the large majority
  of LOW findings. Full enumeration: BUG-FIX-REPORT.md, SECURITY-REPORT.md.
- **Documented as remaining:** versioned migrations, full transaction
  wrapping in `CreateWebsite` (errors are now propagated instead), config
  version allocation race, the `deployments` derived-row dual write,
  state-machine enforcement inside `UpdateJob` (transitions were aligned),
  unwired Settings consumers (`alert_email`, S3 offsite upload, retention
  enforcement), the WP-CLI argv deviation (SEC-L9 rationale in
  SECURITY-REPORT.md), and operator-side items (Cloudflare DNS-only for
  `db.`, token revocations, legacy systemd unit). See REMAINING-ISSUES.md.
- **Verified healthy by the audits** (no action): route↔control mapping is
  complete (62 routes), dashboard/WP-CLI/detail data wiring is real,
  password hashing and session design are sound, authenticated CSRF
  coverage is complete, per-site database isolation and per-site networks
  are real, backup creation verifies SHA-256, downloads are guarded.

## Production state confirmed during the audit

From the operator's `docker ps`: panel, agent and the dedicated MariaDB
are healthy; all 10 protected production containers are up with unchanged
IDs; no WordPress site container exists for wp-1c46776b (it was never
created — see ROOT-CAUSE-ANALYSIS.md). No production data was harmed by
the incident or by this audit.
