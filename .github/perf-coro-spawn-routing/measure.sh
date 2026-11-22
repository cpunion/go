#!/usr/bin/env bash
set -euo pipefail
source_revision="$1"
files="$(cd "$(dirname "$0")" && pwd -P)"
root="$RUNNER_TEMP/coro-spawn-routing"
mkdir -p "$root/bench" "$root/cache" "$root/overlays" "$root/validation"
git worktree add --detach "$root/sdk" "$source_revision"
export GOENV=off GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=1 GOMAXPROCS=6
export GOCACHE="$root/cache"
unset GOEXPERIMENT GOFLAGS
export GOROOT_BOOTSTRAP="$(go env GOROOT)"
cd "$root"
{
    git -C sdk rev-parse HEAD
    go version
    uname -srm
    lscpu
} > validation/machine.txt
(cd sdk/src && GOEXPERIMENT=coro ./make.bash) > validation/bootstrap.log 2>&1
sdk/bin/go version > validation/sdk-version.txt
test "$(git -C sdk rev-parse HEAD)" = "$source_revision"
grep -Fq "${source_revision:0:8}" validation/sdk-version.txt
cp -R sdk/src/cmd/compile/testdata/corobench/. bench/
cp "$files/bench.py" "$files/compute_test.go" "$files/cold_phase_test.go" bench/
cp "$files/bench.py" "$files/route.py" "$files/watch.py" "$files/phase.py" "$files/phase_summary.py" "$files"/screen-*.sh "$files/control.sh" .
cp "$files/wave.py" "$files/prefix.py" "$files/timeline.py" .
bash screen-route.sh
bash screen-watch.sh
bash screen-wave.sh
bash screen-prefix.sh
cp "$files/timeline_test.go" bench/
bash screen-timeline.sh
bash screen-phase.sh
python3 -B phase_summary.py validation/phase > validation/phase-summary.json
cp "$files/startup_control_test.go" bench/
cp -R "$files/startupcontrol" bench/
bash control.sh
