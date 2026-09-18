#!/usr/bin/env bash
# Scenario C: webpage-like short connections with integrity checks.
# After OPEN: small request (~512B) + larger page response (~100KB).
# Verifies response length, SHA-256, and exact bytes.
#
# Usage:
#   ./run_page.sh
#   WORKERS=100 DURATION=20s REQ=1024 PAGESIZE=131072 ./run_page.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "$SCRIPT_DIR/common.sh"

WORKERS="${WORKERS:-100}"
DURATION="${DURATION:-20s}"
REQ="${REQ:-512}"
PAGESIZE="${PAGESIZE:-102400}"
TAG="${TAG:-page}"

cleanup() {
  if [[ "$KEEP_RUNNING" != "1" ]]; then
    stop_stack
  else
    log "KEEP_RUNNING=1, stack left up (socks=:$SOCKS_PORT server=:$SERVER_PORT)"
  fi
}
trap cleanup EXIT

build_tools

# start_stack expects backend mode name; page is custom — start manually pieces.
stop_stack
kill_port_users "$SOCKS_PORT" || true
kill_port_users "$BACKEND_PORT" || true
kill_port_users "$SERVER_PPROF" || true
kill_port_users "$CLIENT_PPROF" || true
: > "$OUT_DIR/pids"

log "start backend mode=page pagesize=$PAGESIZE :$BACKEND_PORT"
setsid "$BACKEND_BIN" -addr "127.0.0.1:${BACKEND_PORT}" -mode page -pagesize "$PAGESIZE" \
  </dev/null >"$LOG_DIR/backend.log" 2>&1 &
echo $! >> "$OUT_DIR/pids"

log "start spp server proto=$PROTO compress=$COMPRESS encrypt=$ENCRYPT maxconn=$MAXCONN"
setsid "$SPP_BIN" \
  -type server \
  -listen ":${SERVER_PORT}" \
  -proto "$PROTO" \
  -key "$KEY" \
  -encrypt "$ENCRYPT" \
  -compress "$COMPRESS" \
  -maxconn "$MAXCONN" \
  -noprint 1 -nolog 1 -loglevel error \
  -profile "$SERVER_PPROF" \
  </dev/null >"$LOG_DIR/server.log" 2>&1 &
echo $! >> "$OUT_DIR/pids"

log "start socks5_client :$SOCKS_PORT -> server :$SERVER_PORT"
setsid "$SPP_BIN" \
  -type socks5_client \
  -server "127.0.0.1:${SERVER_PORT}" \
  -fromaddr ":${SOCKS_PORT}" \
  -proxyproto tcp \
  -proto "$PROTO" \
  -key "$KEY" \
  -encrypt "$ENCRYPT" \
  -compress "$COMPRESS" \
  -maxconn "$MAXCONN" \
  -noprint 1 -nolog 1 -loglevel error \
  -profile "$CLIENT_PPROF" \
  </dev/null >"$LOG_DIR/client.log" 2>&1 &
echo $! >> "$OUT_DIR/pids"

wait_http "http://127.0.0.1:${CLIENT_PPROF}/debug/pprof/" || {
  log "client pprof not ready; see $LOG_DIR/client.log"
  exit 1
}
wait_http "http://127.0.0.1:${SERVER_PPROF}/debug/pprof/" || {
  log "server pprof not ready; see $LOG_DIR/server.log"
  exit 1
}
wait_tcp 127.0.0.1 "$SOCKS_PORT" || {
  log "socks port :$SOCKS_PORT not ready; see $LOG_DIR/client.log"
  exit 1
}
sleep 1
log "stack ready (page req=${REQ}B page=${PAGESIZE}B)"

run_loadgen "$TAG" \
  -proxy "127.0.0.1:${SOCKS_PORT}" \
  -target "127.0.0.1:${BACKEND_PORT}" \
  -c "$WORKERS" \
  -d "$DURATION" \
  -req "$REQ" \
  -pagesize "$PAGESIZE" \
  -mode page

print_summary "$TAG"
