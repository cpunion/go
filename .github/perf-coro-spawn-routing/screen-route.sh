#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
export GOENV=off GOWORK=off GOTOOLCHAIN=local GOCACHE="$PWD/cache" GO111MODULE=off CGO_ENABLED=1 GOMAXPROCS=8
root="$(pwd -P)"
sdk="$root/sdk/bin/go"
"$sdk" version > validation/sdk-version.txt
go version > validation/stock-version.txt
python3 -B route.py
cd bench
go test -c -o stock.test . > ../validation/build-stock.log 2>&1
modes=(base joined joinedwide managedfirst manageddeferred earlytwo earlyall)
for mode in "${modes[@]}"; do
    flags=()
    if [[ "$mode" != base ]]; then flags=(-overlay="$root/overlays/$mode.json"); fi
    GOEXPERIMENT=coro "$sdk" test -c -gcflags=-d=coro=4 "${flags[@]}" -o "$mode.test" . > "../validation/build-$mode.log" 2>&1
done
shasum -a 256 *.test > ../validation/binary-hashes.txt
"$sdk" tool objdump -s 'parallelYieldWork\.coro\.func1$' base.test | grep -E 'CALL.*coroSpawnScalarBatch' > ../validation/native-spawn.asm
python3 -B bench.py run 40 ParallelYieldWork16:4:1,ParallelYieldLight64:4:1,ParallelYieldWork16:2:1,ParallelYieldLight64:2:1,ParallelYieldWork:4:1,ParallelYieldHeavy64:4:1 stock "${modes[@]}" > ../validation/route-cold.jsonl
python3 -B bench.py run 32 ParallelYieldWork16:4:4,ParallelYieldLight64:4:4,ParallelYieldWork:2:4,ParallelYieldHeavy64:4:4 stock "${modes[@]}" > ../validation/route-warm.jsonl
python3 -B bench.py run 28 TaskBurst100000:4:4,TaskPark100000:4:4,MixedParkScalarFirst100000:4:4,TaskSequence:2:10000 stock base managedfirst manageddeferred earlytwo earlyall > ../validation/route-guards.jsonl
