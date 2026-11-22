#!/usr/bin/env bash
set -euo pipefail
base="$1"
wide="$2"
script="$(cd "$(dirname "$0")" && pwd)"
sdk="$RUNNER_TEMP/park-sdk"
bench="$RUNNER_TEMP/park-bench"
out="$RUNNER_TEMP/park-result"
stock="$(go env GOROOT)"
export GOENV=off GOTOOLCHAIN=local GOWORK=off CGO_ENABLED=1 GOMAXPROCS=4
unset GOFLAGS GOEXPERIMENT GODEBUG
mkdir -p "$bench" "$out"
{
  printf 'baseline %s\nwide source %s\n' "$base" "$wide"
  "$stock/bin/go" version
  lscpu
  taskset -pc $$
  cat /sys/fs/cgroup/cpu.max 2>/dev/null || true
} > "$out/machine.txt"
git worktree add --detach "$sdk" "$base"
test "$(git -C "$sdk" rev-parse "$wide^")" = "$base"
(
  cd "$sdk/src"
  GOEXPERIMENT=coro GOROOT_BOOTSTRAP="$stock" ./make.bash
) > "$out/bootstrap.log" 2>&1
"$sdk/bin/go" version | tee "$out/toolchain.txt"
cp -R "$sdk/src/cmd/compile/testdata/corobench/." "$bench"
cd "$bench"
export GO111MODULE=off
{
  "$stock/bin/go" test -c -p=4 -o stock.test .
  GOEXPERIMENT=coro "$sdk/bin/go" test -c -p=4 -gcflags=-d=coro=4 -o base.test .
  for mode in close limit wide wide_limit; do
    overlay="$bench/overlays/$mode.json"
    mkdir -p "$bench/overlays"
    python3 -B "$script/overlays.py" "$sdk" "$bench/overlays/$mode" "$mode" "$wide" > "$overlay"
    GOEXPERIMENT=coro "$sdk/bin/go" test -c -p=4 -overlay "$overlay" -gcflags=-d=coro=4 -o "$mode.test" .
  done
} > "$out/build.log" 2>&1
"$stock/bin/go" version -m ./*.test > "$out/versions.txt"
uptime > "$out/load.txt"
python3 -B "$script/bench.py" run 24 MixedParkScalarFirst100000:4:4,AlternatingTyped100000:4:4,TaskPark100000:4:4,TaskBurst100000:4:4 stock base limit > "$out/limit.jsonl"
python3 -B "$script/bench.py" run 24 MixedParkScalarFirst100000:4:4,AlternatingTyped100000:4:4 stock wide wide_limit > "$out/wide.jsonl"
python3 -B "$script/bench.py" run 24 MixedParkScalarFirst100000:4:4,TaskPark100000:4:4 base close > "$out/close.jsonl"
python3 -B "$script/bench.py" run 18 MixedParkScalarFirst100000:2:4,AlternatingTyped100000:2:4 stock base limit > "$out/p2.jsonl"
for mode in stock base limit wide wide_limit; do
  "./$mode.test" -test.run='^$' -test.bench='^BenchmarkMixedParkScalarFirst100000$' \
    -test.benchtime=24x -test.cpuprofile="$bench/$mode.cpu" > "$out/$mode-cpu.txt"
  "$stock/bin/go" tool pprof -top -nodecount=60 "./$mode.test" "$bench/$mode.cpu" >> "$out/$mode-cpu.txt"
  "./$mode.test" -test.run='^$' -test.bench='^BenchmarkMixedParkScalarFirst100000$' \
    -test.benchtime=12x -test.mutexprofile="$bench/$mode.mutex" -test.mutexprofilefraction=1 > "$out/$mode-mutex.txt"
  for sample in delay contentions; do
    "$stock/bin/go" tool pprof -top -sample_index="$sample" -nodecount=60 "./$mode.test" "$bench/$mode.mutex" >> "$out/$mode-mutex.txt"
  done
done
