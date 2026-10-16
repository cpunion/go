#!/usr/bin/env bash

# Copyright 2026 The Go Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

set -euo pipefail

if [[ $# -ne 3 ]]; then
	echo "usage: collect.bash BASELINE_GOROOT CORO_GOROOT NEW_OUTPUT_DIRECTORY" >&2
	exit 2
fi

baseline=$(cd "$1" && pwd)
coro=$(cd "$2" && pwd)
source_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source_root=$(git -C "$source_dir" rev-parse --show-toplevel)
output_parent=$(cd "$(dirname "$3")" && pwd)
output="$output_parent/$(basename "$3")"
if [[ -e "$output" ]]; then
	echo "output already exists: $output" >&2
	exit 2
fi
for root in "$baseline" "$coro" "$source_root"; do
	if [[ -n $(git -C "$root" status --porcelain) ]]; then
		echo "source must be clean: $root" >&2
		exit 2
	fi
	if [[ "$output" == "$root" || "$output" == "$root/"* ]]; then
		echo "output must be outside source trees" >&2
		exit 2
	fi
done
for root in "$baseline" "$coro"; do
	if [[ ! -x "$root/bin/go" ]]; then
		echo "missing toolchain: $root/bin/go" >&2
		exit 2
	fi
done

# Use explicit benchmark settings without modifying the caller's environment.
unset GOROOT GOOS GOARCH GOAMD64 GOARM64
export GOENV=off GOTOOLCHAIN=local GO111MODULE=off GOEXPERIMENT='' GOFLAGS=''
export CGO_ENABLED=1 GOGC=100 GOMEMLIMIT=off GODEBUG=
baseline_revision=$(git -C "$baseline" rev-parse HEAD)
coro_revision=$(git -C "$coro" rev-parse HEAD)
baseline_version=$("$baseline/bin/go" version)
coro_version=$("$coro/bin/go" version)
for mode in baseline coro; do
	if [[ $mode == baseline ]]; then
		revision=$baseline_revision
		version=$baseline_version
	else
		revision=$coro_revision
		version=$coro_version
	fi
	if [[ "$version" != *"_${revision:0:10} "* && "$version" != *"-${revision:0:10} "* ]]; then
		echo "$mode binary does not match Git revision $revision: $version" >&2
		exit 2
	fi
done
if [[ "$baseline_version" == *X:coro* || "$coro_version" != *X:coro* ]]; then
	echo "require independently built default and coro toolchains" >&2
	exit 2
fi
upstream_ref=${UPSTREAM_REF:-origin/master}
upstream_revision=$(git -C "$coro" rev-parse --verify --end-of-options "$upstream_ref^{commit}")
merge_base=$(git -C "$coro" merge-base "$upstream_revision" "$coro_revision")
if [[ "$merge_base" != "$baseline_revision" ]]; then
	echo "baseline is not the exact upstream merge-base: $merge_base" >&2
	exit 2
fi

samples=${SAMPLES:-6}
warmups=${WARMUP_SAMPLES:-2}
footprint_samples=${FOOTPRINT_SAMPLES:-4}
benchtime=${BENCHTIME:-200ms}
for value in "$samples" "$warmups" "$footprint_samples"; do
	if [[ ! $value =~ ^([1-9][0-9]?|100)$ ]] || ((value % 2 != 0)); then
		echo "sample counts must be positive even integers up to 100" >&2
		exit 2
	fi
done
if [[ ! $benchtime =~ ^[1-9][0-9]*(ms|s|x)$ ]]; then
	echo "BENCHTIME must be a positive integer followed by ms, s or x" >&2
	exit 2
fi
kind=${COLLECTION_KIND:-observational}
case $kind in
observational | validation-only) ;;
*) echo "unknown COLLECTION_KIND: $kind" >&2; exit 2 ;;
esac
source_revision=$(git -C "$source_root" rev-parse HEAD)

mkdir "$output"
cd "$source_dir"
{
	printf 'kind: %s; shared host is not assumed quiet\n' "$kind"
	printf 'baseline: %s\n%s\ncoroutine: %s\n%s\n' \
		"$baseline_revision" "$baseline_version" "$coro_revision" "$coro_version"
	printf 'fixture: %s\n' "$source_revision"
	printf 'upstream: %s\nsamples: %s; warmups: %s; benchtime: %s; footprint samples: %s\n' \
		"$upstream_revision" "$samples" "$warmups" "$benchtime" "$footprint_samples"
	uname -a
	uptime
} >"$output/manifest.txt"
trap 'printf "exit status: %s\n" "$?" >>"$output/manifest.txt"' EXIT

# Complete all compilation and correctness checks before collecting timings.
"$baseline/bin/go" test -c -gcflags=-l -o "$output/baseline.test" \
	>"$output/baseline.build.log" 2>&1
"$output/baseline.test" -test.count=1 -test.timeout=10m \
	>"$output/baseline.correctness.txt" 2>&1
./check.bash "$coro" "$output/coro-check" >"$output/coro-check.log" 2>&1
coro_binary="$output/coro-check/coro.test"
hash() {
	if command -v sha256sum >/dev/null; then
		sha256sum "$@"
	else
		shasum -a 256 "$@"
	fi
}
{
	hash "$output/baseline.test" "$coro_binary"
	while IFS= read -r -d '' path; do
		hash "$source_root/$path"
	done < <(git -C "$source_root" ls-files -z -- src/cmd/compile/testdata/corobench)
} >"$output/input-hashes.txt"

regular_names=(YieldBatch YieldEntry RecursiveYield64 RecursiveYield256
	RecursiveYield262 RecursiveYield264 RecursiveYield267 RecursiveYield4096
	RecursiveLargeFrame512 MutualYield64 MutualYield4096 AlternatingDeepFrames
	HeterogeneousFrameCacheReplacement DeferYield RecoverYield TaskSequence
	TaskBurst100 TaskBurst1000 TaskBurst100000 TaskPark100 TaskPark1000 TaskPark100000
	MixedParkScalarFirst100000 MixedParkPointerFirst100000
	ChannelRoundTrip ReadyChannelPair ReadySelect BlockedSelect
	SleepZero SleepNanosecond SleepMicrosecond FileRead TCPRead FileWrite TCPWrite
	FileBlockingProgress TCPBlockingProgress CScalar CAggregate CErrno CLibm
	CBlockingHandoff CBlockingGroup3)
regular="^Benchmark($(IFS='|'; echo "${regular_names[*]}"))$"
footprints=(LiveTaskFootprint1 LiveTaskFootprint256 LiveTaskFootprint264
	LiveTaskFootprint265 LiveTaskFootprint266 LiveTaskFootprint267
	LiveTaskFootprint268 LiveTaskFootprint1000 LiveTaskFootprint100000
	MixedScalarFirst1000 MixedScalarFirst100000 MixedPointerFirst1000 MixedPointerFirst100000)
run() {
	local mode=$1 prefix=$2 probes=$3 duration=$4 cpus=$5
	local expected=${6:-1}
	local binary="$output/baseline.test"
	if [[ $mode == coro ]]; then binary=$coro_binary; fi
	uptime >>"$output/load.txt"
	env GOMAXPROCS=1 "$binary" -test.run '^$' -test.bench "$probes" \
		-test.benchtime "$duration" -test.count=1 -test.cpu "$cpus" \
		-test.benchmem -test.timeout=10m \
		>"$output/$prefix.$mode.txt" 2>"$output/$prefix.$mode.stderr"
	awk -v expected="$expected" '
		$1 ~ /^Benchmark/ {
			samples++
			if ($2 !~ /^[0-9]+$/ || $2 < 1 || seen[$1]++) bad = 1
			bytes = allocs = 0
			for (i = 3; i <= NF; i++) {
				if ($i == "B/op") bytes++
				if ($i == "allocs/op") allocs++
			}
			if (bytes != 1 || allocs != 1) bad = 1
		}
		END { exit samples == expected && !bad ? 0 : 1 }
	' "$output/$prefix.$mode.txt"
}
for phase in warmup regular; do
	count=$samples
	if [[ $phase == warmup ]]; then count=$warmups; fi
	for ((round = 1; round <= count; round++)); do
		order='baseline coro'
		if ((round % 2 == 0)); then order='coro baseline'; fi
		for mode in $order; do
			run "$mode" "$phase.$round" "$regular" "$benchtime" 1 "${#regular_names[@]}"
		done
	done
done
for phase in scaling-warmup scaling; do
	count=$samples
	if [[ $phase == scaling-warmup ]]; then count=$warmups; fi
	for ((round = 1; round <= count; round++)); do
		order='baseline coro'
		if ((round % 2 == 0)); then order='coro baseline'; fi
		for mode in $order; do
			run "$mode" "$phase.$round" '^BenchmarkParallelYieldWork$' "$benchtime" 1,2,4 3
		done
	done
done
for ((round = 1; round <= footprint_samples; round++)); do
	order='baseline coro'
	if ((round % 2 == 0)); then order='coro baseline'; fi
	for probe in "${footprints[@]}"; do
		for mode in $order; do run "$mode" "footprint.$probe.$round" "^Benchmark${probe}$" 1x 1; done
	done
done
for root in "$baseline" "$coro" "$source_root"; do
	test -z "$(git -C "$root" status --porcelain)"
done
test "$(git -C "$baseline" rev-parse HEAD)" == "$baseline_revision"
test "$(git -C "$coro" rev-parse HEAD)" == "$coro_revision"
test "$(git -C "$source_root" rev-parse HEAD)" == "$source_revision"
if command -v sha256sum >/dev/null; then
	sha256sum -c "$output/input-hashes.txt" >"$output/hash-verification.txt"
else
	shasum -a 256 -c "$output/input-hashes.txt" >"$output/hash-verification.txt"
fi
printf 'collection complete; observational raw data: %s\n' "$output"
