#!/usr/bin/env bash
set -euo pipefail
revision=1a5cdb0acf67781f5043cd3a3d40886044d3d5cc
recipe="$(cd "$(dirname "$0")" && pwd -P)"
scratch="$RUNNER_TEMP/coro-early-executors"
out="$scratch/results"
mkdir -p "$scratch"/{bench,cache,overlays,results}
test "$(uname -s -m)" = 'Linux x86_64'
if ! git cat-file -e "$revision^{commit}"; then
    git fetch --depth=1 --no-tags origin "$revision"
fi
git worktree add --detach "$scratch/sdk" "$revision"
test "$(git -C "$scratch/sdk" rev-parse HEAD)" = "$revision"

export GOENV=off GOWORK=off GOTOOLCHAIN=local GOMAXPROCS=8 CGO_ENABLED=1 GOCACHE="$scratch/cache"
unset GOFLAGS GOEXPERIMENT GODEBUG
GOROOT_BOOTSTRAP="$(go env GOROOT)"
export GOROOT_BOOTSTRAP
cd "$scratch"
{
    printf 'source %s\n' "$revision"
    uname -srm
    lscpu
} > "$out/machine.txt"
go version > "$out/stock-version.txt"
grep -Fx 'go version go1.27.3 linux/amd64' "$out/stock-version.txt"
(cd sdk/src && GOEXPERIMENT=coro ./make.bash) > "$out/bootstrap.log" 2>&1
sdk="$scratch/sdk/bin/go"
"$sdk" version > "$out/sdk-version.txt"
grep -Fq "${revision:0:8}" "$out/sdk-version.txt"

cp -R sdk/src/cmd/compile/testdata/corobench/. bench/
cp "$recipe"/{bench.py,bench-repeat.py,compute_test.go,cold_dispatch_trace_test.go} bench/
python3 -B "$recipe/early-workers.py" "$scratch/sdk" "$scratch/overlays"
cd bench
export GO111MODULE=off
go test -c -o stock.test . > "$out/build-stock.log" 2>&1
GOEXPERIMENT=coro "$sdk" test -c -gcflags=-d=coro=4 -o base.test . > "$out/build-base.log" 2>&1
modes=(stock base early2 early3 early4 after3)
for mode in "${modes[@]:2}"; do
    GOEXPERIMENT=coro "$sdk" test -c -gcflags=-d=coro=4 -overlay="$scratch/overlays/$mode.json" \
        -o "$mode.test" . > "$out/build-$mode.log" 2>&1
    GODEBUG=asyncpreemptoff=1 GOMAXPROCS=4 "./$mode.test" -test.v -test.timeout=3m -test.count=10 \
        -test.run '^TestParallelSpawnProgress$' > "$out/$mode-progress.log" 2>&1
    GODEBUG=asyncpreemptoff=1 GOMAXPROCS=4 "./$mode.test" -test.v -test.timeout=3m -test.count=10 \
        -test.run '^TestPublicWriteBlockingProgress$/^file$' >> "$out/$mode-progress.log" 2>&1
done
sha256sum "${modes[@]/%/.test}" > "$out/hashes.txt"
python3 -B bench.py run 152 ParallelYieldWork16:4:1,ParallelYieldLight64:4:1,ParallelYieldWork16:2:1,ParallelYieldLight64:2:1 "${modes[@]}" > "$out/amd64-cold.jsonl"
python3 -B bench-repeat.py run 44 ParallelYieldWork16:4:4,ParallelYieldLight64:4:4,ParallelYieldHeavy64:4:4,YieldEntry:2:10000 "${modes[@]}" > "$out/amd64-warm.jsonl"
python3 -B bench.py run 28 TaskBurst100000:4:4,TaskPark100000:4:4,MixedParkScalarFirst100000:4:4,DeferYield:2:10000,TaskSequence:2:10000 "${modes[@]}" > "$out/amd64-guards.jsonl"
gzip "$out"/amd64-{cold,warm,guards}.jsonl

for mode in base early3 after3; do
    layer=()
    if test "$mode" != base; then
        layer+=("$scratch/overlays/$mode.json")
    fi
    trace="$scratch/overlays/trace-$mode"
    python3 -B "$recipe/trace-cold-dispatch.py" "$scratch/sdk" "$trace" "${layer[@]}"
    GOEXPERIMENT=coro "$sdk" test -c -tags=corocoldtrace -gcflags=-d=coro=4 -overlay="$trace/trace.json" \
        -o "$mode-trace.test" . > "$out/build-$mode-trace.log" 2>&1
    "$sdk" tool objdump -s '\.parallelYieldWork\.coro\.func1$' "$mode-trace.test" |
        awk '/CALL.*coroSpawnScalarBatch/ { print; found=1 } END { if (!found) exit 1 }' > "$out/$mode-route.asm"
    python3 -B "$recipe/measure-cold-dispatch.py" "$mode-trace.test" "$out/$mode" 36
    if test "$mode" != base; then
        python3 -B -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d["ParallelYieldWork16/P4"]["entry_to_early_worker"]["samples"] > 0' \
            "$out/$mode/cold-dispatch-summary.json"
    fi
    gzip "$out/$mode/cold-dispatch.jsonl"
done
