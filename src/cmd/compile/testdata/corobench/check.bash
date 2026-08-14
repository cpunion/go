#!/usr/bin/env bash

# Copyright 2026 The Go Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

set -euo pipefail

if [[ $# -ne 2 ]]; then
	echo "usage: check.bash CORO_GOROOT OUTPUT_DIRECTORY" >&2
	exit 2
fi

coro_goroot=$(cd "$1" && pwd)
output_dir=$2
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
if [[ ! -x "$coro_goroot/bin/go" ]]; then
	echo "$coro_goroot/bin/go is not executable" >&2
	exit 2
fi

mkdir -p "$output_dir"
output_dir=$(cd "$output_dir" && pwd)
lowering_output="$output_dir/lowering.txt"
symbols_output="$output_dir/coro.symbols"
test_binary="$output_dir/coro.test"
build_output="$output_dir/build.txt"
allocation_output="$output_dir/public-entry-allocation.txt"
read_output="$output_dir/read-lowering.txt"
correctness_output="$output_dir/correctness.txt"
public_coverage_output="$output_dir/public-read-coverage.txt"
poll_coverage_output="$output_dir/poll-read-coverage.txt"
probe_coverage="$output_dir/probes.cover"
public_coverage="$output_dir/public-read.cover"
poll_coverage="$output_dir/poll-read.cover"
merged_coverage="$output_dir/combined.cover"
coverage_functions="$output_dir/coverage-functions.txt"

cd "$script_dir"
export GO111MODULE=off
export GOEXPERIMENT=coro

if ! "$coro_goroot/bin/go" test -count=1 -covermode=set \
	-coverprofile="$probe_coverage" -gcflags='-l -d=coro=4' \
	>"$correctness_output" 2>&1; then
	tail -n 100 "$correctness_output" >&2
	exit 1
fi
if ! "$coro_goroot/bin/go" test -count=1 -covermode=set \
	-coverpkg='internal/poll,os,net' -coverprofile="$public_coverage" \
	-gcflags='-l -d=coro=4' >"$public_coverage_output" 2>&1; then
	tail -n 100 "$public_coverage_output" >&2
	exit 1
fi
if ! "$coro_goroot/bin/go" test -count=1 -covermode=set \
	-coverprofile="$poll_coverage" internal/poll -run '^TestCoroRead' \
	>"$poll_coverage_output" 2>&1; then
	tail -n 100 "$poll_coverage_output" >&2
	exit 1
fi
awk '
	$1 == "mode:" {
		if (!mode) {
			mode = $2
		}
		next
	}
	{
		key = $1 " " $2
		if (!(key in seen)) {
			order[++count] = key
			seen[key] = 1
			hits[key] = $3
		} else if ($3 > hits[key]) {
			hits[key] = $3
		}
	}
	END {
		print "mode: " mode
		for (i = 1; i <= count; i++) {
			print order[i], hits[order[i]]
		}
	}
' "$probe_coverage" "$public_coverage" "$poll_coverage" \
	>"$merged_coverage"
"$coro_goroot/bin/go" tool cover -func="$merged_coverage" \
	>"$coverage_functions"
coverage_targets=(
	'internal/poll/fd_coro_unix.go:tryReadLock'
	'internal/poll/fd_coro_unix.go:CoroReadStart'
	'internal/poll/fd_coro_unix.go:CoroReadFinish'
	'internal/poll/fd_coro_unix.go:CoroCallRead'
	'os/file_coro.go:coroReadStart'
	'os/file_coro.go:coroReadFinish'
	'net/fd_coro.go:coroReadStart'
	'net/fd_coro.go:coroReadFinish'
	'net/fd_coro.go:coroReadError'
)
for target in "${coverage_targets[@]}"; do
	file=${target%:*}
	function_name=${target##*:}
	percentage=$(awk -v file="$file" -v fn="$function_name" '
		index($1, file ":") == 1 && $2 == fn { print $3 }
	' "$coverage_functions")
	if [[ -z "$percentage" ]]; then
		echo "missing coverage for $file $function_name" >&2
		exit 1
	fi
	if ! awk -v value="${percentage%\%}" 'BEGIN { exit value >= 90 ? 0 : 1 }'; then
		echo "$file $function_name coverage is $percentage, want at least 90%" >&2
		exit 1
	fi
done
if ! "$coro_goroot/bin/go" test -a -run '^$' -count=1 \
	-gcflags='-l -d=coro=4' >"$lowering_output" 2>&1; then
	tail -n 100 "$lowering_output" >&2
	exit 1
fi

expected=(
	yieldLoop
	yieldEntry
	recursiveYield
	deferYield
	recoverYield
	taskWorker
	taskSequence
	taskBursts
	taskParkWorker
	taskParkBursts
	taskParkUntilReleased
	parallelSpawnWorker
	parallelSpawnProgress
	parallelYieldWorker
	parallelYieldWork
	channelWorker
	channelRoundTrips
	readySelects
	sleepLoop
	fileReads
	tcpReads
	waitForEpoch
	contendedFileRead
	contendedFileRead1
	contendedFileRead2
	contendedFileReads
	blockingFileRead
	blockingFileRelease
	blockingFileRoundTrips
	contendedTCPRead
	contendedTCPRead1
	contendedTCPRead2
	contendedTCPReads
	blockingTCPRead
	blockingTCPRelease
	blockingTCPRoundTrips
	cScalarCalls
	cPairCalls
	cErrnoCalls
	cLibmCalls
	handoffWorker
	cBlockingHandoffs
	cBlockingGroupWorker
	cBlockingGroup
)
for function in "${expected[@]}"; do
	if ! grep -Eq "coro: func=.*\\.${function} .* primary=coro" \
		"$lowering_output"; then
		echo "$function was not classified as a coroutine primary" >&2
		exit 1
	fi
	if grep -Eq "coro: skip .*\\.${function}([ :]|$)" "$lowering_output"; then
		echo "$function fell back during coroutine lowering" >&2
		exit 1
	fi
done
if ! grep -Eq 'coro: phase=lower lowered=[1-9][0-9]*' "$lowering_output"; then
	echo "coroutine lowering did not lower any probe functions" >&2
	exit 1
fi

if ! "$coro_goroot/bin/go" test -c -o "$test_binary" \
	-gcflags='-l -d=coro=4' >"$build_output" 2>&1; then
	tail -n 100 "$build_output" >&2
	exit 1
fi
"$coro_goroot/bin/go" tool nm "$test_binary" >"$symbols_output"
for function in "${expected[@]}"; do
	if ! grep -Eq "\\.${function}\\.coro$" "$symbols_output"; then
		echo "missing lowered coroutine symbol for $function" >&2
		exit 1
	fi
done
if ! "$test_binary" -test.run '^$' -test.bench '^BenchmarkYieldEntry$' \
	-test.benchmem -test.benchtime=10000x -test.count=3 \
	>"$allocation_output"; then
	tail -n 100 "$allocation_output" >&2
	exit 1
fi
if ! awk '
	$1 ~ /^BenchmarkYieldEntry-/ {
		samples++
		for (i = 2; i <= NF; i++) {
			if (($i == "B/op" || $i == "allocs/op") && $(i - 1) != 0) {
				bad = 1
			}
		}
	}
	END { exit samples == 3 && !bad ? 0 : 1 }
' "$allocation_output"; then
	cat "$allocation_output" >&2
	echo "public coroutine entry allocation is not zero" >&2
	exit 1
fi

: >"$read_output"
read_functions=(
	fileReads
	blockingFileRead
	contendedFileRead
	tcpReads
	blockingTCPRead
	contendedTCPRead
)
for function in "${read_functions[@]}"; do
	disassembly=$("$coro_goroot/bin/go" tool objdump \
		-s "${function}\\.coro\\.func[0-9]+$" "$test_binary")
	printf '%s\n' "$disassembly" >>"$read_output"
	case "$function" in
	fileReads | blockingFileRead | contendedFileRead)
		read_type='os.(*File)'
		;;
	tcpReads | blockingTCPRead | contendedTCPRead)
		read_type='net.(*conn)'
		;;
	esac
	for phase in Start Finish; do
		if ! grep -Fq "$read_type.coroRead$phase" <<<"$disassembly"; then
			echo "$function does not call $read_type.coroRead$phase" >&2
			exit 1
		fi
	done
	if grep -Fq 'runtime.coroCallRead' <<<"$disassembly"; then
		echo "$function still calls the generic read worker" >&2
		exit 1
	fi
done
for symbol in probe_scalar probe_pair_add probe_errno probe_sin probe_block \
	probe_block_group; do
	if ! grep -Eq "_Cdirect2?_${symbol}" "$symbols_output"; then
		echo "missing direct C symbol for $symbol" >&2
		exit 1
	fi
done

printf 'coroutine checks passed; artifacts: %s\n' "$output_dir"
