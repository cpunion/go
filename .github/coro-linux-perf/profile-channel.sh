#!/usr/bin/env bash
set -euo pipefail

fixture="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(git -C "$fixture" rev-parse --show-toplevel)"
candidate=793581ad80cd1333cc74f640179f061ae90f2cd1
official="$(go env GOROOT)"
samples="$RUNNER_TEMP/coro-frame-comparison"
evidence="$RUNNER_TEMP/coro-linux-perf-evidence"

test "$RUNNER_ARCH" = X64
test "$(uname -m)" = x86_64
mkdir -p "$samples"/{broad,cache,tmp} "$evidence"
{
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
sdk="$samples/candidate"
git -C "$repo" merge-base --is-ancestor "$candidate" HEAD
git -C "$repo" worktree add --detach "$sdk" "$candidate"
(cd "$sdk/src" && ./make.bash) > "$evidence/candidate-bootstrap.log" 2>&1 || {
    tail -100 "$evidence/candidate-bootstrap.log"
    exit 1
}
test "$(git -C "$sdk" rev-parse HEAD)" = "$candidate"
"$sdk/bin/go" version >> "$evidence/toolchains.txt"
grep -F "go1.28-devel_793581ad" "$evidence/toolchains.txt"
cp "$sdk"/src/cmd/compile/testdata/corobench/*.{go,c} "$samples/broad/"
printf 'module corosurvey\n\ngo 1.27\n' > "$samples/broad/go.mod"
"$sdk/bin/go" build -o "$samples/verify-gates" "$fixture/verify-gates.go"
cd "$samples/broad"
"$official/bin/go" test -c -mod=mod -buildmode=pie -o stock.test . > "$evidence/stock-build.log" 2>&1
GOEXPERIMENT=coro "$sdk/bin/go" test -c -mod=mod -buildmode=pie -gcflags=-d=coro=4 -o candidate.test . > "$evidence/candidate-build.log" 2>&1
"$samples/verify-gates" candidate.test corosurvey.channelRoundTrips > "$evidence/candidate-gates.txt"
"$sdk/bin/go" version -m stock.test candidate.test > "$evidence/versions.txt"
uptime >> "$evidence/load.txt"
python3 "$fixture/bench.py" run 30 ChannelRoundTrip:1:200000,ChannelRoundTrip:4:200000,BlockedSelect:4:100000 stock candidate > "$evidence/channel.jsonl"
test "$(wc -l < "$evidence/channel.jsonl")" -eq 180
uptime >> "$evidence/load.txt"
# Run this separate diagnostic binary after the measured samples. Attribute
# interrupted coroutine transitions to the signal's machine PC.
python3 - "$sdk/src/runtime/proc.go" "$samples" <<'PY'
import json
from pathlib import Path
import sys
original, out = (Path(p) for p in sys.argv[1:])
body = original.read_text()
needle = "stk[n] = abi.FuncPCABIInternal(_LostCoroSourceTransition) + sys.PCQuantum"
assert body.count(needle) == 1
proc = out / "diagnostic-proc.go"
proc.write_text(body.replace(needle, "stk[n] = pc + sys.PCQuantum"))
(out / "diagnostic.json").write_text(json.dumps({"Replace": {str(original): str(proc)}}))
PY
GOEXPERIMENT=coro "$sdk/bin/go" test -c -mod=mod -buildmode=pie -gcflags=-d=coro=4 -overlay "$samples/diagnostic.json" -o diagnostic.test . > "$evidence/diagnostic-build.log" 2>&1
for p in 1 4; do
    for flavor in stock candidate diagnostic; do
        GOMAXPROCS="$p" "./$flavor.test" -test.run='^$' -test.bench='^BenchmarkChannelRoundTrip$' \
            -test.benchtime=4s -test.cpuprofile="$samples/$flavor-p$p.pprof" > "$evidence/$flavor-p$p-bench.txt"
        "$sdk/bin/go" tool pprof -top -nodecount=35 "$flavor.test" "$samples/$flavor-p$p.pprof" \
            > "$evidence/$flavor-p$p-top.txt"
    done
done
