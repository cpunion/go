#!/usr/bin/env bash

# Copyright 2026 The Go Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

set -euo pipefail

if [[ $# -ne 3 ]]; then
	echo "usage: run.bash BASELINE_GOROOT CORO_GOROOT OUTPUT_DIRECTORY" >&2
	exit 2
fi

baseline_goroot=$(cd "$1" && pwd)
coro_goroot=$(cd "$2" && pwd)
output_dir=$3

for goroot in "$baseline_goroot" "$coro_goroot"; do
	if [[ ! -x "$goroot/bin/go" ]]; then
		echo "$goroot/bin/go is not executable" >&2
		exit 2
	fi
done

baseline_revision=$(git -C "$baseline_goroot" rev-parse HEAD)
coro_revision=$(git -C "$coro_goroot" rev-parse HEAD)
upstream_ref=${UPSTREAM_REF:-origin/master}
baseline_version=$("$baseline_goroot/bin/go" version)
coro_version=$("$coro_goroot/bin/go" version)
if [[ "$baseline_version" != *"_${baseline_revision:0:10} "* ]]; then
	echo "baseline binary does not match $baseline_revision: $baseline_version" >&2
	exit 2
fi
if [[ "$coro_version" != *"_${coro_revision:0:10} "* ]]; then
	echo "coroutine binary does not match $coro_revision: $coro_version" >&2
	exit 2
fi
if ! upstream_revision=$(git -C "$coro_goroot" rev-parse --verify \
	"$upstream_ref^{commit}"); then
	echo "cannot resolve upstream reference $upstream_ref" >&2
	exit 2
fi
merge_base=$(git -C "$coro_goroot" merge-base \
	"$upstream_revision" "$coro_revision")
if [[ "$merge_base" != "$baseline_revision" ]]; then
	echo "baseline $baseline_revision is not the exact merge-base $merge_base of $coro_revision and $upstream_ref" >&2
	exit 2
fi

mkdir -p "$output_dir"
output_dir=$(cd "$output_dir" && pwd)
baseline_output="$output_dir/baseline.txt"
coro_output="$output_dir/coro.txt"
baseline_error="$output_dir/baseline.stderr"
coro_error="$output_dir/coro.stderr"
baseline_capability="$output_dir/baseline-capability.txt"
coro_capability="$output_dir/coro-capability.txt"
baseline_capability_error="$output_dir/baseline-capability.stderr"
coro_capability_error="$output_dir/coro-capability.stderr"
: >"$baseline_output"
: >"$coro_output"
: >"$baseline_error"
: >"$coro_error"
: >"$baseline_capability"
: >"$coro_capability"
: >"$baseline_capability_error"
: >"$coro_capability_error"

export GO111MODULE=off

"$baseline_goroot/bin/go" test -count=1 -cover
./check.bash "$coro_goroot" "$output_dir"
"$baseline_goroot/bin/go" test -run '^TestCBlockingGroupCapacity$' \
	-count=1 -v -gcflags=-l >"$baseline_capability" \
	2>"$baseline_capability_error"
"$output_dir/coro.test" -test.run '^TestCBlockingGroupCapacity$' \
	-test.count=1 -test.v >"$coro_capability" \
	2>"$coro_capability_error"

benchmarks='^Benchmark(YieldBatch|YieldEntry|RecursiveYield64|RecursiveYield4096|MutualYield64|MutualYield4096|DeferYield|RecoverYield|TaskSequence|TaskBurst100|TaskPark100|ChannelRoundTrip|ReadyChannelPair|ReadySelect|BlockedSelect|SleepZero|SleepNanosecond|FileRead|TCPRead|FileBlockingProgress|TCPBlockingProgress|CScalar|CAggregate|CErrno|CLibm|CBlockingHandoff|CBlockingGroup3|CBlockingGroup8)$'
samples=${SAMPLES:-10}
benchtime=${BENCHTIME:-500ms}

run_baseline() {
	if ! "$baseline_goroot/bin/go" test -run '^$' -bench "$benchmarks" \
		-benchtime="$benchtime" -count=1 -cpu=1 -timeout=10m \
		-gcflags=-l >>"$baseline_output" 2>>"$baseline_error"; then
		tail -n 100 "$baseline_error" >&2
		return 1
	fi
}

run_coro() {
	if ! GOEXPERIMENT=coro "$coro_goroot/bin/go" test -run '^$' \
		-bench "$benchmarks" -benchtime="$benchtime" -count=1 -cpu=1 \
		-timeout=10m -gcflags='-l -d=coro=4' \
		>>"$coro_output" 2>>"$coro_error"; then
		tail -n 100 "$coro_error" >&2
		return 1
	fi
}

warmup_samples=${WARMUP_SAMPLES:-3}
for ((sample = 1; sample <= warmup_samples; sample++)); do
	run_baseline
	run_coro
done
: >"$baseline_output"
: >"$coro_output"
: >"$baseline_error"
: >"$coro_error"

for ((sample = 1; sample <= samples; sample++)); do
	if ((sample % 2 == 1)); then
		run_baseline
		run_coro
	else
		run_coro
		run_baseline
	fi
done

scaling_benchmarks='^BenchmarkParallelYieldWork$'
scaling_samples=${SCALING_SAMPLES:-$samples}
scaling_benchtime=${SCALING_BENCHTIME:-$benchtime}

run_baseline_scaling() {
	local output=${1:-$baseline_output}
	if ! "$baseline_goroot/bin/go" test -run '^$' \
		-bench "$scaling_benchmarks" -benchtime="$scaling_benchtime" \
		-count=1 -cpu=1,2,4 -timeout=10m -gcflags=-l \
		>>"$output" 2>>"$baseline_error"; then
		tail -n 100 "$baseline_error" >&2
		return 1
	fi
}

run_coro_scaling() {
	local output=${1:-$coro_output}
	if ! GOEXPERIMENT=coro "$coro_goroot/bin/go" test -run '^$' \
		-bench "$scaling_benchmarks" -benchtime="$scaling_benchtime" \
		-count=1 -cpu=1,2,4 -timeout=10m \
		-gcflags='-l -d=coro=4' >>"$output" 2>>"$coro_error"; then
		tail -n 100 "$coro_error" >&2
		return 1
	fi
}

scaling_warmups=${SCALING_WARMUP_SAMPLES:-$warmup_samples}
for ((sample = 1; sample <= scaling_warmups; sample++)); do
	run_baseline_scaling /dev/null
	run_coro_scaling /dev/null
done
for ((sample = 1; sample <= scaling_samples; sample++)); do
	if ((sample % 2 == 1)); then
		run_baseline_scaling
		run_coro_scaling
	else
		run_coro_scaling
		run_baseline_scaling
	fi
done

footprint_samples=${FOOTPRINT_SAMPLES:-6}
footprint='^BenchmarkTaskParkFootprint10000$'
for ((sample = 1; sample <= footprint_samples; sample++)); do
	if ((sample % 2 == 1)); then
		"$baseline_goroot/bin/go" test -run '^$' -bench "$footprint" \
			-benchtime=1x -count=1 -cpu=1 -gcflags=-l \
			>>"$baseline_output" 2>>"$baseline_error"
		GOEXPERIMENT=coro "$coro_goroot/bin/go" test -run '^$' \
			-bench "$footprint" -benchtime=1x -count=1 -cpu=1 \
			-gcflags='-l -d=coro=4' >>"$coro_output" 2>>"$coro_error"
	else
		GOEXPERIMENT=coro "$coro_goroot/bin/go" test -run '^$' \
			-bench "$footprint" -benchtime=1x -count=1 -cpu=1 \
			-gcflags='-l -d=coro=4' >>"$coro_output" 2>>"$coro_error"
		"$baseline_goroot/bin/go" test -run '^$' -bench "$footprint" \
			-benchtime=1x -count=1 -cpu=1 -gcflags=-l \
			>>"$baseline_output" 2>>"$baseline_error"
	fi
done

printf 'baseline %s\ncoroutine %s\nresults %s\n' \
	"$baseline_revision" "$coro_revision" "$output_dir"
