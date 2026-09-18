#!/usr/bin/env bash
# Scenario B: short-lived SOCKS5 + RUDP connections
# Simulates many page-click style short requests (default: 200 workers, 4KB echo each).
#
# Usage:
#   ./run_short.sh
#   PROFILE=1 ./run_short.sh
#   WORKERS=400 DURATION=40s BS=2048 ./run_short.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "$SCRIPT_DIR/common.sh"

WORKERS="${WORKERS:-200}"
DURATION="${DURATION:-25s}"
BS="${BS:-4096}"
TAG="${TAG:-short}"

cleanup() {
  if [[ "$KEEP_RUNNING" != "1" ]]; then
    stop_stack
  else
    log "KEEP_RUNNING=1, stack left up (socks=:$SOCKS_PORT server=:$SERVER_PORT)"
  fi
}
trap cleanup EXIT

build_tools
start_stack echo

run_loadgen "$TAG" \
  -proxy "127.0.0.1:${SOCKS_PORT}" \
  -target "127.0.0.1:${BACKEND_PORT}" \
  -c "$WORKERS" \
  -d "$DURATION" \
  -bs "$BS" \
  -mode short

print_summary "$TAG"
