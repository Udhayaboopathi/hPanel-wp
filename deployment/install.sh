#!/usr/bin/env bash
# Own hPanel installer - READ-ONLY DISCOVERY FIRST.
#
# Phase A (always run): snapshots the current production state and checks
# every prerequisite. It changes NOTHING.
# Phase B (only with --apply, after you reviewed Phase A output): creates
# panel-owned directories/secrets, installs the agent systemd unit, and
# starts the panel compose stack.
#
# This script NEVER: stops/removes/recreates existing containers, runs
# docker prune / system cleanup, modifies Traefik, Construction MariaDB or
# Redberyl PostgreSQL, upgrades the OS/Docker, or publishes ports 80/443.
set -euo pipefail

APPLY=0
[[ "${1:-}" == "--apply" ]] && APPLY=1

BASE=/opt/own-hpanel
SNAPSHOT="$BASE/discovery-$(date +%Y%m%d-%H%M%S).txt"

echo "=== Own hPanel discovery (read-only) ==="
mkdir -p "$BASE" 2>/dev/null || { echo "Cannot create $BASE (run with sudo)"; exit 1; }
{
  echo "--- date ---";           date -Is
  echo "--- os ---";             cat /etc/os-release | head -3
  echo "--- cpu/mem/disk ---";   nproc; free -h; df -h /
  echo "--- docker version ---"; docker version --format '{{.Server.Version}}' || echo "DOCKER NOT AVAILABLE"
  echo "--- containers BEFORE (id, name, image, status) ---"
  docker ps -a --format '{{.ID}} {{.Names}} {{.Image}} {{.Status}}'
  echo "--- existing production containers expected ---"
  for c in krweb-ui redberyl-api redberyl-ui redberyl-rustfs redberyl-postgres \
           construction-api construction-ui construction-mariadb \
           redberyl-postgres-proxy redberyl-traefik; do
    if docker inspect "$c" >/dev/null 2>&1; then echo "FOUND  $c"; else echo "ABSENT $c"; fi
  done
  echo "--- traefik ---"
  docker inspect redberyl-traefik --format '{{.Name}} state={{.State.Status}} image={{.Config.Image}}' || echo "TRAEFIK NOT FOUND"
  echo "--- dedicated MariaDB of the hPanel stack (created by compose; may not exist yet) ---"
  DB_CONTAINER="${DB_CONTAINER:-wordpress-mariadb}"
  if docker inspect "$DB_CONTAINER" >/dev/null 2>&1; then
    docker inspect "$DB_CONTAINER" --format '{{.Name}} state={{.State.Status}} image={{.Config.Image}}'
  else
    echo "DB container $DB_CONTAINER does not exist yet - it will be created by docker compose from compose.yaml (service wordpress-mariadb)"
  fi
  echo "NOTE: construction-mariadb (listed above) is a pre-existing production container."
  echo "      Own hPanel does NOT use it in any way - it is purely protected/untouched."
  echo "--- external DB endpoint (warning-only check; wordpress-mariadb publishes this port once running) ---"
  DB_HOST="${DB_HOST:-db.krwebsolution.com}"
  DB_EXTERNAL_PORT="${DB_EXTERNAL_PORT:-3024}"
  if timeout 5 bash -c "exec 3<>/dev/tcp/$DB_HOST/$DB_EXTERNAL_PORT" 2>/dev/null; then
    echo "DB endpoint $DB_HOST:$DB_EXTERNAL_PORT reachable"
  else
    echo "WARNING: DB endpoint $DB_HOST:$DB_EXTERNAL_PORT not reachable from here (expected before the stack is up, or while the DNS record is Cloudflare-proxied; WP containers reach the DB internally over their Docker network regardless)"
  fi
  echo "--- networks ---"; docker network ls
  docker network inspect redberyl-internal >/dev/null 2>&1 && echo "network redberyl-internal: OK" || echo "network redberyl-internal: MISSING"
  echo "--- ports 80/443 (must belong to Traefik) ---"
  docker ps --format '{{.Names}} {{.Ports}}' | grep -E '0\.0\.0\.0:(80|443)->' || true
  ss -tlnp 2>/dev/null | grep -E ':(80|443)\s' || true
  echo "--- headroom ---"
  free -m | awk 'NR==2{print "RAM available MB:", $7}'
  df -m / | awk 'NR==2{print "Disk available MB:", $4}'
} | tee "$SNAPSHOT"
echo "Discovery snapshot saved to $SNAPSHOT"
echo "Keep this file: it is the BEFORE record for the production protection test."

# Hard prerequisite checks (still read-only).
docker network inspect redberyl-internal >/dev/null 2>&1 || { echo "ABORT: redberyl-internal network missing"; exit 1; }
docker inspect redberyl-traefik >/dev/null 2>&1 || { echo "ABORT: redberyl-traefik not found"; exit 1; }
AVAIL_MB=$(df -m / | awk 'NR==2{print $4}')
[[ "$AVAIL_MB" -gt 5120 ]] || { echo "ABORT: less than 5 GB free disk"; exit 1; }
DB_CONTAINER="${DB_CONTAINER:-wordpress-mariadb}"
if docker inspect "$DB_CONTAINER" >/dev/null 2>&1; then
  DB_STATE=$(docker inspect "$DB_CONTAINER" --format '{{.State.Status}}')
  echo "dedicated DB container $DB_CONTAINER already exists (state=$DB_STATE) - compose will reconcile it; its data volume is preserved."
else
  echo "dedicated DB container $DB_CONTAINER will be created by docker compose (image mariadb:11.4, config in compose.yaml)."
fi
# No pre-existing database is a prerequisite any more: the stack brings
# its own. construction-mariadb is NOT checked here because Own hPanel
# does not depend on it (or touch it) at all.
if docker inspect own-hpanel >/dev/null 2>&1; then
  echo "NOTE: own-hpanel panel container already exists."
fi

if [[ "$APPLY" -eq 0 ]]; then
  echo
  echo "Discovery complete. NOTHING was changed."
  echo "Review $SNAPSHOT, take a backup, then re-run with --apply to install."
  exit 0
fi

echo "=== Applying (panel-owned resources only) ==="
install -d -m 750 "$BASE"/{panel,state,secrets,logs,websites,backups}
install -d -m 750 /run/own-hpanel

# Secrets: generated once, never overwritten, never printed.
# The MariaDB ROOT password belongs to the stack's own dedicated
# wordpress-mariadb container: it is generated here if missing and becomes
# that container's root password on its FIRST initialization (empty data
# volume). The common admin user's password is NOT generated - it is
# supplied by the operator (GitHub Secrets in CI, or a 0600 file at
# $BASE/secrets/db_admin_password.txt referenced by DB_ADMIN_PASSWORD_FILE).
if [[ ! -f "$BASE/secrets/agent.token" ]]; then
  openssl rand -hex 32 > "$BASE/secrets/agent.token"; chmod 600 "$BASE/secrets/agent.token"
fi
if [[ ! -f "$BASE/secrets/mariadb_root_password.txt" ]]; then
  openssl rand -hex 24 > "$BASE/secrets/mariadb_root_password.txt"; chmod 600 "$BASE/secrets/mariadb_root_password.txt"
  echo "MariaDB root password generated into $BASE/secrets/mariadb_root_password.txt (0600, never printed)"
fi
if [[ ! -f "$BASE/secrets/db_admin_password.txt" && -z "${DB_ADMIN_PASSWORD:-}" ]]; then
  echo "WARNING: no DB admin credential found ($BASE/secrets/db_admin_password.txt missing and DB_ADMIN_PASSWORD unset)."
  echo "         The agent/panel will not be able to provision databases until it is provided (0600 file, never printed)."
fi
# Panel reads the agent token through the shared /run/own-hpanel mount.
cp "$BASE/secrets/agent.token" /run/own-hpanel/agent.token; chmod 640 /run/own-hpanel/agent.token

# Agent systemd service (host process; the only Docker-socket holder).
install -m 644 deployment/own-hpanel-agent.service /etc/systemd/system/own-hpanel-agent.service
systemctl daemon-reload
systemctl enable --now own-hpanel-agent
systemctl --no-pager status own-hpanel-agent | head -5

# Panel stack (panel-owned containers only: the panel + the dedicated
# wordpress-mariadb defined in compose.yaml; pre-existing production
# containers are never started/stopped by it).
: "${PANEL_DOMAIN:?Set PANEL_DOMAIN, e.g. PANEL_DOMAIN=panel.example.com}"
export PANEL_DOMAIN
docker compose up -d --build

echo "--- containers AFTER (compare with the BEFORE snapshot) ---"
docker ps -a --format '{{.ID}} {{.Names}} {{.Image}} {{.Status}}'
echo
echo "Panel: https://$PANEL_DOMAIN  (first visit opens the administrator setup)"
echo "Verify every pre-existing container is still running and unchanged."
