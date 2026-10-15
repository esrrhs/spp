#!/usr/bin/env python3
"""Break a spp CPU profile down by category instead of by function.

`go tool pprof -top` answers "which function is hot", but the interesting
question for a proxy is "which *kind* of work is hot": real I/O, scheduler
overhead, compression, GC, or our own code. This sums each function's flat
samples into those buckets.

Usage:
    PROFILE=1 ./run_bulk.sh                     # writes out/profiles/*.pb.gz
    ./cpu_breakdown.py out/profiles/bulk_server_cpu.pb.gz
    ./cpu_breakdown.py out/profiles/*.pb.gz

Caveat that matters: on macOS a large share lands in "platform primitives"
(pthread_cond_*, usleep, kevent). Those are Darwin-specific; on Linux the
same work is futex/epoll and costs much less. Do not extrapolate that
bucket to a Linux cloud box.
"""

import collections
import re
import subprocess
import sys

# Darwin-only runtime primitives. Linux has no pthread_cond here; it uses
# futex (and epoll instead of kevent), so this bucket shrinks a lot there.
PLATFORM = (
    'pthread_cond', 'usleep', 'kevent', 'pthread_kill',
    'notetsleep', 'osyield', 'nanotime',
)
GC = (
    'gcDrain', 'scan', 'sweep', 'madvise', 'mallocgc', 'gcBg', 'gcAssist',
    'greyobject', 'markroot', 'freeSpan', 'deferToSpan',
)
SCHED = (
    'schedule', 'findRunnable', 'stealWork', 'goready', 'wakep', 'mcall',
    'park', 'ready', 'netpoll', 'startm', 'runq',
)

LINE = re.compile(
    r'\s+([0-9.]+)s\s+[0-9.]+%\s+[0-9.]+%\s+([0-9.]+)s\s+[0-9.]+%\s+(.+)$'
)


def classify(fn: str) -> str:
    if 'syscall.' in fn:
        return 'A. syscall (real I/O)'
    if fn.startswith('runtime.'):
        n = fn.split('.', 1)[1]
        if any(k in n for k in PLATFORM):
            return 'B1. platform primitives (Darwin)'
        if any(k in n for k in GC):
            return 'D. GC / memory'
        if any(k in n for k in SCHED):
            return 'B2. Go scheduler'
        return 'E. runtime other'
    if fn.startswith('internal/sync') or fn.startswith('sync.'):
        return 'C. lock contention'
    if 'klauspost/compress' in fn or 'zstd' in fn or 'zlib' in fn or 'flate' in fn:
        return 'F. compression'
    if 'crypto/chacha20' in fn or 'crypto/aes' in fn or 'cipher' in fn:
        return 'G. encryption (AEAD)'
    if 'esrrhs/spp/proxy' in fn:
        return 'H. spp code'
    if 'esrrhs/gohome' in fn:
        return 'I. gohome transport'
    if 'protobuf' in fn:
        return 'J. protobuf'
    return 'K. other'


def breakdown(path: str):
    out = subprocess.run(
        ['go', 'tool', 'pprof', '-top', '-nodecount=2000', path],
        capture_output=True, text=True,
    ).stdout
    buckets = collections.Counter()
    total = 0.0
    for line in out.splitlines():
        m = LINE.match(line)
        if not m:
            continue
        flat = float(m.group(1))
        buckets[classify(m.group(3).strip())] += flat
        total += flat
    return buckets, total


def main() -> int:
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    for path in sys.argv[1:]:
        label = 'SERVER' if 'server' in path else 'CLIENT'
        buckets, total = breakdown(path)
        print(f'===== {label}  {path.split("/")[-1]}  total {total:.1f}s =====')
        for k, v in sorted(buckets.items()):
            if v / total >= 0.005:
                print(f'  {k:<36} {v:6.2f}s  {v / total * 100:5.1f}%')
        print()
    return 0


if __name__ == '__main__':
    sys.exit(main())
