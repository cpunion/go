#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
root="$(pwd -P)"
export GOENV=off GOWORK=off GOTOOLCHAIN=local GOCACHE="$root/cache" GO111MODULE=off
export GOEXPERIMENT=coro CGO_ENABLED=0
cd progress
modes=(base tail next global eager timerreuseempty handoff managerworker managerone parked parkedfast parkedslow)
for mode in "${modes[@]}"; do
    flags=()
    if [[ "$mode" != base ]]; then flags=(-overlay="$root/overlays/$mode.json"); fi
    "$root/sdk/bin/go" build -gcflags=-d=coro=4 "${flags[@]}" -o "$mode.test" main.go > "../validation/build-progress-$mode.log" 2>&1
done
"$root/sdk/bin/go" tool objdump -s 'main.parent\.coro\.func1$' base.test | grep -E 'CALL.*coroSpawnScalarBatch' > ../validation/progress-route.asm
python3 -B - "${modes[@]}" > ../validation/no-timer-progress.jsonl <<'PY'
import json
import os
import subprocess
import sys
from time import monotonic
for procs in (2, 4):
    env = dict(os.environ, GOMAXPROCS=str(procs), GODEBUG="asyncpreemptoff=1", GOGC="off")
    for mode in sys.argv[1:]:
        for trial in range(12):
            start = monotonic()
            run = subprocess.run([f"./{mode}.test"], capture_output=True, env=env, timeout=6)
            assert run.returncode == 0, (mode, procs, run.stderr.decode())
            print(json.dumps([mode, procs, trial, monotonic() - start]), flush=True)
PY
