# Stage-2 Coroutine Performance & Attribution Suite (`suitev2`)

`suitev2` measures clean-baseline coroutine performance and phase cost attribution across all three required comparison groups:

1. **Same-commit `coro` vs `nocoro`** (`--coro` vs `--nocoro`)
2. **Exact upstream merge-base** (`--coro` / `--nocoro` vs `--upstream`)
3. **Clean parent vs single optimization candidate** (`--parent` vs `--candidate`)

## Workloads

- **Real I/O** (`io_real_test.go`): `RealIOPipeRoundTrip16`, `RealIOTCPEcho16`, `RealIOTCPFanout8`.
- **Parameterized / Capturing / Open-Callable Fan-out/Fan-in** (`fanout_test.go`): `FanoutArgs{16,100,1000}`, `FanoutCapture{16,100,1000}`, `FanoutDynamicOpen64`.
- **Massive Parking** (`massive_park_test.go`): `MassivePark{10k,30k,100k}` (`-benchtime=1x` fresh process reporting spawn-park, wake-drain, wake `p50`/`p95`/`p99`, parked heap / Go-stack / OS-RSS, unhosted ratio, and `GC1`/`GC2`/`GC4`/`FreeOSMemory` reclamation).
- **Phase Attribution** (`phase_attrib_test.go`): `PhaseRootStart`, `PhaseSpawnComplete{100,1000}`, `PhaseSpawnYield{100,1000}`, `PhaseParkWake{100,1000}`, `PhaseChannelPingPong`.
- **Ordinary Fallback Control** (`fanout_test.go`): `FallbackOrdinaryHost16` (deliberately uses ordinary cgo to prove automatic fallback segregation).

## Execution Path Verification & Invalidation

Every coro-enabled SDK (`coro`, `parent`, `candidate`) is verified BOTH:
- **Statically**: `-d=coro=4` diagnostics, `go tool nm` `.coro` symbols, and `.coro.func*` vs public entry `go tool objdump`.
- **At runtime outside timing**: `TestSuiteV2RuntimePath` walks hardware frame pointers (`fpprobe/fpprobe_{arm64,amd64}.s`) at every tested `P` for every benchmark root and worker, observing actual runtime frames (`runtime.coroNativeMain` -> `native-coro`, `runtime.(*stacklessCoroScheduler).*` without native -> `managed-coro`, no coroutine frames -> `ordinary-fallback`).

If any core benchmark unexpectedly falls back, loses a required target operation, mixes backends, or changes execution path between `parent` and `candidate`, `run_v2.py` marks the comparison **INVALID** and exits with code `2`.

## Full SDK Identity Verification

Every SDK must supply a verifiable full 40-character Git SHA from either a clean `.git` worktree or `<goroot>/CORO_BUILD_MANIFEST.json` (checked against live `sha256` of `bin/go` and `pkg/tool/*/compile`, plus `go version` short-SHA prefix). `run_v2.py` verifies same-commit `coro`/`nocoro`, exact `git merge-base` for `upstream`, and Mode-3 `parent -> candidate` commit ancestry.

## Usage

```bash
# Harness unit tests
python3 test_run_v2.py

# Baseline comparison (Modes 1 + 2)
python3 run_v2.py \
  --upstream /path/to/upstream-72aa6db794 \
  --nocoro   /path/to/e277e2c6e3-nocoro \
  --coro     /path/to/e277e2c6e3-coro \
  --git-repo /path/to/go-repo \
  --output   /path/to/results/baseline

# Single-patch attribution (Mode 3)
python3 run_v2.py \
  --parent    /path/to/e277e2c6e3-coro \
  --candidate /path/to/patch-coro \
  --output    /path/to/results/patch
```
