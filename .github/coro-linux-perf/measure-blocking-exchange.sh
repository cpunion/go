#!/usr/bin/env bash
set -euo pipefail

fixture="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(git -C "$fixture" rev-parse --show-toplevel)"
baseline=f44386cff1326cb8527ebb944302d99eb877a8cf
candidate=6e096c5187a3e55d137f1ec6c0fa1aaf9da910e4
official="$(go env GOROOT)"
samples="$RUNNER_TEMP/coro-blocking-public-exchange"
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
cp -p "$fixture/"{reader_effect,buffered_pairs,socket_burst,parameter_pair,buffered_exchange}_test.go "$samples/broad/"
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
    "$sdk/bin/go" tool objdump -s '^corosurvey[.]readyParameterChannelPairs[.]coro[.]func[0-9]+$' "$mode.test" > "$evidence/$mode-exchange.asm"
    test -s "$evidence/$mode-exchange.asm"
done
for mode in base candidate; do
    grep -q 'CALL.*runtime[.]coroTryChanExchange' "$evidence/$mode-exchange.asm"
done
cases=ReadyChannelParameterPair:1:1800000,ReadyChannelParameterPair:4:1800000,ReadyChannelPrefilledPair:4:1400000
cases+=,UnbufferedChannelParameterPair:1:95000,UnbufferedChannelParameterPair:4:95000,ChannelRoundTrip:1:120000,ChannelRoundTrip:4:120000
cases+=,ReadySelect:4:370000,BlockedSelect:4:120000,ReadyChannelPair:4:5000000,TaskBurst100000:4:20,FileRead:4:55000
python3 "$fixture/bench.py" run 24 "$cases" stock base candidate > "$evidence/buffered-exchange.jsonl"
test "$(wc -l < "$evidence/buffered-exchange.jsonl")" -eq 864
uptime >> "$evidence/load.txt"
cp -R "$fixture/public_exchange" "$samples/public"
cd "$samples/public"
"$official/bin/go" test -c -mod=mod -buildmode=pie -o stock.test . > "$evidence/public-stock-build.log" 2>&1
for mode in base candidate; do
    go="$samples/$mode/bin/go"
    GOEXPERIMENT=coro "$go" test -c -mod=mod -buildmode=pie -gcflags='exchangestress/pair=-d=coro=4' -o "$mode.test" . > "$evidence/public-$mode-build.log" 2>&1
    "$go" tool objdump -s '^exchangestress/pair[.]Exchange$' "$mode.test" > "$evidence/public-$mode.asm"
    grep -q 'CALL.*runtime[.]coroRunFrame' "$evidence/public-$mode.asm"
done
grep -q 'CALL.*runtime[.]coroTryChanExchange' "$evidence/public-base.asm"
grep -q 'CALL.*runtime[.]coroTryChanExchange' "$evidence/public-candidate.asm"
"$sdk/bin/go" version -m stock.test base.test candidate.test > "$evidence/public-versions.txt"
python3 "$fixture/bench.py" run 24 ReadyPublicExchange:1:160000,ReadyPublicExchange:4:160000 stock base candidate > "$evidence/public-exchange.jsonl"
test "$(wc -l < "$evidence/public-exchange.jsonl")" -eq 144
cp -R "$fixture/return_exchange" "$samples/returns"
cd "$samples/returns"
"$official/bin/go" test -c -mod=mod -buildmode=pie -o stock.test . > "$evidence/returns-stock-build.log" 2>&1
for mode in base candidate; do
    go="$samples/$mode/bin/go"
    GOEXPERIMENT=coro "$go" test -c -mod=mod -buildmode=pie -gcflags='exchangestress/pair=-d=coro=4' -o "$mode.test" . > "$evidence/returns-$mode-build.log" 2>&1
    "$go" tool objdump -s '^exchangestress/pair[.](Adjusted|CommaOK)$' "$mode.test" > "$evidence/returns-$mode.asm"
    test "$(grep -c 'CALL.*runtime[.]coroRunFrame' "$evidence/returns-$mode.asm")" -eq 2
done
test "$(grep -c 'CALL.*runtime[.]coroTryChanExchange' "$evidence/returns-base.asm")" -eq 2
test "$(grep -c 'CALL.*runtime[.]coroTryChanExchange' "$evidence/returns-candidate.asm")" -eq 2
"$sdk/bin/go" version -m stock.test base.test candidate.test > "$evidence/returns-versions.txt"
python3 "$fixture/bench.py" run 24 ReadyPublicExchangeAdjusted:1:140000,ReadyPublicExchangeAdjusted:4:140000,ReadyPublicExchangeCommaOK:1:140000,ReadyPublicExchangeCommaOK:4:140000 stock base candidate > "$evidence/returns.jsonl"
test "$(wc -l < "$evidence/returns.jsonl")" -eq 288
cp -R "$fixture/blocking_exchange" "$samples/blocking"
cd "$samples/blocking"
"$official/bin/go" test -c -mod=mod -buildmode=pie -o stock.test . > "$evidence/blocking-stock-build.log" 2>&1
for mode in base candidate; do
    go="$samples/$mode/bin/go"
    GOEXPERIMENT=coro "$go" test -c -mod=mod -buildmode=pie -gcflags='exchangestress/pair=-d=coro=4' -o "$mode.test" . > "$evidence/blocking-$mode-build.log" 2>&1
    "$go" tool objdump -s '^runtime[.]coroTryChanExchange$' "$mode.test" > "$evidence/blocking-$mode.asm"
    test -s "$evidence/blocking-$mode.asm"
done
grep -q 'CALL.*runtime[.]chansend' "$evidence/blocking-candidate.asm"
grep -q 'CALL.*runtime[.]chanrecv' "$evidence/blocking-candidate.asm"
"$sdk/bin/go" version -m stock.test base.test candidate.test > "$evidence/blocking-versions.txt"
python3 "$fixture/bench.py" run 24 PublicUnbufferedExchange:1:55000,PublicUnbufferedExchange:4:55000 stock base candidate > "$evidence/blocking.jsonl"
test "$(wc -l < "$evidence/blocking.jsonl")" -eq 144
cd "$sdk/src"
GOEXPERIMENT=coro,staticlockranking "$sdk/bin/go" test -count=12 -gcflags=all=-d=checkptr=2 -run 'TestStacklessCoro.*([Cc]hannel|[Ss]elect)' runtime internal/poll > "$evidence/runtime-checkptr.txt" 2>&1 || {
    cat "$evidence/runtime-checkptr.txt"
    exit 1
}
GOEXPERIMENT=coro "$sdk/bin/go" test -count=1 -run '^Test(BufferedChannelExchange|PublicBufferedChannelExchangeMiss|PrivateBufferedChannelPairs|NativeChannelPairWake|PublicChannelEntry|ChannelSelectLowering|OrdinaryIOCompletionAfterWait|OrderedReturnEffects)$' cmd/compile/internal/coro > "$evidence/compiler-source.txt" 2>&1 || {
    cat "$evidence/compiler-source.txt"
    exit 1
}

GOEXPERIMENT=coro "$sdk/bin/go" test -count=1 cmd/compile/internal/escape > "$evidence/compiler-escape.txt" 2>&1
