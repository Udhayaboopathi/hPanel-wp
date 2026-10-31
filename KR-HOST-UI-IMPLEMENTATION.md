# KR Host UI Implementation

## What was built

- **Rebrand:** all visible "Own hPanel"/"NexHost" copy is now
  **KR Host** (page titles, login, sidebar, footer, browser title).
  Internal identifiers are untouched: Docker labels
  `own-hpanel.*`, container names, database names, env vars, the
  agent protocol and the repository/module path.
- **Shell:** the top-bar layout was replaced by the Stitch shell —
  252 px sidebar with grouped navigation, Create Website button,
  admin footer; slim topbar; 1440 px content ceiling; off-canvas
  sidebar on mobile. One shared `sitenav` define renders the
  website header + sub-navigation on every website page.
- **Design system:** `web/static/app.css` rewritten around the
  brief's tokens (see KR-HOST-DESIGN-SYSTEM.md). The dark theme and
  the theme toggle were removed from CSS, JS and markup. Inter is
  loaded from Google Fonts with a system-font fallback (the panel
  remains fully usable offline).
- **Routes:** the brief's route scheme is canonical —
  `/dashboard`, `/websites/:id/{wordpress,domains,performance,
  files,environment,compose,database,backups,jobs,restore,
  settings}`, `/deployments(/:id)`, `/logs`, `/server`,
  `/server/containers`, `/settings/security`, `/audit-logs`,
  `/websites/import`, `/websites/new/{details,admin,review,
  progress}`. Every legacy path (`/`, `/jobs`, `/monitoring`,
  `/audit`, `/account`, `?tab=` URLs) still works: same handlers or
  a redirect to the canonical path, so no bookmark breaks.
- **New pages:** Performance (live stats + recorded history),
  Restore wizard, Site Settings & Lifecycle, Docker container
  inventory, global Logs viewer, Migration guide. All other pages
  are the existing, working pages restyled by the shared system.
- **Wizard:** the create wizard gained the Stitch 3-step stepper
  (Details & WP admin → Preview & DNS → Create); fields, server
  preview and validation are unchanged. Provisioning progress is
  the real deployment job page (Stitch screen 8).

## Stack discipline (brief §6/§11)

Go templates + vanilla CSS + minimal vanilla JS, exactly as before.
No React/Tailwind runtime, no Node services, no new dependencies;
the Stitch HTML was used as design reference only. The stylesheet
is a single 14 KB file; JS shrank (theme code deleted).
