#!/usr/bin/env bash

# Copyright 2026 The Go Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

set -euo pipefail

if [[ $# -ne 1 ]]; then
	echo "usage: run_push_pull.bash OUTPUT_DIRECTORY" >&2
	exit 2
fi

goroot=$(cd "$(dirname "$0")/../../../../.." && pwd)
output_dir=$1
mkdir -p "$output_dir"
output_dir=$(cd "$output_dir" && pwd)
test_binary="$output_dir/runtime-push-pull.test"
manifest="$output_dir/manifest.txt"
samples=${SAMPLES:-10}
warmups=${WARMUP_SAMPLES:-3}
benchtime=${BENCHTIME:-500ms}
footprint_samples=${FOOTPRINT_SAMPLES:-6}

if [[ ! -x "$goroot/bin/go" ]]; then
	echo "$goroot/bin/go is not executable; build this GOROOT first" >&2
	exit 2
fi

{
	printf 'revision: '
	git -C "$goroot" rev-parse HEAD
	printf 'go: '
	GOEXPERIMENT=coro "$goroot/bin/go" version
	printf 'host: '
	uname -a
	printf 'load: '
	uptime
	printf 'samples: %s; warmups: %s; benchtime: %s; footprint samples: %s\n' \
		"$samples" "$warmups" "$benchtime" "$footprint_samples"
	if command -v sw_vers >/dev/null; then
		sw_vers
	fi
	if command -v sysctl >/dev/null; then
		sysctl -n machdep.cpu.brand_string 2>/dev/null || true
		sysctl -n vm.loadavg 2>/dev/null || true
	fi
	git -C "$goroot" status --short
	git -C "$goroot" diff --stat
} >"$manifest"

CGO_ENABLED=1 GOEXPERIMENT=coro "$goroot/bin/go" test \
	-tags=coropullcompare -c runtime \
	-o "$test_binary"

regular='^BenchmarkStacklessCoroPushPull(Entry|Yield|Await1|Await8|Await64|Await256|Await4096|Timer|FileReady|FileBlocked|SocketReady|SocketBlocked|DirectCSteady|DirectCEpisodes)$'
footprint='^BenchmarkStacklessCoroPushPullFootprint(0|4096)$'

for mode in Stackful Push Pull CompactPull OrdinaryCgo DirectNoBlock; do
	: >"$output_dir/${mode}.txt"
	: >"$output_dir/${mode}.stderr"
done

run_mode() {
	local mode=$1
	local output=${2:-"$output_dir/${mode}.txt"}
	local error=${3:-"$output_dir/${mode}.stderr"}
	GOMAXPROCS=1 "$test_binary" -test.run '^$' \
		-test.bench "$regular/^${mode}$" -test.benchtime "$benchtime" \
		-test.count 1 -test.cpu 1 -test.timeout 10m \
		2>>"$error" | sed -E "s#/${mode}(-[0-9]+)?([[:space:]])#/Model\\1\\2#" \
		>>"$output"
}

run_footprint() {
	local mode=$1
	GOMAXPROCS=1 "$test_binary" -test.run '^$' \
		-test.bench "$footprint/^${mode}$" -test.benchtime 1x \
		-test.count 1 -test.cpu 1 -test.timeout 10m \
		2>>"$output_dir/${mode}.stderr" | \
		sed -E "s#/${mode}(-[0-9]+)?([[:space:]])#/Model\\1\\2#" \
		>>"$output_dir/${mode}.txt"
}

for ((sample = 1; sample <= warmups; sample++)); do
	if ((sample % 2 == 1)); then
		run_mode Push /dev/null /dev/null
		run_mode Pull /dev/null /dev/null
	else
		run_mode Pull /dev/null /dev/null
		run_mode Push /dev/null /dev/null
	fi
done

for ((sample = 1; sample <= samples; sample++)); do
	if ((sample % 2 == 1)); then
		run_mode Push
		run_mode Pull
	else
		run_mode Pull
		run_mode Push
	fi
	run_mode Stackful
	run_mode CompactPull
	run_mode OrdinaryCgo
	run_mode DirectNoBlock
done

footprint_modes=(Stackful Push Pull CompactPull)
for ((sample = 1; sample <= footprint_samples; sample++)); do
	if ((sample % 2 == 1)); then
		for mode in "${footprint_modes[@]}"; do
			run_footprint "$mode"
		done
	else
		for ((index = ${#footprint_modes[@]} - 1; index >= 0; index--)); do
			run_footprint "${footprint_modes[index]}"
		done
	fi
done

printf 'push %s\npull %s\nstackful %s\ncompact pull %s\nmanifest %s\n' \
	"$output_dir/Push.txt" "$output_dir/Pull.txt" \
	"$output_dir/Stackful.txt" "$output_dir/CompactPull.txt" "$manifest"
