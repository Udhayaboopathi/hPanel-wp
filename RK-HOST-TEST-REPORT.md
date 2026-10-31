# RK Host Test Report

Date: 2026-10-31. Environment: local build VM (no Docker, no
production access — by the user's standing rule the server is only
reached through the CI/CD deploy).

## Automated tests

- `go build ./...`, `go vet ./...`: clean.
- `go test ./...`: **all 13 packages pass**, including the audit
  regression suite (scenarios A–H) and the extended template
  render tests: the new templates (performance, restore,
  site_settings, containers, logs, import) and the detail page's
  new sub-navigation are executed against fixture data in
  `cmd/panel/detail_template_test.go`.

## Live route smoke (real panel binary, temp data dir)

Setup → login performed over HTTP with the real pre-auth CSRF
flow; one website row seeded. Result: **34/34 GET routes returned
200**, including every canonical RK Host route and every legacy
alias:

`/`, `/dashboard`, `/websites`, `/websites/new` (+ `/details`,
`/admin`, `/review` aliases), `/deployments`, `/jobs`, `/backups`,
`/server`, `/monitoring`, `/server/containers`, `/logs`,
`/audit-logs`, `/audit`, `/settings`, `/settings/security`,
`/account`, `/websites/import`, and for the seeded site:
overview, `/wordpress`, `/domains`, `/performance`, `/files`,
`/environment`, `/compose`, `/database`, `/backups`, `/jobs`,
`/restore`, `/settings`, plus `/static/app.css` and
`/static/app.js`.

Verified behaviours:

- Legacy `?tab=files` → 302 to `/websites/{id}/files`;
  `?tab=danger` → 302 to `/websites/{id}/settings`.
- Anonymous `GET /dashboard` → 303 to `/login` (auth enforced on
  every new route via the same `requireAuth` wrapper).
- Unknown path renders the RK Host-branded "Not found" page.
- Dashboard HTML contains the sidebar shell and RK Host branding;
  the Performance page renders its honest "No history yet" empty
  state with no agent connected; the Containers page degrades to
  a clear agent-unavailable message (no fake rows).

## Live-monitoring follow-up (same day)

- Unit tests added: `parseAllStats` (multi-site OpStats parsing
  for the sampler) and `makeBars` (chart normalization/clamping) —
  passing.
- Sampler proven against the real binary: with zero page visits,
  a host sample existed in `resource_samples` ~9 seconds after
  startup (first tick), confirming the 1-minute loop runs.
- With seeded samples, the history graph renders on the Dashboard,
  Server and Performance pages; the Logs page shows its live
  badge/polling target and `GET /api/logs?site=…` returns 200
  with the honest fallback text when no agent is connected (on
  production it returns the container's real logs).

## Not tested here (requires the production deploy)

Real agent-backed data on the new pages (live stats, discovery
inventory, container logs) — the same agent operations already
power the existing overview/monitoring pages in production, and
the new pages reuse those exact calls. Post-deploy verification
steps are listed in RK-HOST-DEPLOYMENT-REPORT.md.
