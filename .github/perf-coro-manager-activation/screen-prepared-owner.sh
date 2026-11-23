#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
root="$(pwd -P)"
export GOENV=off GOWORK=off GOTOOLCHAIN=local GOCACHE="$root/cache" GO111MODULE=off CGO_ENABLED=1 GOMAXPROCS=8
python3 -B prepared-owner.py
cd bench
modes=(stock base preparenone prepareg preparecontext preparestack)
for mode in "${modes[@]:2}"; do
    GOEXPERIMENT=coro "$root/sdk/bin/go" test -c -gcflags=-d=coro=4 -overlay="$root/overlays/$mode.json" -o "$mode.test" . > "../validation/build-$mode.log" 2>&1
done
bash "$root/run-progress.sh" "${modes[@]:2}"
python3 -B bench.py run 54 ParallelYieldWork16:4:1,ParallelYieldLight64:4:1,ParallelYieldWork16:2:1,ParallelYieldLight64:2:1,ParallelYieldWork:4:1,ParallelYieldHeavy64:4:1 "${modes[@]}" > ../validation/prepared-cold.jsonl
python3 -B bench.py run 42 ParallelYieldWork16:4:4,ParallelYieldLight64:4:4,ParallelYieldHeavy64:4:4,ParallelYieldWork:2:4 "${modes[@]}" > ../validation/prepared-warm.jsonl
python3 -B bench.py run 30 TaskBurst100000:4:4,TaskPark100000:4:4,MixedParkScalarFirst100000:4:4,TaskSequence:2:10000,YieldEntry:2:10000 "${modes[@]}" > ../validation/prepared-guards.jsonl
