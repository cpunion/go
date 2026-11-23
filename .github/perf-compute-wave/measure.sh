#!/usr/bin/env bash
set -euo pipefail
base="$1"
candidate="$2"
scripts="$(cd "$(dirname "$0")" && pwd -P)"
sandbox="$RUNNER_TEMP/coro-compute-wave"
mkdir -p "$sandbox"/{bench,cache,validation}
test "$(uname -s -m)" = 'Linux x86_64'
for mode in base candidate; do
    revision="${!mode}"
    if ! git cat-file -e "$revision^{commit}"; then
        git fetch --depth=1 --no-tags origin "$revision"
    fi
    git worktree add --detach "$sandbox/$mode" "$revision"
    test "$(git -C "$sandbox/$mode" rev-parse HEAD)" = "$revision"
done
export GOENV=off GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=1 GOCACHE="$sandbox/cache" GOMAXPROCS=8
unset GOEXPERIMENT GOFLAGS
GOROOT_BOOTSTRAP="$(go env GOROOT)"
export GOROOT_BOOTSTRAP
cd "$sandbox"
{
    printf 'base %s\ncandidate %s\n' "$base" "$candidate"
    uname -srm
    lscpu
} > validation/machine.txt
go version > validation/stock-version.txt
grep -Fx 'go version go1.27.3 linux/amd64' validation/stock-version.txt
for mode in base candidate; do
    revision="${!mode}"
    (cd "$mode/src" && GOEXPERIMENT=coro ./make.bash) > "validation/$mode-bootstrap.log" 2>&1
    "$mode/bin/go" version > "validation/$mode-version.txt"
    grep -Fq "${revision:0:8}" "validation/$mode-version.txt"
done
cp -R candidate/src/cmd/compile/testdata/corobench/. bench/
cp "$scripts"/{bench.py,bench-repeat.py,compute_test.go} bench/
cd bench
export GO111MODULE=off
go test -c -o stock.test . > ../validation/build-stock.log 2>&1
for mode in base candidate; do
    GOEXPERIMENT=coro "../$mode/bin/go" test -c -gcflags=-d=coro=4 -o "$mode.test" . > "../validation/build-$mode.log" 2>&1
    "../$mode/bin/go" tool objdump -s '\.parallelYieldWork\.coro\.func1$' "$mode.test" |
        grep 'CALL.*coroSpawnScalarBatch' > "../validation/$mode-spawn.asm"
    GODEBUG=asyncpreemptoff=1 GOMAXPROCS=4 "./$mode.test" -test.v -test.timeout=3m -test.count=10 \
        -test.run '^TestParallelSpawnProgress$' > "../validation/$mode-progress.log" 2>&1
done
shasum -a 256 stock.test base.test candidate.test > ../validation/binary-hashes.txt
modes=(stock base candidate)
python3 -B bench-repeat.py run 128 ParallelYieldWork16:4:1,ParallelYieldLight64:4:1,ParallelYieldWork16:2:1,ParallelYieldLight64:2:1 "${modes[@]}" > ../validation/exact-cold.jsonl
python3 -B bench-repeat.py run 48 ParallelYieldWork16:4:4,ParallelYieldLight64:4:4,ParallelYieldHeavy64:4:4 "${modes[@]}" > ../validation/exact-warm.jsonl
python3 -B bench-repeat.py run 36 TaskBurst100000:4:4,TaskPark100000:4:4,MixedParkScalarFirst100000:4:4 "${modes[@]}" > ../validation/exact-guards.jsonl
