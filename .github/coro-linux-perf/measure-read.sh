#!/usr/bin/env bash
set -euo pipefail

fixture="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(git -C "$fixture" rev-parse --show-toplevel)"
baseline=d74ccdb6c7a804900087ff4a44d4daac2a696b23
candidate=8214d8b6e6fc9d58f2785adccde7b34d0fe518cc
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
grep -F "go1.28-devel_d74ccdb6" "$evidence/toolchains.txt"
grep -F "go1.28-devel_8214d8b6" "$evidence/toolchains.txt"
cp "$sdk"/src/cmd/compile/testdata/corobench/*.{go,c} "$samples/broad/"
cp -p "$fixture/"{reader_effect,buffered_pairs}_test.go "$samples/broad/"
printf 'module corosurvey\n\ngo 1.27\n' > "$samples/broad/go.mod"
"$sdk/bin/go" build -o "$samples/verify-gates" "$fixture/verify-gates.go"
cd "$samples/broad"
"$official/bin/go" test -c -mod=mod -buildmode=pie -o stock.test . > "$evidence/stock-build.log" 2>&1
for mode in base candidate; do
    sdk="$samples/$mode"
    GOEXPERIMENT=coro "$sdk/bin/go" test -c -mod=mod -buildmode=pie -gcflags=-d=coro=4 -o "$mode.test" . > "$evidence/$mode-build.log" 2>&1
    "$samples/verify-gates" "$mode.test" corosurvey.fileReads corosurvey.fileWrites corosurvey.tcpReads corosurvey.readySelects > "$evidence/$mode-gates.txt"
done
sdk="$samples/candidate"
"$sdk/bin/go" version -m stock.test base.test candidate.test > "$evidence/versions.txt"
uptime >> "$evidence/load.txt"
cases=FileRead:1:60000,FileRead:4:60000,FileWrite:1:50000,FileWrite:4:50000,FileBlockingProgress:4:2800
cases+=,TCPRead:4:13000,TCPWrite:4:7500,ReadySelect:4:370000,TaskPark100000:4:5,RecursiveLargeFrame512:1:350
python3 "$fixture/bench.py" run 24 "$cases" stock base candidate > "$evidence/read.jsonl"
test "$(wc -l < "$evidence/read.jsonl")" -eq 720
uptime >> "$evidence/load.txt"
cd "$sdk/src"
GOEXPERIMENT=coro,staticlockranking "$sdk/bin/go" test -count=12 -gcflags=all=-d=checkptr=2 -run 'Test(StacklessCoro.*(FileRead|SocketRead|SelectFastPath|WriteProgress)|Coro(Read|Write))' runtime internal/poll > "$evidence/runtime-checkptr.txt" 2>&1 || {
    cat "$evidence/runtime-checkptr.txt"
    exit 1
}
GOEXPERIMENT=coro "$sdk/bin/go" test -count=1 -run 'Test(IOCancellation|LowerOrdinaryIO|OrdinaryIOOperations|OrdinaryIOMethodErrors|ChannelSelectLowering)$' cmd/compile/internal/coro > "$evidence/compiler-source.txt" 2>&1 || {
    cat "$evidence/compiler-source.txt"
    exit 1
}
