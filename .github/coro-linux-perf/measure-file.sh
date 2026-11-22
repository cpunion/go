#!/usr/bin/env bash
set -euo pipefail

fixture="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(git -C "$fixture" rev-parse --show-toplevel)"
baseline=2b68c5f0a009740509cf12fccbf72259dbca18a4
candidate=0ca84893c87faeb321069940b0e3637cf660c856
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
grep -F "go1.28-devel_2b68c5f0" "$evidence/toolchains.txt"
grep -F "go1.28-devel_0ca84893" "$evidence/toolchains.txt"
cp "$sdk"/src/cmd/compile/testdata/corobench/*.{go,c} "$samples/broad/"
cp -p "$fixture/reader_effect_test.go" "$samples/broad/"
printf 'module corosurvey\n\ngo 1.27\n' > "$samples/broad/go.mod"
"$sdk/bin/go" build -o "$samples/verify-gates" "$fixture/verify-gates.go"
cd "$samples/broad"
"$official/bin/go" test -c -mod=mod -buildmode=pie -o stock.test . > "$evidence/stock-build.log" 2>&1
for mode in base candidate; do
    sdk="$samples/$mode"
    GOEXPERIMENT=coro "$sdk/bin/go" test -c -mod=mod -buildmode=pie -gcflags=-d=coro=4 -o "$mode.test" . > "$evidence/$mode-build.log" 2>&1
    "$samples/verify-gates" "$mode.test" corosurvey.fileWrites corosurvey.fileReads corosurvey.tcpWrites corosurvey.effectfulByteA > "$evidence/$mode-gates.txt"
done
sdk="$samples/candidate"
"$sdk/bin/go" version -m stock.test base.test candidate.test > "$evidence/versions.txt"
uptime >> "$evidence/load.txt"
cases=FileWrite:1:50000,FileWrite:4:50000,FileRead:1:50000,FileRead:4:50000,TCPWrite:4:7000,TCPRead:4:14000
cases+=,PackedArrayReaderEffect440:1:10000,PackedArrayCallerEffect440:1:10000,ChannelRoundTrip:4:180000,TaskBurst100000:4:25
python3 "$fixture/bench.py" run 24 "$cases" stock base candidate > "$evidence/file.jsonl"
test "$(wc -l < "$evidence/file.jsonl")" -eq 720
uptime >> "$evidence/load.txt"
for p in 1 4; do
    if ! GOGC=1 GOMAXPROCS="$p" ./candidate.test -test.run='^TestPublic(FileWrite|TCPWrite|UDPWrite|WriteBlockingProgress|WriteDeadline)$' -test.count=12 > "$evidence/write-source-P$p.txt" 2>&1; then
        cat "$evidence/write-source-P$p.txt"
        exit 1
    fi
done
cd "$sdk/src"
if ! GOEXPERIMENT=coro,staticlockranking "$sdk/bin/go" test -count=6 -gcflags=all=-d=checkptr=2 -run 'Test(StacklessCoro.*([Ww]rite|Blocking)|CoroWrite)' runtime internal/poll > "$evidence/runtime-checkptr.txt" 2>&1; then
    cat "$evidence/runtime-checkptr.txt"
    exit 1
fi
