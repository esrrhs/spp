#!/usr/bin/env bash
# Scenario A: bulk upload over SOCKS5 + RUDP
# Simulates few long-lived high-throughput flows (default: 64 workers, 32KB chunks).
#
# Usage:
#   ./run_bulk.sh
#   PROFILE=1 ./run_bulk.sh
#   WORKERS=128 DURATION=60s COMPRESS=0 ./run_bulk.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "$SCRIPT_DIR/common.sh"

WORKERS="${WORKERS:-64}"
DURATION="${DURATION:-30s}"
BS="${BS:-32768}"
TAG="${TAG:-bulk}"

cleanup() {
  if [[ "$KEEP_RUNNING" != "1" ]]; then
    stop_stack
  else
    log "KEEP_RUNNING=1, stack left up (socks=:$SOCKS_PORT server=:$SERVER_PORT)"
  fi
}
trap cleanup EXIT

build_tools
start_stack sink

run_loadgen "$TAG" \
  -proxy "127.0.0.1:${SOCKS_PORT}" \
  -target "127.0.0.1:${BACKEND_PORT}" \
  -c "$WORKERS" \
  -d "$DURATION" \
  -bs "$BS" \
  -mode upload

print_summary "$TAG"
