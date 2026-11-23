#!/usr/bin/env bash
set -euo pipefail
revision="$1"
files="$(cd "$(dirname "$0")" && pwd -P)"
root="$RUNNER_TEMP/coro-manager-activation"
mkdir -p "$root/bench" "$root/cache" "$root/overlays" "$root/validation" "$root/progress"
if ! git cat-file -e "$revision^{commit}"; then
    git fetch --depth=1 --no-tags origin "$revision"
fi
git worktree add --detach "$root/sdk" "$revision"
test "$(git -C "$root/sdk" rev-parse HEAD)" = "$revision"
export GOENV=off GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=1 GOMAXPROCS=6
export GOCACHE="$root/cache"
unset GOFLAGS GOEXPERIMENT
export GOROOT_BOOTSTRAP="$(go env GOROOT)"
cd "$root"
{
    printf 'source %s\n' "$revision"
    go version
    uname -srm
    lscpu
} > validation/machine.txt
(cd sdk/src && GOEXPERIMENT=coro ./make.bash) > validation/bootstrap.log 2>&1
sdk/bin/go version > validation/sdk-version.txt
grep -Fq "${revision:0:8}" validation/sdk-version.txt
cp -R sdk/src/cmd/compile/testdata/corobench/. bench/
cp "$files/bench.py" "$files/compute_test.go" bench/
cp "$files/progress/main.go" progress/
cp "$files"/*.py "$files"/screen-*.sh .
for phase in queue transfer timer-reuse channels handoff manager-worker parked progress; do
    printf 'Running %s\n' "$phase"
    bash "screen-$phase.sh"
done
