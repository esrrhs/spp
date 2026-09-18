#!/usr/bin/env bash
# Shared helpers for SOCKS5 + RUDP local benchmarks.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BENCH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="${OUT_DIR:-$BENCH_DIR/out}"
BIN_DIR="${BIN_DIR:-$OUT_DIR/bin}"
LOG_DIR="${LOG_DIR:-$OUT_DIR/logs}"
PROF_DIR="${PROF_DIR:-$OUT_DIR/profiles}"

SPP_BIN="${SPP_BIN:-$BIN_DIR/spp}"
BACKEND_BIN="${BACKEND_BIN:-$BIN_DIR/backend}"
LOADGEN_BIN="${LOADGEN_BIN:-$BIN_DIR/loadgen}"

SERVER_PORT="${SERVER_PORT:-18888}"
SOCKS_PORT="${SOCKS_PORT:-11080}"
BACKEND_PORT="${BACKEND_PORT:-19999}"
SERVER_PPROF="${SERVER_PPROF:-16060}"
CLIENT_PPROF="${CLIENT_PPROF:-16061}"

KEY="${KEY:-bench-auth-key}"
ENCRYPT="${ENCRYPT:-bench-encrypt-key}"
COMPRESS="${COMPRESS:-128}"
PROTO="${PROTO:-rudp}"
# Short-conn churn can open >10k sockets before idle timeout reclaims them.
# Default product MaxSonny=10240 would reject mid-test and poison the result.
MAXCONN="${MAXCONN:-200000}"

PROFILE="${PROFILE:-0}"          # 1 = capture CPU profiles during load
PROFILE_SECONDS="${PROFILE_SECONDS:-15}"
KEEP_RUNNING="${KEEP_RUNNING:-0}" # 1 = leave spp running after load

mkdir -p "$BIN_DIR" "$LOG_DIR" "$PROF_DIR"

log() { printf '[%s] %s\n' "$(date '+%H:%M:%S')" "$*"; }

kill_port_users() {
  local port="$1"
  if command -v fuser >/dev/null 2>&1; then
    timeout 2 fuser -k "${port}/tcp" >/dev/null 2>&1 || true
    timeout 2 fuser -k "${port}/udp" >/dev/null 2>&1 || true
  fi
}

stop_stack() {
  if [[ -f "$OUT_DIR/pids" ]]; then
    while read -r pid; do
      [[ -n "${pid:-}" ]] || continue
      kill "$pid" >/dev/null 2>&1 || true
    done < "$OUT_DIR/pids" || true
    rm -f "$OUT_DIR/pids"
  fi
  # Best-effort cleanup by binary path (suppress job-control noise).
  pkill -f "$SPP_BIN " >/dev/null 2>&1 || true
  pkill -f "$BACKEND_BIN " >/dev/null 2>&1 || true
  pkill -f "$LOADGEN_BIN " >/dev/null 2>&1 || true
  wait >/dev/null 2>&1 || true
  sleep 0.3
}

build_tools() {
  log "building spp + bench tools"
  (cd "$ROOT" && go build -o "$SPP_BIN" .)
  (cd "$BENCH_DIR/cmd/backend" && go build -o "$BACKEND_BIN" .)
  (cd "$BENCH_DIR/cmd/loadgen" && go build -o "$LOADGEN_BIN" .)
}

wait_http() {
  local url="$1"
  local n=0
  while (( n < 50 )); do
    if curl -sf -o /dev/null "$url"; then
      return 0
    fi
    sleep 0.1
    n=$((n + 1))
  done
  return 1
}

wait_tcp() {
  local host="$1"
  local port="$2"
  local n=0
  while (( n < 50 )); do
    if (echo >/dev/tcp/${host}/${port}) >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
    n=$((n + 1))
  done
  return 1
}

start_stack() {
  local backend_mode="$1"
  stop_stack
  kill_port_users "$SOCKS_PORT"
  kill_port_users "$BACKEND_PORT"
  kill_port_users "$SERVER_PPROF"
  kill_port_users "$CLIENT_PPROF"

  : > "$OUT_DIR/pids"

  log "start backend mode=$backend_mode :$BACKEND_PORT"
  setsid "$BACKEND_BIN" -addr "127.0.0.1:${BACKEND_PORT}" -mode "$backend_mode" \
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
  # Allow RUDP main channel login to finish before flooding dials.
  sleep 1
  log "stack ready"
}

capture_profiles() {
  local tag="$1"
  local secs="$PROFILE_SECONDS"
  log "capturing CPU profiles ${secs}s (tag=$tag)"
  curl -s "http://127.0.0.1:${SERVER_PPROF}/debug/pprof/profile?seconds=${secs}" \
    -o "$PROF_DIR/${tag}_server_cpu.pb.gz" &
  local p1=$!
  curl -s "http://127.0.0.1:${CLIENT_PPROF}/debug/pprof/profile?seconds=${secs}" \
    -o "$PROF_DIR/${tag}_client_cpu.pb.gz" &
  local p2=$!
  wait "$p1" "$p2" || true
  log "profiles: $PROF_DIR/${tag}_server_cpu.pb.gz $PROF_DIR/${tag}_client_cpu.pb.gz"
}

run_loadgen() {
  local tag="$1"
  shift
  local load_log="$LOG_DIR/${tag}_loadgen.log"
  log "loadgen -> $load_log"
  if [[ "$PROFILE" == "1" ]]; then
    (
      sleep 3
      capture_profiles "$tag"
    ) &
    local prof_watcher=$!
  fi
  set +e
  "$LOADGEN_BIN" "$@" | tee "$load_log"
  local rc=${PIPESTATUS[0]}
  set -e
  if [[ "$PROFILE" == "1" ]]; then
    wait "$prof_watcher" 2>/dev/null || true
  fi
  return "$rc"
}

print_summary() {
  local tag="$1"
  echo
  echo "======== $tag summary ========"
  if [[ -f "$LOG_DIR/${tag}_loadgen.log" ]]; then
    tail -5 "$LOG_DIR/${tag}_loadgen.log" || true
  fi
  echo "logs:     $LOG_DIR"
  echo "profiles: $PROF_DIR (PROFILE=$PROFILE)"
  if [[ "$PROFILE" == "1" ]]; then
    echo "analyze:  go tool pprof -http=:0 $PROF_DIR/${tag}_client_cpu.pb.gz"
    echo "          go tool pprof -top -cum $PROF_DIR/${tag}_server_cpu.pb.gz"
  fi
}
