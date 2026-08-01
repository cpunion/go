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
- blocking pipe and loopback TCP reads released by a runnable logical sibling;
- fixed-work task batches at `GOMAXPROCS=1,2,4`;
- scalar, aggregate, errno, and libm C calls;
- progress from a blocking C call to an already-runnable sibling at
  `GOMAXPROCS=1`.
- groups of three blocking C calls, plus a self-timed capacity probe with
  eight concurrent calls.

The I/O benchmarks intentionally use the ordinary public APIs. They therefore
include the experiment's current closure and worker-pool adaptation rather
than measuring only lower-level runtime helpers. The blocking probes release
each public read with a one-byte raw write syscall that cannot fill the empty
pipe or socket buffer; this keeps the release path out of the unsupported
public write lowering. A preceding `Sleep(0)` keeps the release worker on the
automatically colored path without adding a positive-duration timer.

Every probe is interpreted as one of three capability states. A native fast
path is a performance result. An adapter fallback is an end-to-end measurement
of the current implementation and is labeled as such. An unsupported or
capacity-limited path is a capability result only and is not presented as
performance. The eight-call C benchmark performs a self-timed preflight and
skips its timing when the logical scheduler cannot keep all calls blocked
while retaining an executor to release them.

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
measurements. The fixed-work scaling probe uses the same defaults and runs
separately with `-cpu=1,2,4`; `SCALING_SAMPLES` and `SCALING_BENCHTIME` can
override them.

The output directory contains benchmark text for each toolchain, compiler
diagnostics, the eight-call C capacity result, a lowering audit, the coroutine
test binary, and its symbol table. Before measuring, the runner requires every
intended helper to be classified and lowered, then checks direct entries for
all C probes. A fallback is a capability result and must not be presented as
coroutine performance.

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

### Blocking capacity and parallel scaling

Revision `05335641ac` added measurement-only probes for differences that do
not require a complete implementation to expose. It used the same
`3e6eb83f95` merge-base, three warm-up pairs, ten alternating-order 500ms
samples, and disabled inlining. The Darwin results are native Apple M4 Max
measurements; the Linux results are amd64 measurements under OrbStack
translation. Each entry is the median baseline followed by the median
coroutine result.

| Probe | Darwin arm64 | Linux amd64 |
| --- | ---: | ---: |
| blocking pipe read and sibling release | 1.624 us -> 7.491 us (4.6x) | 1.551 us -> 30.837 us (20x) |
| blocking TCP read and sibling release | 10.31 us -> 12.70 us (+23%) | 2.758 us -> 33.388 us (12x) |
| three concurrent blocking C calls | 224.1 us -> 51.530 ms (230x) | 6.731 ms -> 41.940 ms (6.2x) |
| entry time per blocking C call | 55.916 us -> 8.806 us (-84%) | 1.926 ms -> 28.30 us (-99%) |
| fixed work, `GOMAXPROCS=1` | 544.8 us -> 544.2 us (~) | 567.9 us -> 566.4 us (~) |
| fixed work, `GOMAXPROCS=2` | 290.6 us -> 543.5 us (+87%) | 368.6 us -> 560.9 us (+52%) |
| fixed work, `GOMAXPROCS=4` | 160.3 us -> 545.5 us (+240%) | 226.2 us -> 562.4 us (+149%) |
| eight concurrent blocking C calls | supported -> capacity-limited | supported -> capacity-limited |

The direct C entry advantage remains visible, especially in the translated
Linux environment where the baseline pays its full cgo handoff cost. The
Darwin entry metric was variable, however, and the end-to-end result shows
that the current executor release and recovery path dominates once several C
calls block concurrently. The eight-call result is deliberately reported only
as a capability state; individual timeout counts are scheduler-dependent.

The fixed-work baseline improved by 3.4x on Darwin and 2.5x on translated
Linux from one to four Ps. The coroutine path did not measurably scale. Its
fixed-work batches also allocated about 6400 B and 256 objects, while the
baseline allocated none. Each blocking file or TCP operation used a fixed
1736 B and 26 allocations on both platforms; the baseline used none. These
results isolate executor capacity, release/recovery, multi-P scheduling, and
the public I/O adapter as higher-priority architecture gaps without first
requiring complete timer, file, and network implementations.

### Blocking C return progress

Revision `b9520875d0` lets an executor returning from a blocking C call request
a P when every P is occupied by other coroutine executors. Its exact parent is
`9e5fa62e01`. The parent benchmark binary was built at `05335641ac`; the only
intervening change was this benchmark's documentation and its merge, so the
executable sources are identical to the exact parent.

The comparison used the same checked-in probes, `GOMAXPROCS=1`, three warm-up
pairs, and ten alternating-order 500ms samples. The Darwin results are native
Apple M4 Max measurements; the Linux results are amd64 measurements under
OrbStack translation.

| Probe | Darwin arm64 | Linux amd64 |
| --- | ---: | ---: |
| three concurrent blocking C calls | 56.323 ms -> 110.5 us (-99.80%, 510x) | 49.110 ms -> 960.6 us (-98.04%, 51x) |
| blocking C handoff | 24.57 us -> 23.99 us (~) | 119.8 us -> 111.8 us (~) |
| logical yield batch | 17.34 ns -> 17.11 ns (~) | 34.90 ns -> 30.98 ns (-11%) |

Ordinary direct C calls remained allocation-free. Their short-call results
ranged from no change to +5.3% on Darwin and from -3.6% to +3.8% on translated
Linux. These small, bidirectional changes were sensitive to native code layout;
the executed Go fast path has the same instruction shape before and after this
change. They are therefore not classified as an algorithmic regression or
improvement.

For scale only, rather than as a paired statistical comparison, the repaired
three-call result is about twice as fast on Darwin and seven times as fast on
translated Linux as the earlier unmodified-Go measurements in the preceding
section. Eight simultaneous blocking calls remain capacity-limited, and the
coroutine scheduler still does not scale fixed work across multiple Ps. Those
are separate executor-capacity and multi-P scheduling tasks.

`check.bash` runs the coroutine correctness and coverage test, verifies the
lowering audit, and checks the direct C symbols without collecting performance
data. CI runs the correctness test with both toolchains, invokes this check,
and retains its artifacts. It does not enforce performance thresholds:
hosted-runner measurements are not stable enough for that purpose.

### Elastic blocking C capacity

Revision `3a2a4d07e9` replaces the fixed four-executor limit with four warm
executors and on-demand growth. Its exact parent is `50c67d66c0`. A scheduler
now counts executors from blocking entry until they return with a P. When all
current capacity is blocked, a managed-stack coordinator starts another
replacement executor. Native contexts retain a four-entry lock-free reuse
path and use a locked overflow list beyond it.

The paired comparison used `GOMAXPROCS=1`, three warm-up pairs, and ten
alternating-order 500ms samples. Darwin ran natively on an Apple M4 Max. Linux
amd64 ran under OrbStack translation, so its absolute times should not be
compared with Darwin. Each entry is the median exact parent followed by the
median change.

| Probe | Darwin arm64 | Linux amd64 |
| --- | ---: | ---: |
| scalar C call | 11.76 ns -> 12.21 ns (+3.9%) | 20.66 ns -> 21.28 ns (+3.0%) |
| aggregate C call | 14.94 ns -> 15.16 ns (+1.5%) | 21.66 ns -> 22.30 ns (+2.9%) |
| errno C call | 15.21 ns -> 15.56 ns (+2.3%) | 24.06 ns -> 26.04 ns (+8.2%) |
| libm C call | 17.33 ns -> 17.32 ns (~) | 26.92 ns -> 27.33 ns (+1.5%) |
| blocking C handoff | 24.57 us -> 25.46 us (~) | 93.17 us -> 96.48 us (~) |
| three concurrent blocking C calls | 112.7 us -> 112.9 us (~) | 965.1 us -> 950.3 us (~) |
| eight concurrent blocking C calls | capacity-limited -> 166.6 us | capacity-limited -> 1.203 ms |

The eight-call result is a newly supported capability, not a numerical
speedup: the parent preflight skips timing because it cannot keep all eight
calls blocked while retaining a release executor. The change measured
5.654 us of entry time per call on Darwin and 25.62 us under translated Linux.
It used 705-706 B and 32 allocations per eight-call operation. Three-call and
handoff allocations were unchanged, and ordinary direct C calls remained
allocation-free.

The small short-call cost is fixed rather than proportional to blocking
duration. A direct C return now checks the current logical executor state after
`exitsyscall`; the measured absolute difference was about 0.2-2 ns. This keeps
the no-child entry path compact while making the blocking count exact through
P reacquisition.

The native-context pool test holds 16 simultaneous roots, verifies growth, and
then verifies that a second batch reuses the same high-water capacity. The
eight-call runtime test also verifies exact blocking counts and return-state
cleanup. The experiment's per-G extension remains one pointer. Capacity does
not yet retire within a live root, and the global native-context pool retains
its high-water allocation for later roots. Multi-P logical scheduling remains
a separate limitation.
