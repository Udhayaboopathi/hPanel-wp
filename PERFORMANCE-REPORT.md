# Performance Report — audit remediation (31 Oct 2026)

## What was measured

Local build VM (no Docker daemon, no server access — production figures
come from the operator's environment):

| Metric | Value | Method |
|---|---|---|
| Panel binary size | 19.5 MB | `go build ./cmd/panel` |
| Agent binary size | 10.0 MB | `go build ./cmd/agent` |
| Panel idle RSS (fresh start, this VM) | ~2 MB (VmRSS 2048 kB at startup) | `/proc/<pid>/status` after start; grows modestly with Go runtime warm-up and template/session caches |
| Panel login response | HTTP 303 → /setup (fresh data dir) | `curl` — correct first-run behaviour |

Production reference points (unchanged by this work): the panel container
reports `(healthy)` in the operator's `docker ps`; per-site container
limits remain 0.5 CPU / 512 MB for the lightweight profile.

## Performance impact of the fixes

- **Container resolution** now performs one label-filtered
  `GET /containers/json` per lifecycle operation (the daemon does the
  filtering) instead of one name-filtered call — same order of cost, no
  polling added anywhere.
- **Website detail page** makes at most two extra bounded agent calls
  (stats + DB size, 8 s timeouts, only when the agent is up) and caches
  the DB size in the store.
- **Job auto-retry** uses `attempts² × 10s` backoff (10 s, 40 s) with the
  global 2-slot semaphore still bounding concurrent jobs; a new per-site
  conflict check *reduces* wasted parallel work against one site.
- **Housekeeping** added one pruning pass (90-day history) to the existing
  10-minute ticker; each delete is a single indexed-by-time statement.
- **Removed** the unused htmx script from every page load (~50 KB
  transferred per page view before).
- The one-time-secret store and pre-auth CSRF are O(1) map/cookie
  operations on auth pages only.

## Known performance characteristics (unchanged, by design)

- Job events are delivered in batch when an agent call returns, not
  streamed; long operations (backup, create) show progress at step
  boundaries.
- The monitoring host sampler and per-page agent calls are read-only and
  best-effort; an agent outage degrades the website page to stored data
  rather than blocking it.
