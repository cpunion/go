#!/usr/bin/env bash
set -euo pipefail
revision=1a5cdb0acf67781f5043cd3a3d40886044d3d5cc
recipe="$(cd "$(dirname "$0")" && pwd -P)"
scratch="$RUNNER_TEMP/coro-native-idle"
out="$scratch/results"
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
cp "$recipe"/{bench.py,bench-repeat.py,compute_test.go} bench/
python3 -B "$recipe/measured-idle-p.py" "$scratch/sdk" "$scratch/overlays"
cd bench
export GO111MODULE=off
go test -c -o stock.test . > "$out/build-stock.log" 2>&1
GOEXPERIMENT=coro "$sdk" test -c -gcflags=-d=coro=4 -o base.test . > "$out/build-base.log" 2>&1
modes=(stock base localwake reusem startm)
for mode in "${modes[@]:2}"; do
    GOEXPERIMENT=coro "$sdk" test -c -gcflags=-d=coro=4 -overlay="$scratch/overlays/$mode.json" \
        -o "$mode.test" . > "$out/build-$mode.log" 2>&1
    GODEBUG=asyncpreemptoff=1 GOMAXPROCS=4 "./$mode.test" -test.v -test.timeout=2m -test.count=12 \
        -test.run '^TestParallelSpawnProgress$' > "$out/$mode-progress.log" 2>&1
    GODEBUG=asyncpreemptoff=1 GOMAXPROCS=4 "./$mode.test" -test.v -test.timeout=2m -test.count=12 \
        -test.run '^TestPublicWriteBlockingProgress$/^file$' >> "$out/$mode-progress.log" 2>&1
done
sha256sum "${modes[@]/%/.test}" > "$out/hashes.txt"
python3 -B bench.py run 160 ParallelYieldWork16:4:1,ParallelYieldLight64:4:1,ParallelYieldWork16:2:1,ParallelYieldLight64:2:1 "${modes[@]}" > "$out/native-cold.jsonl"
python3 -B bench-repeat.py run 56 ParallelYieldWork16:4:4,ParallelYieldLight64:4:4,ParallelYieldHeavy64:4:4,YieldEntry:2:10000 "${modes[@]}" > "$out/native-warm.jsonl"
python3 -B bench.py run 24 TaskBurst100000:4:4,TaskPark100000:4:4,MixedParkScalarFirst100000:4:4 "${modes[@]}" > "$out/native-guards.jsonl"
gzip "$out"/native-{cold,warm,guards}.jsonl
for mode in proofreusem proofstartm; do
    GOEXPERIMENT=coro "$sdk" test -c -gcflags=-d=coro=4 -overlay="$scratch/overlays/$mode.json" \
        -o "$mode.test" . > "$out/build-$mode.log" 2>&1
    for i in $(seq 1 48); do
        GOMAXPROCS=4 "./$mode.test" -test.run=^$ -test.bench='^BenchmarkParallelYieldWork16$' \
            -test.benchtime=1x -test.timeout=8s >> "$out/$mode.log" 2>&1
    done
done
