#!/usr/bin/env bash
set -euo pipefail

fixture="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(git -C "$fixture" rev-parse --show-toplevel)"
baseline=1472c651ba279083a031f76affb9d03f90c6c610
candidate=f7607ac52c7d6533c4daa103fecddec20fb7c71b
official="$(go env GOROOT)"
samples="$RUNNER_TEMP/coro-frame-comparison"
evidence="$RUNNER_TEMP/coro-linux-perf-evidence"

test "$RUNNER_ARCH" = X64
test "$(uname -m)" = x86_64
mkdir -p "$samples"/{broad,cache,tmp} "$evidence"
{
    printf 'baseline %s\n' "$baseline"
    printf 'candidate %s\n' "$candidate"
    "$official/bin/go" version
    uname -a
    lscpu
    uptime
} > "$evidence/machine.txt"
export GOROOT_BOOTSTRAP="$official" GOTOOLCHAIN=local GOENV=off GOWORK=off
export GOCACHE="$samples/cache" GOTMPDIR="$samples/tmp"
unset GOROOT GOFLAGS GOEXPERIMENT GOOS GOARCH GOAMD64
export CGO_ENABLED=1 GOAMD64=v1
for mode in base candidate; do
    revision="$candidate"
    if [[ "$mode" = base ]]; then
        revision="$baseline"
    fi
    git -C "$repo" merge-base --is-ancestor "$revision" HEAD
    sdk="$samples/$mode"
    git -C "$repo" worktree add --detach "$sdk" "$revision"
    (cd "$sdk/src" && ./make.bash) > "$evidence/$mode-bootstrap.log" 2>&1 || {
        tail -100 "$evidence/$mode-bootstrap.log"
        exit 1
    }
    test "$(git -C "$sdk" rev-parse HEAD)" = "$revision"
    "$sdk/bin/go" version >> "$evidence/toolchains.txt"
done
grep -F "go1.28-devel_${baseline:0:8}" "$evidence/toolchains.txt"
grep -F "go1.28-devel_${candidate:0:8}" "$evidence/toolchains.txt"
cp "$sdk"/src/cmd/compile/testdata/corobench/*.{go,c} "$samples/broad/"
cp -p "$fixture/"{reader_effect,buffered_pairs,socket_burst,parameter_pair}_test.go "$samples/broad/"
printf 'module corosurvey\n\ngo 1.27\n' > "$samples/broad/go.mod"
"$sdk/bin/go" build -o "$samples/verify-gates" "$fixture/verify-gates.go"
cd "$samples/broad"
"$official/bin/go" test -c -mod=mod -buildmode=pie -o stock.test . > "$evidence/stock-build.log" 2>&1
for mode in base candidate; do
    sdk="$samples/$mode"
    GOEXPERIMENT=coro "$sdk/bin/go" test -c -mod=mod -buildmode=pie -gcflags=-d=coro=4 -o "$mode.test" . > "$evidence/$mode-build.log" 2>&1
    "$samples/verify-gates" "$mode.test" corosurvey.readySelects corosurvey.blockedSelects corosurvey.channelRoundTrips corosurvey.readyParameterChannelPairs > "$evidence/$mode-gates.txt"
done
sdk="$samples/candidate"
"$sdk/bin/go" version -m stock.test base.test candidate.test > "$evidence/versions.txt"
uptime >> "$evidence/load.txt"
for mode in base candidate; do
    "$sdk/bin/go" tool objdump -s '^corosurvey[.]BenchmarkReadyChannelPair$' "$mode.test" > "$evidence/$mode-private-pairs.asm"
    test -s "$evidence/$mode-private-pairs.asm"
done
if grep -E 'runtime[.](chansend|chanrecv|coroChan|coroNative|coroRun)' "$evidence/candidate-private-pairs.asm"; then
    echo "candidate still calls the channel runtime in the private pair benchmark"
    exit 1
fi
cases=ReadyChannelPair:1:8000000,ReadyChannelPair:4:8000000,ReadyChannelParameterPair:4:1000000,ReadySelect:4:370000,BlockedSelect:4:120000
cases+=,ChannelRoundTrip:1:120000,ChannelRoundTrip:4:120000,TCPWrite:1:13000,FileRead:4:55000,TaskBurst100000:4:20,PackedArrayReaderEffect440:1:10000
python3 "$fixture/bench.py" run 24 "$cases" stock base candidate > "$evidence/channel-pairs.jsonl"
test "$(wc -l < "$evidence/channel-pairs.jsonl")" -eq 792
uptime >> "$evidence/load.txt"
cd "$sdk/src"
GOEXPERIMENT=coro,staticlockranking "$sdk/bin/go" test -count=12 -gcflags=all=-d=checkptr=2 -run 'TestStacklessCoro.*([Cc]hannel|[Ss]elect)' runtime internal/poll > "$evidence/runtime-checkptr.txt" 2>&1 || {
    cat "$evidence/runtime-checkptr.txt"
    exit 1
}
GOEXPERIMENT=coro "$sdk/bin/go" test -count=1 -run '^Test(PrivateBufferedChannelPairs|NativeChannelPairWake|PublicChannelEntry|ChannelSelectLowering|OrdinaryIOCompletionAfterWait|OrderedReturnEffects)$' cmd/compile/internal/coro > "$evidence/compiler-source.txt" 2>&1 || {
    cat "$evidence/compiler-source.txt"
    exit 1
}
