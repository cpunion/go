#!/usr/bin/env bash
set -euo pipefail
base="$1"
tip="$2"
build="$RUNNER_TEMP/toolchains"
out="$RUNNER_TEMP/coro-spawn-batching"
script="$(cd "$(dirname "$0")" && pwd)"
stock="$(go env GOROOT)"
export GOENV=off GOTOOLCHAIN=local GOWORK=off CGO_ENABLED=1 GOMAXPROCS=4
unset GOFLAGS GOEXPERIMENT
mkdir -p "$build" "$out" "$build/bench"
{
  printf 'baseline %s\ncandidate %s\n' "$base" "$tip"
  "$stock/bin/go" version
  uname -srm
  lscpu
} > "$out/machine.txt"
git worktree add --detach "$build/base" "$base"
git worktree add --detach "$build/candidate" "$tip"
for mode in base candidate; do
  sdk="$build/$mode"
  (
    cd "$sdk/src"
    GOROOT_BOOTSTRAP="$stock" GOEXPERIMENT=coro ./make.bash
  ) > "$out/$mode-bootstrap.log" 2>&1
  "$sdk/bin/go" version >> "$out/toolchains.txt"
done
base_short="$(git -C "$build/base" rev-parse --short=8 HEAD)"
tip_short="$(git -C "$build/candidate" rev-parse --short=8 HEAD)"
grep -q "$base_short" "$out/toolchains.txt"
grep -q "$tip_short" "$out/toolchains.txt"
(
  cd "$build/candidate/src"
  GOEXPERIMENT=coro,cgocheck2,staticlockranking GOGC=1 ../bin/go test -p=2 -count=3 -timeout=15m \
    -gcflags=all=-d=checkptr=2 runtime \
    -run '^TestStacklessCoro(ParallelSpawn|ExecutorJoinGrowth|NativeReplacementDrain|SinglePExecutors|NativeChannel(PopulationReuse|CacheLifecycle)|ClosePublication|ChannelWaiterCache|DrainedCacheOperations|Compact(ChannelCloseMany|SelectClose))$'
) > "$out/runtime-checkptr.txt" 2>&1
(
  cd "$build/candidate/src"
  GOEXPERIMENT=coro ../bin/go test -p=2 -count=1 -timeout=15m cmd/compile/internal/coro \
    -run '^Test(SpawnReadyPressure|TailFrameSpawn|TerminalSourceCallersSpawn)$'
) > "$out/compiler-spawn.txt" 2>&1
cp -R "$build/candidate/src/cmd/compile/testdata/corobench/." "$build/bench/"
(
  cd "$build/bench"
  export GO111MODULE=off
  GOEXPERIMENT= "$stock/bin/go" test -c -p=4 -o stock.test .
  for mode in base candidate; do
    GOEXPERIMENT=coro "$build/$mode/bin/go" test -c -p=4 -gcflags=-d=coro=4 -o "$mode.test" .
  done
) > "$out/task-build.log" 2>&1
cd "$build/bench"
"$stock/bin/go" version -m stock.test base.test candidate.test > "$out/task-versions.txt"
uptime > "$out/load.txt"
python3 -B "$script/bench.py" run 24 AlternatingTyped100000:4:4,AlternatingTyped100000:1:4,MixedParkScalarFirst100000:4:4,MixedParkPointerFirst100000:4:4,TaskPark100000:4:4,TaskBurst100000:4:4,ParallelYieldWork:4:4,ParallelYieldWork:3:4 stock base candidate > "$out/tasks-warm.jsonl"
python3 -B "$script/bench.py" run 24 AlternatingTyped100000:4:1,MixedParkScalarFirst100000:4:1,MixedParkPointerFirst100000:4:1 stock base candidate > "$out/tasks-cold.jsonl"

cp -R "$script/blocking" "$build/channel"
cd "$build/channel"
(
  GOEXPERIMENT= "$stock/bin/go" test -c -p=4 -o stock.test .
  for mode in base candidate; do
    GOEXPERIMENT=coro "$build/$mode/bin/go" test -c -p=4 -gcflags=exchangestress/pair=-d=coro=4 -o "$mode.test" .
  done
) > "$out/channel-build.log" 2>&1
"$stock/bin/go" version -m stock.test base.test candidate.test > "$out/channel-versions.txt"
"$build/candidate/bin/go" tool objdump -s '^exchangestress/pair\.CommaOK$' candidate.test > "$out/channel-entry.asm"
grep -q 'runtime.coroTryChanExchange' "$out/channel-entry.asm"
python3 -B "$script/bench.py" run 24 PublicUnbufferedExchange:1:80000,PublicUnbufferedExchange:4:80000 stock base candidate > "$out/channel.jsonl"
