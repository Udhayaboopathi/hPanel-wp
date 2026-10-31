# Stitch Screen Inventory — RK Host

Source: Google Stitch project **"NexHost WordPress Control Panel"**
(`projects/9139544689105612440`), retrieved 2026-10-31 through the
registered Stitch connector (read-only). Design system:
**"NexHost Enterprise Control Panel"** — LIGHT mode, Inter, 8 px
roundness, custom primary `#2563eb`, 252 px fixed left sidebar,
content ceiling 1440 px.

All 28 screens are DESKTOP designs with generated HTML/CSS and a
screenshot. Raw downloads are kept outside the repository under
`design-reference/screens/<screen-id>/` (`design.html`,
`screenshot.png`, `screen.json`); re-fetch with
`design-reference/fetch_all.py`.

| # | Stitch screen | Screen ID | Implemented route |
|---|---|---|---|
| 1 | NexHost - Login | 4c9ab4e9d1844b879f192e6d32d74827 | `/login` |
| 2 | NexHost - First-Time Admin Setup | ed5ae7aebe344156aea31108736454ed | `/setup` |
| 3 | NexHost - Dashboard | 39cefdee1b944b948d4fdb39f8cc1012 | `/dashboard` (also `/`) |
| 4 | NexHost - Websites | ea5ff4ebc67748c28c84b40052353e00 | `/websites` |
| 5 | NexHost - Create Website: Basic Details | aea5f3873c554f9ab4568a6d235a3ae0 | `/websites/new` (step 1) |
| 6 | NexHost - Create Website: WordPress Admin Setup | 839c0d70fb3d4db5b1d4cc403fb89ea6 | `/websites/new` (step 2) |
| 7 | NexHost - Create Website: Resources & Review | 06e9374681d14d9b9d677d8c82bcd2c8 | `/websites/new` (step 3) |
| 8 | NexHost - Create Website: Provisioning Progress & Result | 6aba922369ac490a9bff8c18ffba7c44 | `/deployments/{jobId}` + site page job banner |
| 9 | NexHost - example.com: Overview | abeee409d8d64827a6923cc4b91b0a91 | `/websites/{siteId}` |
| 10 | NexHost - example.com: WordPress Manager | 24339a0a9dec4eb2bd517c397a767b01 | `/websites/{siteId}/wordpress` |
| 11 | NexHost - example.com: Domains & SSL | 2b4bd75e6d4b4fd0897c40a9bd2d3488 | `/websites/{siteId}/domains` |
| 12 | NexHost - example.com: Performance & Resources | bb076a4879024497835f194f67abb5f0 | `/websites/{siteId}/performance` |
| 13 | NexHost - example.com: File Manager | 5d9e1f1a8bca41b1a77ea93fda4e1c72 | `/websites/{siteId}/files` |
| 14 | NexHost - example.com: Environment Variables | 17bda893466e4eb392eb1a441728d881 | `/websites/{siteId}/environment` |
| 15 | NexHost - example.com: Docker Compose Editor | 1eadd7bf2e51472e957605b77a68d940 | `/websites/{siteId}/compose` |
| 16 | NexHost - example.com: Database Manager | 913aeb8c24ea46fa8b933dbb74b17d79 | `/websites/{siteId}/database` |
| 17 | NexHost - example.com: Backup Manager | 8c083711391a4ab1ad2d49020e019f17 | `/websites/{siteId}/backups` |
| 18 | NexHost - example.com: Restore Backup Wizard | 421f4ab87705456886bc70b7de347604 | `/websites/{siteId}/restore` |
| 19 | NexHost - Migrate WordPress Website | 6ba4755fbade4b988dee9ce6c3e832fc | `/websites/import` |
| 20 | NexHost - Deployments & Job History | ae7094fec8d34739bb62d2036d00aaeb | `/deployments` (also `/jobs`) |
| 21 | NexHost - Deployment Job Details & Debugging | 692090a1f7f642d29013bcf05b0e67b8 | `/deployments/{jobId}` (also `/jobs/{jobId}`) |
| 22 | NexHost - Logs Viewer | b5fab826e85e43349e8518bd02ff4f46 | `/logs` |
| 23 | NexHost - VPS Server Overview | b691e0ecc4604a349df0c08c33eaf4f4 | `/server` (also `/monitoring`) |
| 24 | NexHost - Docker Resource Inventory | 402c12983a7c46558302bc2e300ac77b | `/server/containers` |
| 25 | NexHost - example.com: Website Settings & Lifecycle | 114b6c876b424b82a672835d4386fc88 | `/websites/{siteId}/settings` |
| 26 | NexHost - System Settings & Infrastructure Defaults | b0655b2c92404c4db1ef82c8b1103abf | `/settings` |
| 27 | NexHost - Administrator Account & Security | f101420c2fa24c598255108ede428bcb | `/settings/security` (also `/account`) |
| 28 | NexHost - Audit Logs & Activity Center | 95c1382926d9427d96aa12a09be1cab9 | `/audit-logs` (also `/audit`) |

Notes:

- The Stitch designs are Tailwind-CDN mockups with demo content
  (fictional "cluster", tenants and people). Per the RK Host brief
  §6–§7 they are used as *design reference only*: the implementation
  reproduces the visual system in the application's existing vanilla
  CSS and connects every element to real backend data. Demo-only
  content (cluster sync, SSO/WebAuthn buttons, tenant switchers) is
  not reproduced because the backend has no such features.
- Screens that had no pre-existing application page — Performance
  (12), Logs (22), Docker inventory (24), site Settings (25),
  Migration (19) — were implemented against existing backend
  capabilities (agent stats/samples, agent logs, agent discovery,
  lifecycle/repair/delete, file/database tooling); see
  RK-HOST-BACKEND-INTEGRATION.md and RK-HOST-REMAINING-ISSUES.md.
