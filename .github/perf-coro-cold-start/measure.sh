#!/usr/bin/env bash
set -euo pipefail
revision="$1"
files="$(cd "$(dirname "$0")" && pwd)"
root="$RUNNER_TEMP/coro-cold-start"
mkdir -p "$root/bench" "$root/validation" "$root/cache"
git worktree add --detach "$root/sdk" "$revision"
export GOENV=off GOTOOLCHAIN=local GOWORK=off CGO_ENABLED=1 GOMAXPROCS=4
export GOCACHE="$root/cache"
unset GOFLAGS GOEXPERIMENT
export GOROOT_BOOTSTRAP="$(go env GOROOT)"
cd "$root"
{
    printf 'source %s\n' "$revision"
    "$GOROOT_BOOTSTRAP/bin/go" version
    uname -srm
    lscpu
} > validation/machine.txt
(cd sdk/src && GOEXPERIMENT=coro ./make.bash) > validation/bootstrap.log 2>&1
sdk/bin/go version > validation/sdk-version.txt
grep -q "${revision:0:8}" validation/sdk-version.txt
cp -R sdk/src/cmd/compile/testdata/corobench/. bench/
cp "$files/compute_test.go" "$files/cold_phase_test.go" bench/
cp "$files/bench.py" "$files/variants.py" "$files/screen.sh" .
(
    cd bench
    GO111MODULE=off GOEXPERIMENT=coro ../sdk/bin/go test -c -gcflags=-d=coro=4 -o base.test .
    GO111MODULE=off "$GOROOT_BOOTSTRAP/bin/go" test -c -o stock.test .
) > validation/base-build.log 2>&1
"$GOROOT_BOOTSTRAP/bin/go" version -m bench/base.test bench/stock.test > validation/versions.txt
bash screen.sh
