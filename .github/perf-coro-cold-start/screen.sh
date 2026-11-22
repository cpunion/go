#!/usr/bin/env bash
set -euo pipefail
export GOENV=off GOTOOLCHAIN=local GOWORK=off GO111MODULE=off GOEXPERIMENT=coro
export GOCACHE="$PWD/cache"
root="$PWD"
python3 -B variants.py phase
cd bench
for mode in phase allp batchwake batchall managed ownerwake; do
    flags=()
    if [[ "$mode" == phase ]]; then flags=(-tags=corocoldphase); fi
    "$root/sdk/bin/go" test -c -p=7 -overlay="$root/overlays/$mode.json" -gcflags=-d=coro=4 "${flags[@]}" -o "$mode.test" . > "$root/validation/build-$mode.log" 2>&1
    if cmp -s base.test "$mode.test"; then
        printf 'overlay was not applied for %s\n' "$mode" >&2
        exit 1
    fi
done
shasum -a 256 base.test allp.test batchwake.test batchall.test managed.test ownerwake.test phase.test > "$root/validation/binary-hashes.txt"
"$root/sdk/bin/go" tool nm phase.test | grep -E 'runtime.coroCold(Record|Dump)' > "$root/validation/phase-symbols.txt"
for trial in {1..16}; do
    for point in ColdPhase16:4 ColdPhaseLight64:2 ColdPhaseLight64:4; do
        name="${point%:*}"
        procs="${point##*:}"
        GOMAXPROCS="$procs" ./phase.test -test.run='^$' -test.bench="^Benchmark${name}$" -test.benchtime=1x > "$root/validation/phase-${name}-${procs}-${trial}.log" 2>&1
    done
done
modes=(stock base allp batchwake batchall managed ownerwake)
python3 -B ../bench.py run 28 ParallelYieldWork16:4:1,ParallelYieldWork16:2:1,ParallelYieldLight64:2:1,ParallelYieldLight64:4:1,ParallelYieldWork:2:1,ParallelYieldWork:4:1 "${modes[@]}" > ../validation/screen-cold.jsonl
python3 -B ../bench.py run 28 ParallelYieldWork16:4:4,ParallelYieldLight64:2:4,ParallelYieldLight64:4:4,ParallelYieldWork:2:4,ParallelYieldWork:4:4 "${modes[@]}" > ../validation/screen-warm.jsonl
python3 -B ../bench.py run 24 TaskBurst100000:2:4,TaskBurst100000:4:4,TaskPark100000:4:4,MixedParkScalarFirst100000:4:4 "${modes[@]}" > ../validation/screen-guard.jsonl
