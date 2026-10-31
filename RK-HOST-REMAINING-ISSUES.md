# RK Host Remaining Issues

Carried from the audit's REMAINING-ISSUES.md (still applicable:
versioned migrations, config-version race, restore-chain restart
behaviour, unwired S3/alert/retention settings, WP-CLI argv note)
plus the RK Host UI release:

1. **Migration is guided, not one-click.** `/websites/import`
   composes create + file manager + database import. A true
   one-click importer (upload archive → provision → restore,
   including cross-server backups, which are currently tied to
   their origin site) needs a cross-site restore path in the
   agent — future work.
2. ~~**Performance history granularity.**~~ **Resolved in the
   live-monitoring follow-up:** a background sampler records host
   and per-site samples every minute, and the dashboard and server
   pages gained the same history graph as the performance page.
   The logs viewer is live (4-second polling) and running job
   pages auto-refresh. Samples are pruned at 7 days.
3. **Wizard step URLs.** The brief's step paths
   (`/websites/new/details|admin|review`) render the single-page
   wizard (the step is client-side state); `/progress` redirects
   to Deployments. Deep-linking a specific step would need
   server-side wizard state.
4. **Server page naming.** `/server` reuses the monitoring page
   (host metrics + agent stats output). A richer server screen
   (per-container table using the new discovery parser, host
   sample chart using the new store reader) is a natural next
   iteration; both data paths now exist.
5. **Design fidelity deltas.** Stitch mockups use Tailwind +
   Material Symbols and demo-only content; the implementation
   reproduces layout, hierarchy and tokens in the app's vanilla
   CSS with local SVG icons. Pixel-identical reproduction was
   traded for the brief's own constraints (§6/§11: keep the
   lightweight stack, no heavy component libraries).
6. **Deployment pending.** As with the audit release, everything
   is verified locally; production verification happens after the
   owner provides a fresh GitHub token and the push deploys.
