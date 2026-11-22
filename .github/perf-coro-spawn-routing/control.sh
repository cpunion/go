#!/usr/bin/env bash
set -euo pipefail
root="$PWD"
export GOENV=off GOTOOLCHAIN=local GOWORK=off GO111MODULE=off
export GOCACHE="$root/cache"
cd bench
GOEXPERIMENT=coro "$root/sdk/bin/go" test -c -gcflags=-d=coro=4 -o coro-control.test . > ../validation/control-build.log 2>&1
GOEXPERIMENT= go test -c -o go-control.test . >> ../validation/control-build.log 2>&1
"$root/sdk/bin/go" tool objdump -s 'startupcontrol.PrimeThreads$' coro-control.test | grep -E 'TEXT|CALL.*runtime.newproc' > ../validation/control-ordinary.asm
"$root/sdk/bin/go" tool objdump -s 'yieldEntry$' coro-control.test | grep -E 'TEXT|CALL.*runtime.coroRunFrame' > ../validation/control-native.asm
grep -q 'runtime.newproc' ../validation/control-ordinary.asm
grep -q 'runtime.coroRunFrame' ../validation/control-native.asm
python3 -B ../bench.py run 40 ColdControlWork16:4:1,ThreadControlWork16:4:1,ContextControlWork16:4:1,BothControlWork16:4:1,ColdControlLight64:4:1,ThreadControlLight64:4:1,ContextControlLight64:4:1,BothControlLight64:4:1,ColdControlLight64:2:1,ThreadControlLight64:2:1,BothControlLight64:2:1 go-control coro-control > ../validation/thread-controls.jsonl
