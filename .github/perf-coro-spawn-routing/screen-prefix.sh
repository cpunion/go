#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
root="$(pwd -P)"
export GOENV=off GOWORK=off GOTOOLCHAIN=local GOCACHE="$root/cache" GO111MODULE=off
export GOEXPERIMENT=coro CGO_ENABLED=1 GOMAXPROCS=8
python3 -B prefix.py
cd bench
variants=(prefix1 prefix3 prefix5 prefix3owner)
for mode in "${variants[@]}"; do
   "$root/sdk/bin/go" test -c -gcflags=-d=coro=4 -overlay="$root/overlays/$mode.json" -o "$mode.test" . > "../validation/build-$mode.log" 2>&1
done
shasum -a 256 "${variants[@]/%/.test}" > ../validation/prefix-hashes.txt
python3 -B bench.py run 42 ParallelYieldWork16:4:1,ParallelYieldLight64:4:1,ParallelYieldWork16:2:1,ParallelYieldLight64:2:1,ParallelYieldWork:4:1,ParallelYieldHeavy64:4:1 stock base waveall "${variants[@]}" > ../validation/prefix-cold.jsonl
python3 -B bench.py run 36 ParallelYieldWork16:4:4,ParallelYieldLight64:4:4,ParallelYieldHeavy64:4:4,ParallelYieldWork:2:4 stock base waveall "${variants[@]}" > ../validation/prefix-warm.jsonl
