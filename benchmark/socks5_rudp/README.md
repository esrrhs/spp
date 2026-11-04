# SOCKS5 + RUDP local benchmarks
#
# Two fixed scenarios used for CPU/throughput regression:
#
# 1) Bulk upload (long-lived flows)
#    ./run_bulk.sh
#    PROFILE=1 ./run_bulk.sh
#
# 2) Short connections (page-click style churn)
#    ./run_short.sh
#    PROFILE=1 ./run_short.sh
#
# 3) Webpage-like (small req + ~100KB page, SHA-256 integrity)
#    ./run_page.sh
#    WORKERS=100 DURATION=20s PAGESIZE=102400 ./run_page.sh
#
# Common knobs (env):
#   WORKERS DURATION BS REQ PAGESIZE
#   COMPRESS ENCRYPT PROTO KEY
#   PROFILE=1 PROFILE_SECONDS=15
#   KEEP_RUNNING=1
#   SERVER_PORT SOCKS_PORT BACKEND_PORT
#
# Outputs:
#   out/logs/       runtime + loadgen logs
#   out/profiles/   CPU profiles when PROFILE=1
#   out/bin/        built spp/backend/loadgen

Defaults match the tuning runs:
- bulk:  64 workers, 32KB chunks, sink backend, ~30s
- short: 200 workers, 4KB echo (byte-exact verify), echo backend, ~25s
- page:  100 workers, 512B req + 100KB page (len+SHA-256+bytes verify), ~20s

Methodology caveat (measured on macOS, applies to any host with a ~16K
ephemeral-port range):

- Every successful short open consumes ~3 sockets that sit in TIME_WAIT
  (~30s each); a full short run (~8K opens) exhausts the range, and the
  NEXT run collapses to single-digit opens/s with 5s client-deadline
  timeouts — regardless of which code tree is running (verified with a
  self-decay experiment: identical binaries, fresh state run1=830
  opens/s, immediately-following run2=3).
- Comparing trees with back-to-back short runs is therefore invalid.
  Use >=150s of cooldown between runs and take a SINGLE sample per
  state; the collapse signature is opens/s < 100 with avg_lat > 2s
  (fresh-state spread across trees is 746-1003 opens/s).
- bulk/page churn few sockets and are not affected; consecutive runs
  are comparable.
