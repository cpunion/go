#!/usr/bin/env bash
set -euo pipefail
base="$1"
candidate="$2"
files="$(cd "$(dirname "$0")" && pwd)"
root="$RUNNER_TEMP/coro-manager-handoff"
out="$root/results"
mkdir -p "$root/bench" "$root/cache" "$out"
git worktree add --detach "$root/base" "$base"
git worktree add --detach "$root/candidate" "$candidate"
export GOENV=off GOTOOLCHAIN=local GOWORK=off CGO_ENABLED=1 GOMAXPROCS=4
export GOCACHE="$root/cache"
unset GOFLAGS GOEXPERIMENT
export GOROOT_BOOTSTRAP="$(go env GOROOT)"
{
    printf 'baseline %s\ncandidate %s\n' "$base" "$candidate"
    "$GOROOT_BOOTSTRAP/bin/go" version
    uname -srm
    lscpu
} > "$out/machine.txt"
for mode in base candidate; do
    (cd "$root/$mode/src" && GOEXPERIMENT=coro ./make.bash) > "$out/$mode-bootstrap.log" 2>&1
    "$root/$mode/bin/go" version >> "$out/sdk-versions.txt"
done
grep -q "${base:0:8}" "$out/sdk-versions.txt"
grep -q "${candidate:0:8}" "$out/sdk-versions.txt"
(
    cd "$root/candidate/src"
    GOEXPERIMENT=coro,cgocheck2,staticlockranking GOGC=1 ../bin/go test -p=4 -cpu=2,4 -count=10 -timeout=12m -gcflags=all=-d=checkptr=2 runtime \
        -run '^TestStacklessCoro(BlockingProgress|NativeBlocking(ReturnProgress|Capacity)|NativeReplacementDrain|ParallelSpawn|ExecutorJoinGrowth|SinglePExecutors|NativeChannelCacheLifecycle)$'
) > "$out/runtime-checkptr.txt" 2>&1
cp -R "$root/candidate/src/cmd/compile/testdata/corobench/." "$root/bench/"
cp "$files/compute_test.go" "$root/bench/"
cd "$root/bench"
(
    export GO111MODULE=off
    "$GOROOT_BOOTSTRAP/bin/go" test -c -o stock.test .
    for mode in base candidate; do
        GOEXPERIMENT=coro "$root/$mode/bin/go" test -c -gcflags=-d=coro=4 -o "$mode.test" .
    done
) > "$out/build.log" 2>&1
"$GOROOT_BOOTSTRAP/bin/go" version -m base.test candidate.test stock.test > "$out/bench-versions.txt"
python3 -B "$files/bench.py" run 36 ParallelYieldWork16:4:1,ParallelYieldWork16:2:1,ParallelYieldLight64:2:1,ParallelYieldLight64:4:1,ParallelYieldWork:2:1,ParallelYieldWork:4:1 stock base candidate > "$out/cold.jsonl"
python3 -B "$files/bench.py" run 36 ParallelYieldWork16:4:4,ParallelYieldLight64:2:4,ParallelYieldLight64:4:4,ParallelYieldWork:2:4,ParallelYieldWork:4:4 stock base candidate > "$out/warm.jsonl"
python3 -B "$files/bench.py" run 28 TaskBurst100000:2:4,TaskBurst100000:4:4,TaskPark100000:4:4,MixedParkScalarFirst100000:4:4,TaskSequence:2:10000 stock base candidate > "$out/guards.jsonl"
