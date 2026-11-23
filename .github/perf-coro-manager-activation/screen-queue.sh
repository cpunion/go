#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
root="$(pwd -P)"
export GOENV=off GOWORK=off GOTOOLCHAIN=local GOCACHE="$root/cache" GO111MODULE=off
export CGO_ENABLED=1 GOMAXPROCS=8
python3 -B queue.py
cd bench
go test -c -o stock.test . > ../validation/build-stock.log 2>&1
modes=(base tail next global eager)
for mode in "${modes[@]}"; do
    flags=()
    if [[ "$mode" != base ]]; then flags=(-overlay="$root/overlays/$mode.json"); fi
    GOEXPERIMENT=coro "$root/sdk/bin/go" test -c -gcflags=-d=coro=4 "${flags[@]}" -o "$mode.test" . > "../validation/build-$mode.log" 2>&1
done
shasum -a 256 *.test > ../validation/binary-hashes.txt
bash "$root/run-progress.sh" "${modes[@]}"
modes=(stock "${modes[@]}")
python3 -B bench.py run 48 ParallelYieldWork16:4:1,ParallelYieldLight64:4:1,ParallelYieldWork16:2:1,ParallelYieldLight64:2:1,ParallelYieldWork:4:1,ParallelYieldHeavy64:4:1 "${modes[@]}" > ../validation/queue-cold.jsonl
python3 -B bench.py run 36 ParallelYieldWork16:4:4,ParallelYieldLight64:4:4,ParallelYieldHeavy64:4:4,ParallelYieldWork:2:4 "${modes[@]}" > ../validation/queue-warm.jsonl
python3 -B bench.py run 28 TaskBurst100000:4:4,TaskPark100000:4:4,MixedParkScalarFirst100000:4:4,TaskSequence:2:10000,YieldEntry:2:10000 "${modes[@]}" > ../validation/queue-guards.jsonl
