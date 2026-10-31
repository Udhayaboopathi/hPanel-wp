#!/usr/bin/env bash
# Local development helper: creates a throwaway agent token + data dirs and
# starts the agent IF a Docker socket exists on this machine. On machines
# without Docker the agent will fail to serve Docker operations and the panel
# will report it as unavailable - which is the honest local-dev behaviour.
set -euo pipefail
mkdir -p data
[[ -f data/agent.token ]] || openssl rand -hex 32 > data/agent.token
chmod 600 data/agent.token
export AGENT_SOCKET="$PWD/data/agent.sock"
export AGENT_TOKEN_FILE="$PWD/data/agent.token"
export SITES_DIR="$PWD/data/websites"
export BACKUPS_DIR="$PWD/data/backups"
exec ./bin/agent
