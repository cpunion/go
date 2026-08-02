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

Fetch the upstream development branch, then build one GOROOT at its exact
merge-base with the coroutine revision and another at the coroutine revision
under test. The runner resolves `origin/master` by default and verifies the
first revision against that merge-base. Set `UPSTREAM_REF` when the upstream
development branch has a different local name:

```
git fetch origin master

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

### Multi-P runnable scheduling

Revision `f0f5a48057` admits the existing warm replacement executors when an
ordinary spawn makes new logical work runnable and more than one P is
available. Its exact parent is `f967a81bbe`. Previously, replacements were
prepared at root startup but only blocking C entry could wake them, so
ordinary coroutine work remained serial even with `GOMAXPROCS` greater than
one. The single-P path does not perform this wakeup; blocking foreign calls
continue to use the independent capacity mechanism described above.

The compiler-level progress probe uses only ordinary Go source and automatic
coloring. One parent continuation remains active while three spawned workers
must all start. The exact parent consistently times out after five seconds;
the change passes both native Darwin and translated Linux runs, including 20
repetitions. The runtime regression additionally holds three child resume
calls active at the same time.

The fixed-work comparison used prebuilt test binaries, three unrecorded
warm-ups, 12 rotating-order samples, and a 500ms benchmark time. The official
column is the exact upstream merge-base, `3e6eb83f95`; parent and change use
the exact revisions above. Each time is the median of the per-operation
samples.

| Platform and Ps | Official | Parent | Change | Change versus parent |
| --- | ---: | ---: | ---: | ---: |
| Darwin arm64, P=1 | 667.164 us | 647.149 us | 640.398 us | -1.04% |
| Darwin arm64, P=2 | 358.194 us | 619.427 us | 350.350 us | -43.44% |
| Darwin arm64, P=4 | 237.281 us | 612.583 us | 239.572 us | -60.89% |
| Linux amd64, P=1 | 725.920 us | 735.046 us | 722.989 us | -1.64% |
| Linux amd64, P=2 | 424.641 us | 716.091 us | 442.387 us | -38.22% |
| Linux amd64, P=4 | 311.563 us | 732.977 us | 326.100 us | -55.51% |

On Darwin, official Go scaled by 2.81x and the change by 2.67x from one to
four Ps; the parent scaled by 1.06x. Under translated Linux, the corresponding
figures were 2.33x, 2.22x, and 1.00x. The change was within -4.01% to +0.97%
of official Go on Darwin and -0.40% to +4.67% under translated Linux. The
Linux values validate direction and broad magnitude only.

Single-P controls showed no regression. The following entries are exact
parent followed by change medians:

| Probe | Darwin arm64 | Linux amd64 |
| --- | ---: | ---: |
| sequential task creation | 185.5 ns -> 183.6 ns (-1.0%) | 462.1 ns -> 441.4 ns (-4.5%) |
| burst creation of 100 tasks | 11.812 us -> 11.484 us (-2.8%) | 31.606 us -> 28.904 us (-8.6%) |
| park and wake 100 tasks | 31.846 us -> 31.214 us (-2.0%) | 78.320 us -> 72.635 us (-7.3%) |
| fixed-work batch | 547.558 us -> 546.358 us (-0.2%) | 861.381 us -> 821.332 us (-4.7%) |

Allocation counts were unchanged: sequential creation used 68 B and three
allocations per task, burst creation used 6800 B and 300 allocations, the
park/wake probe used 37,312 B and 501 allocations, and the fixed-work batch
used 6400 B and 256 allocations.

Runnable work currently uses at most the four warm logical executors. It does
not yet grow beyond that warm capacity, retire idle executors, steal logical
work through the Go scheduler, or provide scheduler-integrated fairness.
Those remain separate tasks from the dynamic capacity used for blocking C
calls.

### Logical task reuse

Revision `ef630a4be2` retains up to 256 completed non-root task headers in
each live logical scheduler. Its exact parent is `f4830f567a`; that revision
contains the same concurrent-safe runtime benchmarks, so the comparison does
not mix the benchmark repair with the runtime change. Race builds do not reuse
task addresses because the race detector also uses each address as a logical
synchronization identity.

The runtime-level comparison used prebuilt test binaries, three unrecorded
warm-ups, and alternating parent/change order. The Darwin samples used a
500ms benchmark time and the translated Linux samples used 300ms. Each entry
is the median exact parent followed by the median change; the percentage is
the median of the paired differences.

| Probe | Darwin arm64 | Linux amd64 |
| --- | ---: | ---: |
| spawn one task | 75.52 ns -> 65.84 ns (-12.8%) | 179.4 ns -> 163.9 ns (-11.8%) |
| await one task | 48.68 ns -> 40.84 ns (-16.1%) | 153.0 ns -> 127.3 ns (-12.1%) |
| allocation per task | 48 B, 1 alloc -> 0 B, 0 allocs | 48 B, 1 alloc -> 0 B, 0 allocs |

The ordinary-Go probes were compiled from identical source with automatic
coloring. They used three warm-ups and 15 alternating 300ms samples per
probe. The task header accounts for exactly 48 B and one allocation in every
result:

| Probe | Darwin arm64 | Linux amd64 |
| --- | ---: | ---: |
| sequential task | 182.9 ns -> 144.2 ns (-20.8%) | 391.2 ns -> 304.6 ns (-18.6%) |
| burst of 100 tasks | 11.671 us -> 10.155 us (-13.6%) | 26.620 us -> 20.338 us (-23.2%) |
| park and wake 100 tasks | 30.442 us -> 30.523 us (+0.6%) | 61.563 us -> 67.978 us (+5.4%) |
| sequential allocation | 68 B, 3 allocs -> 20 B, 2 allocs | 68 B, 3 allocs -> 20 B, 2 allocs |
| burst allocation | 6800 B, 300 allocs -> 2000 B, 200 allocs | 6800 B, 300 allocs -> 2000 B, 200 allocs |
| park/wake allocation | 37,312 B, 501 allocs -> 32,512 B, 401 allocs | 37,312 B, 501 allocs -> 32,512 B, 401 allocs |

A separate 15-pair, 500ms translated-Linux control measured the park/wake
difference at +7.8%, while the native Darwin result remained neutral. CPU
profiles attributed 3.7% of the change profile cumulatively to the task-cache
helper; larger sampled differences appeared in GC write barriers and the
channel path. The result is retained as a visible follow-up target instead of
being classified as a general native regression. The remaining 20 B and two
allocations per sequential task belong to the compiler-generated frame and
closure; channel operations dominate the park/wake allocation total.

### Logical operation reuse

Revision `57d68dbee3` retains up to 64 completed operation headers in each
live logical scheduler. Its exact parent is `a39f1317e7`. Completion clears
the entire union-shaped header before caching it, so timer, channel, select,
file, socket, and asynchronous-call operations can safely reuse one another.
The existing operation registry still uses a monotonically increasing ID;
late timer callbacks therefore cannot find a header that has been reused for
a different operation.

Normal completion, operation panic, and recycling use the scheduler's existing
lock. Allocation on a cache miss remains outside that lock. Race builds do not
reuse operation addresses because the channel race hooks use the address as a
synchronization identity. The tests exercise reuse across timer, channel, and
read-call kinds, the 64-entry bound with 65 simultaneously pending timers,
panic and every ordinary completion source, registry cleanup, and the
race-build no-reuse rule.

The comparison used prebuilt test binaries, three unrecorded warm-up pairs,
and 15 recorded samples with alternating parent/change order. Darwin used a
500ms benchmark time. Translated Linux used 300ms except for the timer rows:
the 300ms timer samples varied from about 12 to 44 us and gave contradictory
runtime and end-to-end directions, so those two rows were repeated in
isolation with a 1s benchmark time. Each entry is the median exact parent
followed by the median change; the percentage is the median of the 15 paired
differences. Linux values validate direction and broad magnitude rather than
native absolute time.

The direct runtime operation probes produced:

| Probe | Darwin arm64 | Linux amd64 | Allocation per iteration |
| --- | ---: | ---: | ---: |
| positive timer | 8.288 us -> 8.132 us (-2.06%) | 31.452 us -> 28.592 us (-12.25%) | 296 B, 3 allocs -> 104 B, 2 allocs |
| channel | 309.9 ns -> 237.6 ns (-23.03%) | 639.2 ns -> 217.7 ns (-65.79%) | 496 B, 3 allocs -> 112 B, 1 alloc |
| ready file read | 9.004 us -> 8.939 us (paired +0.37%, ~) | 23.094 us -> 24.691 us (paired +3.36%, ~) | 192 B, 1 alloc -> 0 B, 0 allocs |
| ready socket read | 10.723 us -> 10.229 us (-3.97%) | 29.047 us -> 29.033 us (paired +0.73%, ~) | 192 B, 1 alloc -> 0 B, 0 allocs |

The ordinary-Go probes were compiled from identical source with automatic
coloring:

| Probe | Darwin arm64 | Linux amd64 | Allocation per iteration |
| --- | ---: | ---: | ---: |
| channel round trip | 627.7 ns -> 461.7 ns (-26.55%) | 1.310 us -> 463.6 ns (-64.13%) | 992 B, 6 allocs -> 224 B, 2 allocs |
| ready `select` | 244.5 ns -> 190.1 ns (-22.19%) | 574.4 ns -> 348.8 ns (-39.61%) | 516 B, 5 allocs -> 132 B, 3 allocs |
| `Sleep(1ns)` | 8.130 us -> 8.112 us (-1.68%) | 32.446 us -> 29.043 us (-9.79%) | 296 B, 3 allocs -> 104 B, 2 allocs |
| ready file read | 8.687 us -> 8.547 us (-1.08%) | 22.537 us -> 23.415 us (paired +0.12%, ~) | 256 B, 2 allocs -> 64 B, 1 alloc |
| ready TCP read | 8.730 us -> 8.719 us (paired +0.06%, ~) | 23.523 us -> 23.133 us (-5.08%) | 256 B, 2 allocs -> 64 B, 1 alloc |

The existing 100-task simultaneous park/wake control measured 98.159 us ->
93.703 us (-3.89%) on Darwin and 70.137 us -> 58.128 us (-13.67%) under
translated Linux. Darwin allocation fell from 32,357 B and 399 allocations to
19,978 B and 335 allocations; Linux fell from 32,500 B and 400 allocations to
20,215 B and 336 allocations. The exact reduction of 64 allocations and about
12 KiB per iteration demonstrates the cache bound: 64 of the 100 concurrent
operation headers are reused and the rest remain collectable.

An operation header occupies the 192-byte allocation class. The direct channel
probe starts one send and one receive per iteration, which explains its 384 B
and two-allocation reduction. The ordinary-Go ping/pong probe starts four
channel operations and therefore saves 768 B and four allocations. Its exact
parent already includes logical task reuse. File and socket runtime probes now
allocate nothing for the operation itself. Their ordinary-Go probes retain 64
B and one allocation for the compiler-generated frame or closure. The
remaining channel allocation is its stackless waiter; ready `select` and
positive timers similarly expose their non-header costs as the next focused
optimization targets.

### Exact-upstream architecture checkpoint

The 2026-08-02 checkpoint compares the upstream merge-base
`5d29d80b6c` with coroutine revision `efe6bee4fb`. Its only change from
`90ba34fa38` is a compiler stress test, so the measured compiler and runtime
sources are identical. The runner
discarded three warm-up pairs and collected ten alternating-order 500ms
samples from identical source with inlining disabled. The 10,000-task
footprint used six independent processes, and the scaling probe used ten
samples at each processor count.

Darwin ran natively on an Apple M4 Max. Linux amd64 ran under OrbStack
translation, so its values validate direction and broad magnitude rather
than native absolute time. Each entry below is the median exact upstream
baseline followed by the median coroutine result. A tilde marks a difference
that was not statistically significant in this run.

| Probe | Darwin arm64 | Linux amd64 |
| --- | ---: | ---: |
| amortized `Gosched` | 50.61 ns -> 20.61 ns (-59%) | 56.51 ns -> 32.49 ns (-43%) |
| one public coroutine entry | 54.98 ns -> 1.330 us (24x) | 60.53 ns -> 1.105 us (18x) |
| recursive yield, depth 4096 | 22.52 us -> 627.73 us (28x) | 11.16 us -> 933.47 us (84x) |
| defer across a yield | 56.41 ns -> 1.497 us (27x) | 63.77 ns -> 1.144 us (18x) |
| panic/recover across a yield | 190.6 ns -> 1.529 us (8.0x) | 229.2 ns -> 1.351 us (5.9x) |
| park and wake 100 tasks | 18.85 us -> 33.73 us (1.8x) | 21.66 us -> 45.31 us (2.1x) |
| channel round trip | 179.1 ns -> 318.9 ns (1.8x) | 228.7 ns -> 450.6 ns (2.0x) |
| ready `select` | 44.78 ns -> 395.55 ns (8.8x) | 59.23 ns -> 473.30 ns (8.0x) |
| `Sleep(0)` | 1.210 ns -> 4.019 ns (3.3x) | 1.431 ns -> 6.955 ns (4.9x) |
| `Sleep(1ns)` | 137.4 ns -> 5.889 us (43x) | 334.0 ns -> 26.109 us (78x) |
| ready file read | 363.7 ns -> 6.026 us (17x) | 181.6 ns -> 27.446 us (151x) |
| ready TCP read | 312.1 ns -> 6.264 us (20x) | 212.8 ns -> 27.690 us (130x) |
| blocking file read and sibling release | 1.874 us -> 8.427 us (4.5x) | 1.569 us -> 29.706 us (19x) |
| blocking TCP read and sibling release | 11.11 us -> 14.78 us (+33%) | 2.804 us -> 35.806 us (13x) |
| park and wake 10,000 tasks | 10.84 ms -> 754.09 ms (70x) | 10.59 ms -> 460.53 ms (43x) |

Fixed-work scheduling remained statistically level with upstream on native
Darwin at one, two, and four Ps. Translated Linux was level at one and four
Ps and 16% faster at two Ps. The experiment therefore preserves the earlier
multi-P repair while the single-task and operation paths expose the remaining
fixed costs.

The direct foreign-call path also preserved its intended advantage:

| Probe | Darwin arm64 | Linux amd64 |
| --- | ---: | ---: |
| scalar C call | 20.97 ns -> 17.11 ns (-18%) | 28.90 ns -> 22.30 ns (-23%) |
| aggregate, errno, and libm C calls | ~ | -21% to -26% |
| blocking C handoff | 60.18 us -> 27.60 us (-54%) | 1.924 ms -> 96.77 us (-95%) |
| three concurrent blocking C calls | 223.3 us -> 104.0 us (-53%) | 6.742 ms -> 971.2 us (-86%) |
| eight concurrent blocking C calls | 524.6 us -> 163.0 us (-69%) | 16.752 ms -> 1.219 ms (-93%) |

The translated environment amplifies the upstream cgo and syscall handoff
costs. Entry time, measured separately from the blocking interval, was about
89% lower on Darwin and 99% lower under translated Linux. Ordinary direct C
calls remained allocation-free.

Allocation counts explain much of the largest negative differences. A public
coroutine entry used 584 B and eight allocations. Depth-4096 recursion used
464.6 KiB and 16,393 allocations. A 100-task park/wake round used 20,225 B
and 337 allocations, compared with 112 B and one allocation upstream. The
remaining steady-state operation costs were 224 B and two allocations for a
channel round trip, 132 B and three allocations for ready `select`, 104 B and
two allocations for a positive timer, 64 B and one allocation for a ready
file or TCP read, and 632 B and 18 allocations for a blocking read.

The 10,000-task space result retained the experiment's central advantage:
live heap fell from 623.1 B to 372.6 B per task and live stack fell from about
2048 B to 6.554 B per task. Live objects rose from 2.002 to 4.253 per task.
The next performance work should therefore preserve the space and direct-C
results while addressing three independent cost centers:

1. make logical frames and root entry explicit enough to coalesce or recycle
   their typed storage, reducing recursive and high-task-count allocation;
2. connect logical timer and file/network waits directly to runtime timer and
   netpoll readiness instead of paying the current adapter-worker hop; and
3. remove ready-select temporary storage and, only after its ownership model
   is proven under stress, revisit channel waiter lifetime.

### Lazy replacement-executor channels

Revision `6bad5627c8` leaves the two warm replacement-executor channels nil
until a root first spawns independent logical work. Its exact parent is
`246b24eb84`. Roots that only run, yield, await, use timers or I/O, or call C
therefore do not allocate channels that they never use. The first spawn still
prepares all replacement state synchronously before admitting an executor;
the scheduler's capacity and blocking-call paths are otherwise unchanged.

The comparison used prebuilt test binaries, three unrecorded warm-up pairs,
and 15 recorded 500ms samples with alternating parent/change order. The
allocation result was identical on native Darwin arm64 and translated Linux
amd64:

| Probe | Exact parent | Change |
| --- | ---: | ---: |
| one public coroutine entry | 584 B, 8 allocs | 360 B, 6 allocs |
| recursive yield, depth 64 | 8032 B, 265 allocs | 7808 B, 263 allocs |
| recursive yield, depth 4096 | 475,744 B, 16,393 allocs | 475,520 B, 16,391 allocs |
| defer across a yield | 608 B, 9 allocs | 384 B, 7 allocs |
| panic/recover across a yield | 864 B, 11 allocs | 640 B, 9 allocs |

Every root saves exactly 224 B and two allocations. The proportional saving
is largest for shallow roots; recursive frame allocation is independent of
this change.

Translated Linux measured public-entry time at 1.693 us -> 1.564 us
(-7.62%, `p=0.037`). The other four timing differences were not statistically
significant (`p=0.054` to `p=0.775`). Native Darwin timing samples were
collected while unrelated host workloads were active and were too noisy to
classify, so they are deliberately omitted rather than presented as a
speedup. The deterministic allocation reduction is the result this change is
intended to establish.
