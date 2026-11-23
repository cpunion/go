#!/usr/bin/env bash
set -euo pipefail
# Linux loopback can take about ten seconds to drain each small TCP window.
# Run these I/O checks in parallel and finish all of them before any timing.
printf '%s\n' "$@" | xargs -P 4 -n 1 bash -c '
    set -euo pipefail
    mode="$1"
    GODEBUG=asyncpreemptoff=1 GOMAXPROCS=4 "./$mode.test" \
        -test.v -test.timeout=3m -test.count=10 \
        -test.run "^Test(ParallelSpawnProgress|PublicWriteBlockingProgress)$" \
        > "../validation/$mode-progress.log" 2>&1
' bash
