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
if docker inspect wordpress-mariadb >/dev/null 2>&1; then
  echo "NOTE: wordpress-mariadb already exists; --apply will not recreate it."
fi
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
if [[ ! -f "$BASE/secrets/agent.token" ]]; then
  openssl rand -hex 32 > "$BASE/secrets/agent.token"; chmod 600 "$BASE/secrets/agent.token"
fi
if [[ ! -f "$BASE/secrets/mariadb_root_password.txt" ]]; then
  openssl rand -base64 36 | tr -d '\n' > "$BASE/secrets/mariadb_root_password.txt"; chmod 600 "$BASE/secrets/mariadb_root_password.txt"
fi
install -d -m 750 ./secrets 2>/dev/null || true
if [[ ! -f ./secrets/mariadb_root_password.txt ]]; then
  cp "$BASE/secrets/mariadb_root_password.txt" ./secrets/mariadb_root_password.txt; chmod 600 ./secrets/mariadb_root_password.txt
fi
# Panel reads the agent token through the shared /run/own-hpanel mount.
cp "$BASE/secrets/agent.token" /run/own-hpanel/agent.token; chmod 640 /run/own-hpanel/agent.token

# Agent systemd service (host process; the only Docker-socket holder).
install -m 644 deployment/own-hpanel-agent.service /etc/systemd/system/own-hpanel-agent.service
systemctl daemon-reload
systemctl enable --now own-hpanel-agent
systemctl --no-pager status own-hpanel-agent | head -5

# Panel + shared MariaDB stack.
: "${PANEL_DOMAIN:?Set PANEL_DOMAIN, e.g. PANEL_DOMAIN=panel.example.com}"
export PANEL_DOMAIN
docker compose pull wordpress-mariadb || true
docker compose up -d --build

echo "--- containers AFTER (compare with the BEFORE snapshot) ---"
docker ps -a --format '{{.ID}} {{.Names}} {{.Image}} {{.Status}}'
echo
echo "Panel: https://$PANEL_DOMAIN  (first visit opens the administrator setup)"
echo "Verify every pre-existing container is still running and unchanged."
