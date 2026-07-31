# Coroutine architecture probes

This directory contains portable Go and cgo benchmarks for comparing the
native stackless coroutine experiment with the unmodified Go compiler at its
exact upstream merge-base. The same source directory is compiled by both
toolchains. It imports no private coroutine package and uses no source
annotation to select coroutine functions.

These are architecture probes, not a complete language or standard-library
benchmark suite. The first set deliberately prioritizes places where the
implementations differ most:

- amortized scheduler yield and one public coroutine-root entry;
- direct and deeply recursive calls, including allocation cost when logical
  frames replace native stack growth;
- sequential, burst, and parked task creation;
- live heap and stack footprint with 10,000 simultaneously parked tasks;
- unbuffered channel round trips and ready `select`;
- zero and one-nanosecond sleeps;
- ready one-byte reads through `os.File.Read` and `net.TCPConn.Read`;
- scalar, aggregate, errno, and libm C calls;
- progress from a blocking C call to an already-runnable sibling at
  `GOMAXPROCS=1`.

The I/O benchmarks intentionally use the ordinary public APIs. They therefore
include the experiment's current closure and worker-pool adaptation rather
than measuring only lower-level runtime helpers.

## Reproducibility

Build one GOROOT at the coroutine branch's upstream merge-base and another at
the coroutine revision under test. The runner verifies that the first revision
is the merge-base of the two histories:

```
cd src
GOROOT_BOOTSTRAP=/path/to/bootstrap ./make.bash

cd /path/to/coroutine/src
GOROOT_BOOTSTRAP=/path/to/bootstrap GOEXPERIMENT=coro ./make.bash

cd /path/to/coroutine/src/cmd/compile/testdata/corobench
./run.bash /path/to/baseline /path/to/coroutine /tmp/corobench-results
```

`SAMPLES` defaults to 10 and `BENCHTIME` defaults to 500ms. The runner
runs three unrecorded warm-up pairs, then alternates which toolchain runs first
on each recorded sample. `FOOTPRINT_SAMPLES` defaults to six separate one-shot
measurements.

The output directory contains benchmark text for each toolchain, compiler
diagnostics, a lowering audit, the coroutine test binary, and its symbol table.
Before measuring, the runner requires every intended helper to be classified
and lowered, then checks direct entries for all five C probes. A fallback is a
capability result and must not be presented as coroutine performance.

`check.bash` runs the coroutine correctness and coverage test, verifies the
lowering audit, and checks the direct C symbols without collecting performance
data. CI runs the correctness test with both toolchains, invokes this check,
and retains its artifacts. It does not enforce performance thresholds:
hosted-runner measurements are not stable enough for that purpose.
