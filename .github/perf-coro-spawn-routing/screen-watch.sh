#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
export GOENV=off GOWORK=off GOTOOLCHAIN=local GOCACHE="$PWD/cache" GO111MODULE=off CGO_ENABLED=1 GOMAXPROCS=8
root="$(pwd -P)"
sdk="$root/sdk/bin/go"
python3 -B watch.py
cd bench
for mode in quietwatch quietnet clockless inlinewatch watchproof; do
    GOEXPERIMENT=coro "$sdk" test -c -gcflags=-d=coro=4 -overlay="$root/overlays/$mode.json" -o "$mode.test" . > "../validation/build-$mode.log" 2>&1
done
shasum -a 256 quietwatch.test quietnet.test clockless.test inlinewatch.test watchproof.test > ../validation/watch-hashes.txt
for procs in 2 4; do
  for ((i=0; i<32; i++)); do
    GOMAXPROCS=$procs ./watchproof.test -test.run '^$' -test.bench '^BenchmarkParallelYieldWork16$' -test.benchtime=1x
  done
done > ../validation/watch-proof.txt 2>&1
modes=(stock base quietwatch quietnet clockless inlinewatch)
python3 -B bench.py run 40 ParallelYieldWork16:4:1,ParallelYieldLight64:4:1,ParallelYieldWork16:2:1,ParallelYieldLight64:2:1,ParallelYieldWork:4:1,ParallelYieldHeavy64:4:1 "${modes[@]}" > ../validation/watch-cold.jsonl
python3 -B bench.py run 32 ParallelYieldWork16:4:4,ParallelYieldLight64:4:4,ParallelYieldWork:2:4,ParallelYieldHeavy64:4:4 "${modes[@]}" > ../validation/watch-warm.jsonl
python3 -B bench.py run 24 TaskBurst100000:4:4,TaskPark100000:4:4,MixedParkScalarFirst100000:4:4,TaskSequence:2:10000 stock base inlinewatch > ../validation/watch-guards.jsonl
