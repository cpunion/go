#!/usr/bin/env bash
set -euo pipefail
revision=3c783203356ddda011910a5174d394607c2ff558
recipe="$(cd "$(dirname "$0")" && pwd -P)"
scratch="$RUNNER_TEMP/coro-ordinary-amd64"
out="$scratch/results"
mkdir -p "$scratch"/{bench,cache,results}
test "$(uname -s -m)" = 'Linux x86_64'
git worktree add --detach "$scratch/sdk" "$revision"
test "$(git -C "$scratch/sdk" rev-parse HEAD)" = "$revision"
export GOENV=off GOWORK=off GOTOOLCHAIN=local GOMAXPROCS=4 CGO_ENABLED=1 GOCACHE="$scratch/cache"
unset GOFLAGS GOEXPERIMENT GODEBUG
GOROOT_BOOTSTRAP="$(go env GOROOT)"
export GOROOT_BOOTSTRAP
{
    printf 'source %s\n' "$revision"
    uname -srm
    lscpu
} > "$out/machine.txt"
go version > "$out/stock-version.txt"
grep -Fx 'go version go1.27.3 linux/amd64' "$out/stock-version.txt"
(cd "$scratch/sdk/src" && GOEXPERIMENT=coro ./make.bash) > "$out/bootstrap.log" 2>&1
sdk="$scratch/sdk/bin/go"
"$sdk" version > "$out/sdk-version.txt"
grep -Fq "${revision:0:8}" "$out/sdk-version.txt"
cp -R "$scratch/sdk/src/cmd/compile/testdata/corobench/." "$scratch/bench/"
cp "$recipe"/{bench.py,bench-repeat.py,compute_test.go} "$scratch/bench/"
GOEXPERIMENT=coro "$sdk" test -count=1 -timeout=6m cmd/compile/internal/coro cmd/cgo runtime     -run 'Test(StacklessCoro|AffineReturn|AnalyzeCgo|Cgo|GoCall|Lower|Coro)' > "$out/focused.log" 2>&1
GOEXPERIMENT=coro timeout 480s bash "$scratch/sdk/src/cmd/compile/testdata/corobench/check.bash"     "$scratch/sdk" "$scratch/architecture" > "$out/architecture.log" 2>&1
sourcefile=src/runtime/coro_stackless_spawn_batch.go
git show "$revision^:$sourcefile" > "$scratch/disabled.go"
python3 - "$scratch/sdk/$sourcefile" "$scratch/disabled.go" > "$scratch/disabled.json" <<'PY'
import json
import sys
print(json.dumps({'Replace': {sys.argv[1]: sys.argv[2]}}))
PY
cd "$scratch/bench"
export GO111MODULE=off
go test -c -o stock.test . > "$out/build-stock.log" 2>&1
GOEXPERIMENT=coro "$sdk" test -c -gcflags=-d=coro=4 -o candidate.test . > "$out/build-candidate.log" 2>&1
GOEXPERIMENT=coro "$sdk" test -c -gcflags=-d=coro=4 -overlay="$scratch/disabled.json" -o disabled.test . > "$out/build-disabled.log" 2>&1
"$sdk" tool objdump -s '\.parallelYieldWork$' candidate.test > "$out/root.asm"
"$sdk" tool objdump -s '\.parallelYieldWorker\.coro\.batch$' candidate.test > "$out/worker.asm"
grep 'CALL.*coroOrdinarySpawnBatch' "$out/root.asm" > "$out/route.log"
grep 'CALL.*coroOrdinaryBatchYield' "$out/worker.asm" >> "$out/route.log"
for procs in 2 4 8; do
    for mode in candidate disabled; do
        GODEBUG=asyncpreemptoff=1 GOMAXPROCS="$procs" "./$mode.test" -test.v -test.timeout=2m -test.count=8 \
            -test.run '^TestParallel(YieldBatchBounds|SpawnProgress)$' >> "$out/$mode-progress.log" 2>&1
    done
done
modes=(stock disabled candidate)
sha256sum "${modes[@]/%/.test}" > "$out/hashes.txt"
python3 -B bench-repeat.py run 120 ParallelYieldWork16:2:1,ParallelYieldLight64:2:1,ParallelYieldWork16:4:1,ParallelYieldLight64:4:1,ParallelYieldHeavy64:4:1 "${modes[@]}" > "$out/cold.jsonl"
python3 -B bench.py run 70 ParallelYieldWork16:2:64,ParallelYieldLight64:2:64,ParallelYieldWork16:4:64,ParallelYieldLight64:4:64,ParallelYieldHeavy64:4:64 "${modes[@]}" > "$out/steady.jsonl"
python3 -B bench-repeat.py run 24 TaskBurst100000:4:4,TaskPark100000:4:4,TaskBurst1000:4:20,TaskSequence:2:30000,TaskSequence:8:30000,DeferYield:2:10000,RecoverYield:2:10000 "${modes[@]}" > "$out/guards.jsonl"
gzip "$out"/{cold,steady,guards}.jsonl
