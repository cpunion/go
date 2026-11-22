#!/usr/bin/env bash
set -euo pipefail

fixture="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(git -C "$fixture" rev-parse --show-toplevel)"
base=86dfe67ed562b9307e91560c160c9b56e9e67b4b
candidate=793581ad80cd1333cc74f640179f061ae90f2cd1
official="$(go env GOROOT)"
samples="$RUNNER_TEMP/coro-frame-comparison"
evidence="$RUNNER_TEMP/coro-linux-perf-evidence"

test "$RUNNER_ARCH" = X64
test "$(uname -m)" = x86_64
mkdir -p "$samples"/{broad,cache,tmp} "$evidence"
{
    printf 'baseline %s\ncandidate %s\n' "$base" "$candidate"
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
    ref="$base"
    if [[ "$mode" == candidate ]]; then ref="$candidate"; fi
    sdk="$samples/$mode"
    git -C "$repo" merge-base --is-ancestor "$ref" HEAD
    git -C "$repo" worktree add --detach "$sdk" "$ref"
    (cd "$sdk/src" && ./make.bash) > "$evidence/$mode-bootstrap.log" 2>&1 || {
        tail -100 "$evidence/$mode-bootstrap.log"
        exit 1
    }
    test "$(git -C "$sdk" rev-parse HEAD)" = "$ref"
    version="$("$sdk/bin/go" version)"
    printf '%s %s\n' "$mode" "$version" >> "$evidence/toolchains.txt"
    prefix="$(sed -nE 's/.*-devel_([0-9a-f]+) .*/\1/p' <<< "$version")"
    [[ "$prefix" =~ ^[0-9a-f]{7,40}$ && "$ref" == "$prefix"* ]]
done
cp "$samples/base"/src/cmd/compile/testdata/corobench/*.{go,c} "$samples/broad/"
cp "$fixture/reader_effect_test.go" "$samples/broad/"
diff -qr "$samples/base/src/cmd/compile/testdata/corobench" "$samples/candidate/src/cmd/compile/testdata/corobench"
printf 'module corosurvey\n\ngo 1.27\n' > "$samples/broad/go.mod"
"$samples/base/bin/go" build -o "$samples/verify-gates" "$fixture/verify-gates.go"
cd "$samples/broad"
"$official/bin/go" test -c -mod=mod -buildmode=pie -o stock.test . > "$evidence/stock-build.log" 2>&1
for mode in base candidate; do
    GOEXPERIMENT=coro "$samples/$mode/bin/go" test -c -mod=mod -buildmode=pie -gcflags=-d=coro=4 -o "$mode.test" . > "$evidence/$mode-build.log" 2>&1
    "$samples/verify-gates" "$mode.test" corosurvey.packedBoundary432A corosurvey.packedBoundary440A corosurvey.packedBoundary448A corosurvey.packedMutualA corosurvey.taskBursts corosurvey.effectfulByteA corosurvey.callerEffectA > "$evidence/$mode-gates.txt"
    GOMAXPROCS=1 GOGC=1 "./$mode.test" -test.run='^TestPackedWorkloads$' -test.count=24 > "$evidence/$mode-stress.txt"
done
"$samples/base/bin/go" version -m stock.test base.test candidate.test > "$evidence/versions.txt"
sha256sum ./*.go ./*.test > "$evidence/input-hashes.txt"
uptime >> "$evidence/load.txt"
python3 "$fixture/bench.py" run 24 PackedBoundary432:1:6500,PackedBoundary440:1:6500,PackedBoundary448:1:6500,PackedMutual4096:1:400,PackedSelf4096:1:400,ChannelRoundTrip:4:200000,TaskBurst100000:4:6,PackedArrayReaderEffect440:1:1000,PackedArrayCallerEffect440:1:3000 stock base candidate > "$evidence/frames.jsonl"
uptime >> "$evidence/load.txt"
test "$(wc -l < "$evidence/frames.jsonl")" -eq 648

sdk="$samples/candidate"
(
    cd "$sdk/src"
    GOEXPERIMENT=coro,staticlockranking GOGC=1 "$sdk/bin/go" test -count=5 -gcflags=-d=checkptr=2 runtime \
        -run 'TestStacklessCoro(NativeFusedChunk|Fused(ChunkInline|Resume)|LocalFused|UncachedFusedCompletion|FrameChunkInvariants|NativeProfileTransitions|NativePreemption|ChunkSwitchRunsReadyTask|QueuedYieldRunsOrdinaryGoroutine)'
) > "$evidence/candidate-runtime-checkptr.txt" 2>&1

(
    cd "$sdk/src"
    GOEXPERIMENT=coro GO_TEST_TIMEOUT_SCALE=5 "$sdk/bin/go" test -count=1 cmd/compile/internal/coro \
        -run '^Test(InvariantReturn|RootArrayReconstructionProof|MutualRootArrayLineage|RootArrayCallerBoundary|FrameArrayReuseChunkPeers)$'
) > "$evidence/candidate-compiler-lineage.txt" 2>&1

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
GOMAXPROCS=1 ./diagnostic.test -test.run='^$' -test.bench='^BenchmarkPackedArrayReaderEffect440$' -test.benchtime=3s -test.cpuprofile="$samples/machine.pprof" > "$evidence/diagnostic-profile-bench.txt"
"$sdk/bin/go" tool pprof -top -nodecount=40 diagnostic.test "$samples/machine.pprof" > "$evidence/diagnostic-profile-top.txt"
