# Own hPanel

A lightweight, self-hosted WordPress hosting control panel written in Go.

**Login → Create New WordPress Site → enter domain + WordPress details → Create → a real WordPress site is provisioned automatically**: per-site Docker container, dedicated database + least-privilege user in a shared MariaDB, persistent storage, Traefik route via the existing reverse proxy, and a WP-CLI installation — with DNS instructions for the manual Cloudflare step.

## Architecture

```
Panel (unprivileged, NO Docker socket)          Agent (host systemd service)
  Go + html/templates + HTMX                       Unix socket + bearer token
  SQLite (WAL) metadata, durable jobs      ───►    Allowlisted ops only, re-validates
  Sessions, Argon2id, TOTP, audit                  Production guard: managed label
                                                        │ Docker Engine API (unix socket)
        ┌──────────────── Traefik (existing redberyl-traefik, network redberyl-internal)
        ▼                        ▼                          ▼
   WordPress site 1        WordPress site N          wordpress-mariadb (shared, new)
   own container/db        own container/db          one database + user per site
```

- The panel process never opens `/var/run/docker.sock`. All privileged work goes through the agent (`internal/agentproto`), which accepts only `CreateSite, DeleteSite, StartSite, StopSite, RestartSite, BackupSite, RestoreSite, WPCLI, ApplyConfig, RotateDBPassword, DBSize, Ping, Discovery, Stats, Logs`.
- The agent refuses to mutate any container without the `own-hpanel.managed=true` label. Existing production containers (redberyl-*, construction-*, krweb-*, GitHub runners) are never touched.
- WordPress containers publish **no host ports**; the existing Traefik routes them by label (`websecure` + `letsencrypt`).
- The `wpadmin.<domain>` alias is redirect-only to `https://<domain>/wp-admin/` (v1 design decision — no separate login hostname, no cookie problems).
- Jobs are durable and DB-backed with events, retries, cancellation and crash recovery (jobs left `running` at boot become `interrupted`). The WordPress admin password is held in memory only for the create job and is never stored.

## Quick start (local development)

```bash
# Go 1.23+
make build
make test && make vet

# Run the panel locally (agent optional locally; provisioning reports
# "agent unavailable" honestly until an agent + Docker exist):
mkdir -p data
PANEL_ADDR=:8080 PANEL_DATA_DIR=./data SITES_DIR=./data/websites \
BACKUPS_DIR=./data/backups AGENT_SOCKET=./data/agent.sock \
AGENT_TOKEN_FILE=./data/agent.token ./bin/panel
# open http://127.0.0.1:8080  -> first-run administrator setup
```

## Production deployment

See [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md). In short: `deployment/install.sh` first performs **read-only discovery** (container snapshot, Traefik/network/port checks, headroom) and changes nothing; only `--apply` installs, after review and backup. Rollback: [docs/RECOVERY.md](docs/RECOVERY.md). Security model: [SECURITY.md](SECURITY.md).

## Repository layout

| Path | Contents |
|---|---|
| `cmd/panel` | Web UI + API (unprivileged) |
| `cmd/agent` | Privileged provisioning worker |
| `internal/auth` | Argon2id, TOTP, sessions helpers, recovery codes |
| `internal/store` | SQLite (WAL) store + embedded migrations |
| `internal/provisioning` | Durable job runner |
| `internal/docker` | Docker Engine API client over the unix socket (stdlib only) |
| `internal/database` | Validated MariaDB provisioning SQL builders |
| `internal/configgen` | Compose/.env generation + Traefik labels from the approved template |
| `internal/security` | Domain/DB validation, YAML policy, path-traversal-safe files |
| `internal/domains` | DNS lookup, TLS/SSL checks, Cloudflare proxy detection |
| `internal/wordpress` | Allowlisted WP-CLI command builder |
| `internal/backups` | Streaming tar.gz backups, SHA-256 verify, retention, S3 (SigV4) upload |
| `internal/monitoring` | Host CPU/RAM/disk from /proc + Statfs (cached polling) |
| `web/` | Embedded templates + CSS/JS + vendored HTMX 2.0.4 |

## Testing

```bash
go test ./...          # unit tests (no Docker required)
go vet ./...
go test -tags integration ./internal/docker/   # requires a Docker daemon
```

Integration tests that need Docker or the production VPS are build-tagged and were not run in the development VM (no Docker there) — see `REPORT.md` for exactly what was and was not verified.
