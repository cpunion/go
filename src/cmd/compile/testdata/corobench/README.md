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

## Initial architecture comparison

The initial measurements were collected on 2026-07-31. The baseline was
`3e6eb83f958cb7d447e5e1e02f0c196a19817d5e`, the exact upstream merge-base,
and the coroutine revision was
`efe7ca1d5b2ad98bf8388e14d3d4ba2fa751593f`. Both used the same source,
disabled inlining, set `GOMAXPROCS=1`, discarded three warm-up pairs, and
alternated toolchain order over ten recorded samples. The footprint values
used six independent processes.

The Darwin measurements ran natively on an Apple M4 Max. The Linux
measurements ran as Ubuntu amd64 under OrbStack translation, so they validate
lowering, ABI behavior, direction, and broad magnitude; their absolute times
are not directly comparable with native Darwin. Each entry below is the
median baseline followed by the median coroutine result.

| Probe | Darwin arm64 | Linux amd64 |
| --- | ---: | ---: |
| amortized `Gosched` | 42.06 ns -> 17.32 ns (-59%) | 55.55 ns -> 34.07 ns (-39%) |
| one public coroutine entry | 45.98 ns -> 1.154 us (25x) | 58.74 ns -> 1.124 us (19x) |
| recursive yield, depth 4096 | 18.74 us -> 477.41 us (25x) | 9.19 us -> 897.55 us (98x) |
| defer across a yield | 54.54 ns -> 1.171 us (21x) | 61.78 ns -> 1.162 us (19x) |
| panic/recover across a yield | 161.5 ns -> 1.307 us (8.1x) | 217.8 ns -> 1.368 us (6.3x) |
| park and wake 100 tasks | 14.96 us -> 30.41 us (2.0x) | 20.95 us -> 46.42 us (2.2x) |
| channel round trip | 157.7 ns -> 493.4 ns (3.1x) | 204.8 ns -> 814.6 ns (4.0x) |
| ready `select` | 39.91 ns -> 311.75 ns (7.8x) | 55.02 ns -> 520.4 ns (9.5x) |
| `Sleep(0)` | 1.036 ns -> 5.096 us (4917x) | 1.343 ns -> 26.16 us (19480x) |
| `Sleep(1ns)` | 121.7 ns -> 5.183 us (43x) | 353.0 ns -> 25.77 us (73x) |
| ready `/dev/zero` read | 305.2 ns -> 5.604 us (18x) | 180.0 ns -> 27.53 us (153x) |
| ready loopback TCP read | 266.3 ns -> 5.569 us (21x) | 210.1 ns -> 27.86 us (133x) |
| blocking C round trip | 70.64 us -> 25.13 us (-64%) | 1.873 ms -> 87.51 us (-95%) |
| runnable-sibling progress during C | 69.04 us -> 7.381 us (-89%) | 1.869 ms -> 25.59 us (-99%) |
| park and wake 10,000 tasks | 9.773 ms -> 622.10 ms (64x) | 9.735 ms -> 342.56 ms (35x) |

The ordinary direct C calls did not allocate. Scalar calls improved from
17.56 ns to 11.78 ns on Darwin and from 27.52 ns to 20.23 ns on Linux.
Aggregate, errno, and libm calls improved by 12-26% on Linux, but their
Darwin differences were not statistically significant in this run. The
translated Linux environment substantially amplifies the official cgo
blocking-handoff cost, so only the direction, not its 95-99% magnitude,
should be generalized.

The 10,000-task footprint exposes the central space/time tradeoff. Live stack
space fell from about 2048 B to 6.554 B per task and live heap fell from
623.1 B to 372.4 B per task. Live objects increased from 2.002 to 4.252 per
task, while the park/wake operation became 35-64 times slower. Deep recursion
similarly replaced native stack growth with 464.5 KiB and 16,393 allocations
per operation. A public coroutine entry currently costs 520 B and eight
allocations.

The ready-path adapters also make their fixed costs visible: each timer call
used 296 B and three allocations, each file or TCP read used 256 B and two
allocations, a channel round trip used 992 B and six allocations, and a ready
`select` used 516 B and five allocations. Parking 100 tasks used 37,312 B and
501 allocations. The blocking C probe used 88 B and three allocations even
though the ordinary direct C calls remained allocation-free.

Sequential and burst task creation are intentionally not classified as a
speedup: they were statistically indistinguishable on Darwin and 5-13% slower
on Linux, while consistently adding 68 B and three allocations per task.

These results identify optimization targets without requiring a complete
feature matrix. The highest-leverage targets are logical-frame allocation,
park/wake queue and object costs, the zero-duration timer fast path, ready
file/network I/O without a closure/worker hop, and channel/select adapters.
The direct C fast path and the live-stack reduction should be preserved while
those costs are addressed.

### Non-positive sleep fast path

Revision `eda8306ce2` made the compiler-generated sleep operation conditional:
the runtime helper reports whether a positive-duration timer was started, and
the logical task only returns a wait action in that case. Against its exact
parent, `b26bb6fe67`, the same portable benchmark produced these medians:

| Probe | Darwin arm64 | Linux amd64 |
| --- | ---: | ---: |
| `Sleep(0)` | 5.615 us -> 2.657 ns (-99.95%) | 27.21 us -> 18.75 ns (-99.93%) |
| allocations | 296 B, 3 allocs -> 0 B, 0 allocs | 296 B, 3 allocs -> 0 B, 0 allocs |

In a separate comparison, the unmodified Go baseline measured 1.024 ns on
Darwin and 2.279 ns under the translated Linux environment. An
alternating-order control measurement of `Sleep(1ns)` found no significant
parent-to-change difference: 5.922 us versus 5.949 us on Darwin (`p=0.853`),
and 31.78 us versus 31.82 us on Linux (`p=0.796`). The fast path therefore
removes the unnecessary timer and scheduler round trip without changing the
positive-duration path.

`check.bash` runs the coroutine correctness and coverage test, verifies the
lowering audit, and checks the direct C symbols without collecting performance
data. CI runs the correctness test with both toolchains, invokes this check,
and retains its artifacts. It does not enforce performance thresholds:
hosted-runner measurements are not stable enough for that purpose.
