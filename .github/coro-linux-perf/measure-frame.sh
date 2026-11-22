#!/usr/bin/env bash
set -euo pipefail

fixture="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(git -C "$fixture" rev-parse --show-toplevel)"
ref=d340d47ae16f0ea71ba6231488862677e33d8b8c
official="$(go env GOROOT)"
sdk="$RUNNER_TEMP/coro-sdk"
samples="$RUNNER_TEMP/coro-frame-work"
evidence="$RUNNER_TEMP/coro-linux-perf-evidence"

test "$RUNNER_ARCH" = X64
test "$(uname -m)" = x86_64
git -C "$repo" merge-base --is-ancestor "$ref" HEAD
mkdir -p "$samples"/{broad,cache,tmp} "$evidence"
{
    printf 'implementation %s\n' "$ref"
    "$official/bin/go" version
    uname -a
    lscpu
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
test "$(git -C "$sdk" rev-parse HEAD)" = "$ref"
cp "$sdk"/src/cmd/compile/testdata/corobench/*.{go,c} "$samples/broad/"
printf 'module corosurvey\n\ngo 1.27\n' > "$samples/broad/go.mod"
"$sdk/bin/go" build -o "$samples/verify-gates" "$fixture/verify-gates.go"

cd "$samples/broad"
export CGO_ENABLED=1 GOAMD64=v1
"$official/bin/go" test -c -mod=mod -buildmode=pie -o stock.test . > "$evidence/stock-build.log" 2>&1
"$sdk/bin/go" test -c -mod=mod -buildmode=pie -o plain.test . > "$evidence/plain-build.log" 2>&1
GOEXPERIMENT=coro "$sdk/bin/go" test -c -mod=mod -buildmode=pie -gcflags=-d=coro=4 -o final.test . > "$evidence/final-build.log" 2>&1
"$sdk/bin/go" version -m stock.test plain.test final.test > "$evidence/versions.txt"
sha256sum ./*.go ./*.test > "$evidence/input-hashes.txt"
"$samples/verify-gates" final.test corosurvey.packedBoundary432A corosurvey.packedBoundary440A corosurvey.packedBoundary448A corosurvey.packedMutualA > "$evidence/gates.txt"

uptime >> "$evidence/load.txt"
python3 "$fixture/bench.py" run 24 PackedBoundary432:1:2500,PackedBoundary440:1:2500,PackedBoundary448:1:2500,PackedMutual4096:1:240,PackedSelf4096:1:240 stock plain final > "$evidence/frames.jsonl"
uptime >> "$evidence/load.txt"
test "$(wc -l < "$evidence/frames.jsonl")" -eq 360

for mode in stock plain final; do
    "$sdk/bin/go" tool objdump -s '^corosurvey\.packed(Boundary(432|440|448)[AB]|Consume(432|440|448))(\.|$)' "$mode.test" > "$evidence/$mode-objdump.txt"
    GOMAXPROCS=1 "./$mode.test" -test.run='^$' -test.bench='^BenchmarkPackedBoundary440$' -test.benchtime=160000x -test.benchmem -test.cpuprofile="$samples/$mode.pprof" > "$evidence/$mode-profile-benchmark.txt"
    "$sdk/bin/go" tool pprof -top -nodecount=35 "$mode.test" "$samples/$mode.pprof" > "$evidence/$mode-profile-top.txt"
    "$sdk/bin/go" tool pprof -list 'packedBoundary440|packedConsume440|coroAdvanceFusedChunk|coroPopFusedChunk|coroCompleteFusedFrame' "$mode.test" "$samples/$mode.pprof" > "$evidence/$mode-profile-source.txt" 2>&1 || true
done
test -z "$(git -C "$sdk" status --porcelain)"
