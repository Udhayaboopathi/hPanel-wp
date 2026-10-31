# KR Host Design System

Implemented in `web/static/app.css` (single vanilla stylesheet, no
framework, no build step) and `web/templates/layout.html` (the one
shared shell). Sources: the KR Host brief's token table (canonical)
and the Stitch "NexHost Enterprise Control Panel" design system
(252 px sidebar, 1440 px content ceiling, Inter, 8 px radius).

## Tokens (CSS variables)

| Token | Value | Use |
|---|---|---|
| `--primary` | `#2563EB` | primary actions, active states, links |
| `--primary-hover` | `#1D4ED8` | primary hover |
| `--primary-tint` | `#EFF6FF` | active nav, avatar |
| `--bg` | `#F8FAFC` | app background |
| `--card` | `#FFFFFF` | cards, sidebar, topbar |
| `--fg` | `#0F172A` | primary text |
| `--muted` | `#64748B` | secondary text |
| `--line` | `#E2E8F0` | borders/dividers |
| `--success` | `#16A34A` | ready/running/succeeded |
| `--warning` | `#D97706` | queued/provisioning/attention |
| `--error` | `#DC2626` | failed/danger |
| `--info` | `#0284C7` | informational |
| `--radius` / `--radius-lg` | `8px` / `12px` | controls / cards |
| `--shadow` | subtle 2-layer | cards only |
| `--font` | Inter, system fallback | UI text |
| `--mono` | JetBrains Mono, ui-monospace fallback | code, logs, YAML |

White/light theme **only**: there is no dark palette, no
`data-theme` attribute and no theme toggle (removed from markup,
CSS and JS in this release).

## Shell

- Fixed left sidebar, 252 px: KR logo badge + wordmark, primary
  **Create Website** button, grouped navigation (Manage / System /
  Configure) with inline SVG icons and an active state (tint +
  primary text, set by a small pathname matcher in `app.js`),
  signed-in admin + Logout in the footer.
- Slim sticky topbar with the page title; content column capped at
  1440 px with 24 px padding.
- Mobile (≤900 px): sidebar becomes an off-canvas drawer with a
  scrim; hamburger in the topbar. Tables scroll horizontally;
  stat grids collapse to two columns (≤640 px).
- Auth pages use a centered brand + card shell (max 430 px).

## Components (shared classes, used by every page)

Buttons (`.btn`, `.primary`, `.danger`, `.big`, `.small`), cards
(`.card`), stat cards (`.stat` + `.meter`/`.progress`), tables
(`.table-wrap`, `.kv`), status badges (`.badge.status-*` with a
colored dot; ready/running/succeeded = success, failed = error,
queued/provisioning/interrupted/missing = warning), alerts
(`.flash`, `.error-box`, `.notice`), underline sub-navigation
(`.subnav`, also `.tabs`), site header (`.site-head` via the shared
`sitenav` template define), wizard stepper (`.steps`), empty state
(`.empty`), log viewer (`.log-view`), danger zone
(`.danger-zone`), forms with primary focus rings, and
`:focus-visible` outlines throughout. Motion is limited to short
hover/width transitions and disabled under
`prefers-reduced-motion`.

## Iconography

Inline SVG (stroke style, 18 px) drawn locally in the layout — no
icon font, no external asset, works offline. WordPress/site content
keeps the few emoji markers already used in file listings.
