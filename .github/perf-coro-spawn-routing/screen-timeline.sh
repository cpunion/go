#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
root="$(pwd -P)"
export GOENV=off GOWORK=off GOTOOLCHAIN=local GOCACHE="$root/cache" GO111MODULE=off CGO_ENABLED=1
gofmt -w bench/timeline_test.go
cd bench
GOEXPERIMENT= go test -c -tags=corospawntimeline -o timeline-stock.test .
for mode in base waveall prefix1 prefix3; do
    flags=()
    if [[ "$mode" != base ]]; then flags=(-overlay="$root/overlays/$mode.json"); fi
    GOEXPERIMENT=coro "$root/sdk/bin/go" test -c -tags=corospawntimeline -gcflags=-d=coro=4 "${flags[@]}" -o "timeline-$mode.test" . > "../validation/build-timeline-$mode.log" 2>&1
done
"$root/sdk/bin/go" tool objdump -s 'timelineYield\.coro\.func1$' timeline-base.test | grep -E 'CALL.*coroSpawnScalarBatch' > ../validation/timeline-native.asm
"$root/sdk/bin/go" tool objdump -s 'timelineYield$' timeline-stock.test | grep -E 'CALL.*runtime.newproc' > ../validation/timeline-stock.asm
python3 -B ../timeline.py > ../validation/timeline.jsonl
