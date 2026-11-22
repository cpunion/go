#!/usr/bin/env bash
set -euo pipefail

fixture="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(git -C "$fixture" rev-parse --show-toplevel)"
baseline=f7607ac52c7d6533c4daa103fecddec20fb7c71b
official="$(go env GOROOT)"
samples="$RUNNER_TEMP/coro-buffered-receive"
evidence="$RUNNER_TEMP/coro-linux-perf-evidence"

test "$RUNNER_ARCH" = X64
test "$(uname -m)" = x86_64
mkdir -p "$samples"/{broad,cache,tmp} "$evidence"
{
    printf 'baseline %s\n' "$baseline"
    "$official/bin/go" version
    uname -a
    lscpu
    uptime
} > "$evidence/machine.txt"
export GOROOT_BOOTSTRAP="$official" GOTOOLCHAIN=local GOENV=off GOWORK=off
export GOCACHE="$samples/cache" GOTMPDIR="$samples/tmp"
unset GOROOT GOFLAGS GOEXPERIMENT GOOS GOARCH GOAMD64
export CGO_ENABLED=1 GOAMD64=v1
sdk="$samples/base"
git -C "$repo" merge-base --is-ancestor "$baseline" HEAD
git -C "$repo" worktree add --detach "$sdk" "$baseline"
(cd "$sdk/src" && ./make.bash) > "$evidence/base-bootstrap.log" 2>&1
test "$(git -C "$sdk" rev-parse HEAD)" = "$baseline"
"$sdk/bin/go" version > "$evidence/toolchains.txt"
grep -F "go1.28-devel_${baseline:0:8}" "$evidence/toolchains.txt"
mkdir -p "$samples/probe"
cp "$sdk/src/runtime/chan_coro.go" "$samples/probe/chan_coro.go"
patch "$samples/probe/chan_coro.go" < "$fixture/receive-buffered.patch" > "$evidence/patch.log"
sha256sum "$sdk/src/runtime/chan_coro.go" "$samples/probe/chan_coro.go" > "$evidence/source-sha256.txt"
overlay="$samples/probe/overlay.json"
python3 - "$sdk" "$samples/probe" <<'OVERLAY'
import json
from pathlib import Path
import sys
sdk, probe = map(Path, sys.argv[1:])
(probe / "overlay.json").write_text(json.dumps({"Replace": {
    str(sdk / "src/runtime/chan_coro.go"): str(probe / "chan_coro.go"),
}}))
OVERLAY
cp "$sdk"/src/cmd/compile/testdata/corobench/*.{go,c} "$samples/broad/"
cp -p "$fixture/"{reader_effect,buffered_pairs,socket_burst,parameter_pair}_test.go "$samples/broad/"
printf 'module corosurvey\n\ngo 1.27\n' > "$samples/broad/go.mod"
"$sdk/bin/go" build -o "$samples/verify-gates" "$fixture/verify-gates.go"
cd "$samples/broad"
"$official/bin/go" test -c -mod=mod -buildmode=pie -o stock.test . > "$evidence/stock-build.log" 2>&1
for mode in base probe; do
    flags=()
    if [[ "$mode" = probe ]]; then flags+=("-overlay=$overlay"); fi
    GOEXPERIMENT=coro "$sdk/bin/go" test "${flags[@]}" -c -mod=mod -buildmode=pie -gcflags=-d=coro=4 -o "$mode.test" . > "$evidence/$mode-build.log" 2>&1
    "$samples/verify-gates" "$mode.test" corosurvey.readyParameterChannelPairs corosurvey.readySelects corosurvey.channelRoundTrips > "$evidence/$mode-gates.txt"
    "$sdk/bin/go" tool objdump -s '^runtime[.]coroEntryChanRecv$' "$mode.test" > "$evidence/$mode-receive.asm"
    test -s "$evidence/$mode-receive.asm"
done
if cmp -s base.test probe.test; then
    echo "overlay did not change the executable"
    exit 1
fi
"$sdk/bin/go" version -m stock.test base.test probe.test > "$evidence/versions.txt"
uptime > "$evidence/load.txt"
cases=ReadyChannelParameterPair:1:1200000,ReadyChannelParameterPair:4:1200000,ReadySelect:4:370000,BlockedSelect:4:120000,ChannelRoundTrip:4:120000,FileRead:4:55000
python3 "$fixture/bench.py" run 24 "$cases" stock base probe > "$evidence/receive.jsonl"
test "$(wc -l < "$evidence/receive.jsonl")" -eq 432
uptime >> "$evidence/load.txt"
cd "$sdk/src"
GOEXPERIMENT=coro,staticlockranking "$sdk/bin/go" test -overlay="$overlay" -count=12 -gcflags=all=-d=checkptr=2 -run 'TestStacklessCoro.*([Cc]hannel|[Ss]elect)' runtime > "$evidence/runtime-checkptr.txt" 2>&1
GOEXPERIMENT=coro GOFLAGS="-overlay=$overlay" "$sdk/bin/go" test -count=1 -run '^Test(PrivateBufferedChannelPairs|NativeChannelPairWake|PublicChannelEntry|ChannelSelectLowering)$' cmd/compile/internal/coro > "$evidence/compiler-source.txt" 2>&1
test "$(git -C "$sdk" rev-parse HEAD)" = "$baseline"
test -z "$(git -C "$sdk" status --porcelain)"
