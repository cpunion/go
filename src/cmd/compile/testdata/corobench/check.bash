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

cd "$script_dir"
export GO111MODULE=off
export GOEXPERIMENT=coro

"$coro_goroot/bin/go" test -count=1 -cover
"$coro_goroot/bin/go" test -a -run '^$' -count=1 \
	-gcflags='-l -N -d=coro=4' >"$lowering_output" 2>&1

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
	blockingFileRead
	blockingFileRelease
	blockingFileRoundTrips
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
for symbol in probe_scalar probe_pair_add probe_errno probe_sin probe_block \
	probe_block_group; do
	if ! grep -Eq "_Cdirect2?_${symbol}" "$symbols_output"; then
		echo "missing direct C symbol for $symbol" >&2
		exit 1
	fi
done

printf 'coroutine checks passed; artifacts: %s\n' "$output_dir"
