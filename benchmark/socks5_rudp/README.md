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
#   COMPRESS ENCRYPT PROTO KEY MAXCONN
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
- MAXCONN defaults to 200000 (product default 10240 is too low for short-conn churn)
