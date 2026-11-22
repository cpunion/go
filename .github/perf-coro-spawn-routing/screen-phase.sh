#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
root="$(pwd -P)"
export GOENV=off GOTOOLCHAIN=local GOWORK=off GO111MODULE=off GOEXPERIMENT=coro
export GOCACHE="$root/cache"
python3 -B phase.py
cd bench
"$root/sdk/bin/go" test -c -tags=corocoldphase -gcflags=-d=coro=4 -overlay="$root/overlays/phase.json" -o phase.test . > ../validation/build-phase.log 2>&1
"$root/sdk/bin/go" tool nm phase.test | grep -E 'runtime.coro(ColdDump|RoutePhase)' > ../validation/phase-symbols.txt
mkdir -p ../validation/phase
for trial in {1..30}; do
    for point in ColdPhase16:4 ColdPhaseLight64:2 ColdPhaseLight64:4; do
        name="${point%:*}"
        procs="${point##*:}"
        GOMAXPROCS="$procs" ./phase.test -test.run='^$' -test.bench="^Benchmark${name}$" -test.benchtime=1x > "../validation/phase/$name-$procs-$trial.log" 2>&1
    done
done
