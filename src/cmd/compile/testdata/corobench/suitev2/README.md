# Stage-2 Coroutine Performance & Attribution Suite (`suitev2`)

`suitev2` measures stackless coroutine performance with mandatory execution-path auditing, tail latency, process OS RSS, GC reclamation, and phase cost attribution.

## Three Required Comparison Groups

1. **Same-commit experiment switch cost** (`coro` vs `nocoro`):
   - Measures the isolated impact of `GOEXPERIMENT=coro` + `-gcflags='-l -d=coro=4'` vs `GOEXPERIMENT=nocoro` at the exact same Git commit.
2. **Exact upstream merge-base delta** (`coro`/`nocoro` vs `upstream`):
   - Verifies `git merge-base` against unmodified upstream Go (e.g. `72aa6db794`) and tracks both total fork delta (`coro/upstream`) and experiment-off drift (`nocoro/upstream`).
3. **Single-optimization patch attribution** (`candidate` vs `parent`):
   - Compares a single candidate commit against its clean parent commit (`--parent` vs `--candidate`) using the same paired interleaved rounds and path audit.

## Execution Path Audit & Segregation

Every benchmark is audited automatically from `-d=coro=4` compiler diagnostics, `go tool nm`, and `go tool objdump` (inspecting `.coro.func*` lowered state machines separately from public `coroUseOrdinary` entry wrappers):

- **`native-coro`** / **`managed-coro`**: All entry/worker functions lowered to `.coro`, all required coroutine operations present in disassembly, and no `runtime.newproc` fallback inside `.coro.func*`. Scored in the Core Dashboard.
- **`ordinary-fallback`**: Skipped by lowering, missing `.coro` symbol, or calls `runtime.newproc` inside `.coro.func*`. Segregated into Section 7 (never mixed into stackless scores). `FallbackOrdinaryHost16` serves as a built-in control proving automatic fallback detection.
- **`compiler-eliminated`**: Lowered to `.coro` without fallback, but a required target operation (e.g., `coroChanSend`/`coroChanRecv`/`coroReadStart`) disappeared from disassembly. Segregated into Section 8 as compiler-optimization-only data.

## Usage

Baseline (Modes 1 & 2):

```bash
python3 run_v2.py \
  --output /path/to/new-output-dir \
  --upstream /path/to/upstream-goroot \
  --nocoro /path/to/same-commit-nocoro-goroot \
  --coro /path/to/same-commit-coro-goroot \
  --upstream-ref master \
  --samples 6 --warmups 2 --park-samples 4 --benchtime 150ms --cpus 1,2,4
```

Single-patch attribution (Mode 3):

```bash
python3 run_v2.py \
  --output /path/to/patch-eval-dir \
  --parent /path/to/clean-parent-coro-goroot \
  --candidate /path/to/candidate-coro-goroot \
  --samples 6 --warmups 2 --park-samples 4 --benchtime 150ms --cpus 1,2,4
```

Unit tests for parser and audit classifier:

```bash
python3 test_run_v2.py -v
```
