#!/bin/bash
# Own hPanel - first-init provisioning grants for the dedicated MariaDB
# container (compose service wordpress-mariadb).
#
# Mounted to /docker-entrypoint-initdb.d/01-grants.sh by compose.yaml.
# Init scripts run ONLY when the data volume is empty (first boot); the
# deploy workflow's "ensure panel DB" step repeats the same grants
# idempotently via root exec for already-initialized volumes.
#
# Why: the MariaDB entrypoint grants MARIADB_USER rights ONLY on
# MARIADB_DATABASE. The hPanel agent, however, provisions with that common
# account - it must be able to CREATE DATABASE, CREATE USER and GRANT for
# every new WordPress site. This script upgrades the common account to a
# full admin OF THIS CONTAINER ONLY.
#
# Scope note: wordpress-mariadb is a dedicated container that serves ONLY
# Own hPanel and the WordPress sites it manages. A full grant here gives
# the common provisioning account rights over panel + WordPress databases
# on this one container - it has NO rights on any other database server
# (in particular not on the separate production construction-mariadb,
# which Own hPanel never connects to).
#
# Environment available here (set by the entrypoint / compose):
#   MARIADB_ROOT_PASSWORD - root password (the entrypoint also exports the
#                           _FILE-resolved value under this name)
#   MARIADB_USER          - the common provisioning account name
set -e

if [ -z "${MARIADB_USER:-}" ]; then
  echo "01-grants: MARIADB_USER is not set - skipping provisioning grants"
  exit 0
fi

# User names are validated identifiers in the panel config; still, refuse
# anything that is not a plain identifier before interpolating into SQL.
case "$MARIADB_USER" in
  *[!A-Za-z0-9_]*)
    echo "01-grants: MARIADB_USER '$MARIADB_USER' is not a plain identifier - refusing to grant" >&2
    exit 1
    ;;
esac

mariadb -uroot -p"$MARIADB_ROOT_PASSWORD" <<-EOSQL
	GRANT ALL PRIVILEGES ON *.* TO '${MARIADB_USER}'@'%' WITH GRANT OPTION;
	GRANT ALL PRIVILEGES ON *.* TO '${MARIADB_USER}'@'localhost' WITH GRANT OPTION;
	FLUSH PRIVILEGES;
EOSQL

echo "01-grants: provisioning grants applied for '${MARIADB_USER}' (this container only)"
