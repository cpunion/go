#!/usr/bin/env bash
set -euo pipefail
revision=1a5cdb0acf67781f5043cd3a3d40886044d3d5cc
recipe="$(cd "$(dirname "$0")" && pwd -P)"
scratch="$RUNNER_TEMP/coro-cold-dispatch"
mkdir -p "$scratch"/{bench,cache,overlays,results}
test "$(uname -s -m)" = 'Linux x86_64'
git worktree add --detach "$scratch/sdk" "$revision"
test "$(git -C "$scratch/sdk" rev-parse HEAD)" = "$revision"

export GOENV=off GOWORK=off GOTOOLCHAIN=local GOMAXPROCS=8 CGO_ENABLED=1 GOCACHE="$scratch/cache"
unset GOFLAGS GOEXPERIMENT GODEBUG
GOROOT_BOOTSTRAP="$(go env GOROOT)"
export GOROOT_BOOTSTRAP
cd "$scratch"
{
    printf 'candidate %s\n' "$revision"
    uname -srm
    lscpu
} > results/machine.txt
go version > results/stock-version.txt
grep -Fx 'go version go1.27.3 linux/amd64' results/stock-version.txt
(cd sdk/src && GOEXPERIMENT=coro ./make.bash) > results/bootstrap.log 2>&1
sdk="$scratch/sdk/bin/go"
"$sdk" version > results/candidate-version.txt
grep -Fq "${revision:0:8}" results/candidate-version.txt

cp -R sdk/src/cmd/compile/testdata/corobench/. bench/
cp "$recipe"/{bench.py,compute_test.go,cold_dispatch_trace_test.go} bench/
python3 -B "$recipe/trace-cold-dispatch.py" "$scratch/sdk" "$scratch/overlays"
cd bench
export GO111MODULE=off
go test -c -o stock.test . > ../results/build-stock.log 2>&1
GOEXPERIMENT=coro "$sdk" test -c -gcflags=-d=coro=4 -o base.test . > ../results/build-base.log 2>&1
GOEXPERIMENT=coro "$sdk" test -c -tags=corocoldtrace -gcflags=-d=coro=4 \
    -overlay="$scratch/overlays/trace.json" -o cold-dispatch.test . > ../results/build-trace.log 2>&1
"$sdk" tool objdump -s '\.parallelYieldWork\.coro\.func1$' cold-dispatch.test |
    grep 'CALL.*coroSpawnScalarBatch' > ../results/trace-spawn.asm
sha256sum stock.test base.test cold-dispatch.test > ../results/hashes.txt
GODEBUG=asyncpreemptoff=1 GOMAXPROCS=4 ./base.test -test.v -test.timeout=3m -test.count=12 \
    -test.run '^Test(ParallelSpawnProgress|PublicWriteBlockingProgress)$' > ../results/progress.log 2>&1
python3 -B bench.py run 128 ParallelYieldWork16:4:1,ParallelYieldLight64:4:1,ParallelYieldWork16:2:1,ParallelYieldLight64:2:1 stock base > ../results/cold.jsonl
python3 -B "$recipe/measure-cold-dispatch.py" cold-dispatch.test ../results 64
gzip ../results/{cold,cold-dispatch}.jsonl
