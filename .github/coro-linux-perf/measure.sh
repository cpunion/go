#!/usr/bin/env bash
set -euo pipefail
fixture="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(git -C "$fixture" rev-parse --show-toplevel)"
ref=d340d47ae16f0ea71ba6231488862677e33d8b8c
official="$(go env GOROOT)"
sdk="$RUNNER_TEMP/coro-sdk"
samples="$RUNNER_TEMP/coro-linux-perf-work"
evidence="$RUNNER_TEMP/coro-linux-perf-evidence"
mkdir -p "$samples"/{mixed,range,broad,cache,tmp} "$evidence"
test "$RUNNER_ARCH" = X64
test "$(uname -m)" = x86_64
git -C "$repo" merge-base --is-ancestor "$ref" HEAD
{
    printf 'implementation %s\n' "$ref"
    printf 'workflow revision %s\n' "$(git -C "$repo" rev-parse HEAD)"
    printf 'runner architecture %s\n' "$RUNNER_ARCH"
    "$official/bin/go" version
    uname -a
    lscpu
    nproc
    if [[ -r /sys/fs/cgroup/cpu.max ]]; then cat /sys/fs/cgroup/cpu.max; fi
    uptime
} > "$evidence/machine.txt"
git -C "$repo" worktree add --detach "$sdk" "$ref"
export GOROOT_BOOTSTRAP="$official" GOTOOLCHAIN=local GOENV=off GOWORK=off
export GOCACHE="$samples/cache" GOTMPDIR="$samples/tmp"
unset GOROOT GOFLAGS GOEXPERIMENT GOOS GOARCH GOAMD64
(
    cd "$sdk/src"
    ./make.bash
) > "$evidence/bootstrap.log" 2>&1 || {
    tail -100 "$evidence/bootstrap.log"
    exit 1
}
"$sdk/bin/go" version | tee "$evidence/toolchain.txt"
built_ref="$(sed -nE 's/.*-devel_([0-9a-f]+) .*/\1/p' "$evidence/toolchain.txt")"
[[ "$built_ref" =~ ^[0-9a-f]{7,40}$ && "$ref" == "$built_ref"* ]]
cp "$fixture/bench.py" "$samples/"
cp "$fixture"/mixed/*.go "$fixture/mixed/go.mod" "$samples/mixed/"
cp "$fixture"/range/*.go "$fixture/range/go.mod" "$samples/range/"
cp "$sdk"/src/cmd/compile/testdata/corobench/*.{go,c} "$samples/broad/"
printf 'module corosurvey\n\ngo 1.27\n' > "$samples/broad/go.mod"
"$sdk/bin/go" build -o "$samples/verify-gates" "$fixture/verify-gates.go"
export CGO_ENABLED=1 GOGC=100 GOMEMLIMIT=off
for suite in mixed range broad; do
    (
        cd "$samples/$suite"
        GOROOT="$official" "$official/bin/go" test -c -mod=mod -buildmode=pie -o stock.test . > "$evidence/$suite-stock-build.txt" 2>&1
        GOEXPERIMENT=coro GOROOT="$sdk" "$sdk/bin/go" test -c -mod=mod -buildmode=pie -gcflags=-d=coro=4 -o final.test . > "$evidence/$suite-final-build.txt" 2>&1
        "$sdk/bin/go" version -m stock.test final.test > "$evidence/$suite-versions.txt"
        sha256sum ./*.go ./*.test > "$evidence/$suite-input-hashes.txt"
        case "$suite" in
          mixed) "$samples/verify-gates" final.test coroprobe.mixedYieldWork coroprobe.parallelYieldWork coroprobe.parallelYieldWorker ;;
          range) "$samples/verify-gates" final.test coroprobe.parallelYieldWork coroprobe.parallelYieldWorker ;;
          broad) "$samples/verify-gates" final.test corosurvey.sleepLoop corosurvey.channelRoundTrips corosurvey.packedBoundary440A corosurvey.taskBursts ;;
        esac > "$evidence/$suite-gates.txt"
    )
done

measure() {
    local suite=$1 rounds=$2 cases=$3
    (
        cd "$samples/$suite"
        uptime >> "$evidence/load.txt"
        python3 ../bench.py run "$rounds" "$cases" stock final > "$evidence/$suite.jsonl"
        uptime >> "$evidence/load.txt"
    )
}
measure mixed 24 ShortThenLong64:4:1000,ShortThenLong128:4:600,LongThenShort64:4:1000,B64W256:4:2700
measure range 24 B64W256:2:2400,B64W256:4:2700,B128W4096:4:660
measure broad 24 SleepZero:1:12000000,SleepNanosecond:1:450000,SleepMicrosecond:1:20000,SleepMicrosecond:4:21000,ChannelRoundTrip:4:200000,PackedBoundary440:1:380,TaskBurst100000:4:8
wc -l "$evidence"/*.jsonl | tee "$evidence/sample-counts.txt"
test "$(git -C "$sdk" rev-parse HEAD)" = "$ref"
test -z "$(git -C "$sdk" status --porcelain)"
