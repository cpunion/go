# Coroutine architecture probes

This directory contains portable Go and cgo benchmarks for comparing the
native stackless coroutine experiment with the unmodified Go compiler at its
exact upstream merge-base. The same source directory is compiled by both
toolchains. It imports no private coroutine package and uses no source
annotation to select coroutine functions.

[`PUSH_PULL.md`](PUSH_PULL.md) defines the controlled comparison between the
current exact-task push queue, a Rust-like wake-and-poll driver with identical
task representation, and a compact structured-frame pull model.

These are architecture probes, not a complete language or standard-library
benchmark suite. The first set deliberately prioritizes places where the
implementations differ most:

- amortized scheduler yield and one public coroutine-root entry;
- direct and deeply recursive calls, including allocation cost when logical
  frames replace native stack growth;
- sequential, burst, and parked task creation;
- live heap and stack footprint with 10,000 simultaneously parked tasks;
- ready buffered send/receive pairs, unbuffered channel round trips, and ready
  `select`;
- zero and one-nanosecond sleeps;
- ready one-byte reads through `os.File.Read` and `net.TCPConn.Read`;
- blocking pipe and loopback TCP reads released by a runnable logical sibling;
- fixed-work task batches at `GOMAXPROCS=1,2,4`;
- scalar, aggregate, errno, and libm C calls;
- progress from a blocking C call to an already-runnable sibling at
  `GOMAXPROCS=1`.
- groups of three blocking C calls, plus a self-timed capacity probe with
  eight concurrent calls.

The I/O benchmarks intentionally use the ordinary public APIs. Regular files
use the compensated-syscall boundary, while pollable pipes and TCP connections
borrow their existing poll descriptors; none of these common paths creates a
read closure or uses the worker pool. A concurrent read that cannot acquire the
same descriptor's read lock is the bounded worker fallback. The blocking
probes release each public read with a one-byte raw write syscall that cannot
fill the empty pipe or socket buffer; this keeps the release path out of the
unsupported public write lowering. A preceding `Sleep(0)` keeps the release
worker on the automatically colored path without adding a positive-duration
timer.

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
lowering audit, checks that each ready, blocked, and contended public read
resume calls the library-owned
start/finish boundary without the generic read worker, and checks the direct C
symbols without collecting performance data. Its merged public-read and
`internal/poll` profiles require at least 90% statement coverage for every new
library helper. CI runs the correctness test with both toolchains, invokes
this check, and retains its artifacts. It does not enforce performance
thresholds: hosted-runner measurements are not stable enough for that
purpose.

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

### Embedded root task

Revision `79fc8a8736` stores the root task header directly in its scheduler
instead of allocating a separately owned object. Its exact parent is
`d7b9d12d87`. Non-root tasks retain their existing cache and ownership model;
root completion, panic, Goexit, defer, and race synchronization continue to
use the root header's stable address.

The comparison used prebuilt test binaries, three unrecorded warm-up pairs,
and 15 recorded 500ms samples with alternating parent/change order. Native
Darwin arm64 and translated Linux amd64 produced the same allocation result:

| Probe | Exact parent | Change |
| --- | ---: | ---: |
| one public coroutine entry | 360 B, 6 allocs | 360 B, 5 allocs |
| recursive yield, depth 64 | 7808 B, 263 allocs | 7808 B, 262 allocs |
| recursive yield, depth 4096 | 475,520 B, 16,391 allocs | 475,520 B, 16,390 allocs |
| defer across a yield | 384 B, 7 allocs | 384 B, 6 allocs |
| panic/recover across a yield | 640 B, 9 allocs | 640 B, 8 allocs |

The scheduler's larger allocation accounts for the former root bytes, so
aggregate bytes per root are unchanged while every root uses one fewer heap
object. The checked-in runtime entry benchmark measures the resulting
scheduler and wake channel at 304 B and two allocations per root.

Darwin timing differences were not statistically significant (`p=0.395` to
`p=1.000`). Translated Linux measured public entry at 997.8 ns -> 968.9 ns
(-2.90%, `p=0.001`), defer at 1.044 us -> 1.010 us (-3.26%, `p=0.001`), and
recover at 1.242 us -> 1.219 us (-1.85%, `p=0.008`); both recursive probes
were neutral. These small translated-Linux changes are retained as supporting
data. The cross-platform conclusion is the exact one-object reduction, not a
general timing claim.

### Latest-upstream architecture checkpoint

The 2026-08-12 checkpoint follows the repository's merge-based maintenance
model. It compares Go development revision `9549c91031` with the coroutine
tree after merging that exact revision at `d64e2793c2`. The source probes are
identical, inlining is disabled, and `run.bash` verified the official revision
as the candidate's exact upstream merge-base. It discarded three warm-up
pairs, alternated execution order over ten 500 ms samples, used ten samples
at each processor count, and measured the 10,000-task footprint in six
independent processes.

Darwin ran natively on an Apple M4 Max. The host had unrelated concurrent
load, which widened several official-baseline distributions; the table is an
architecture-scale checkpoint rather than a release threshold. Linux/amd64
ran under VirtualApple translation and had still wider scheduling variance.
Its timings validate direction and expose sensitivity to executor or worker
hops, but are not native x86 performance results. Each entry is the median
official result followed by the median coroutine result. A tilde marks a
difference that was not statistically significant.

| Probe | Darwin arm64 | Translated Linux amd64 |
| --- | ---: | ---: |
| amortized logical yield | 125.80 ns -> 46.43 ns (-63%) | 227.7 ns -> 188.0 ns (~) |
| one public coroutine entry | 127.5 ns -> 2.630 us (20.6x) | 356.9 ns -> 5.900 us (16.5x) |
| recursive yield, depth 4,096 | 43.81 us -> 1.235 ms (28.2x) | 36.85 us -> 6.584 ms (179x) |
| park and wake 100 tasks | 49.90 us -> 71.69 us (1.44x) | 83.00 us -> 328.95 us (3.96x) |
| channel round trip | 495.9 ns -> 718.0 ns (1.45x) | 994.9 ns -> 4.930 us (4.96x) |
| ready `select` | 99.72 ns -> 687.20 ns (6.89x) | 312.6 ns -> 3.325 us (10.6x) |
| `Sleep(1ns)` adapter | 277.2 ns -> 16.633 us (60.0x) | 649.1 ns -> 411.07 us (633x) |
| ready file-read adapter | 748.0 ns -> 21.642 us (28.9x) | 596.3 ns -> 521.47 us (874x) |
| ready TCP-read adapter | 662.6 ns -> 20.756 us (31.3x) | 917.1 ns -> 542.13 us (591x) |
| blocking file read and sibling release | 4.154 us -> 30.979 us (7.46x) | 9.281 us -> 765.94 us (82.5x) |
| blocking TCP read and sibling release | 27.86 us -> 60.99 us (2.19x) | 22.24 us -> 786.05 us (35.3x) |
| scalar C call | 44.34 ns -> 35.54 ns (-20%) | 235.8 ns -> 224.1 ns (~) |
| three concurrent blocking C calls | 413.4 us -> 236.8 us (-43%) | 10.949 ms -> 5.353 ms (-51%) |
| eight concurrent blocking C calls | 884.8 us -> 546.3 us (-38%) | 27.80 ms -> 16.12 ms (-42%) |
| park and wake 10,000 tasks | 16.02 ms -> 831.56 ms (51.9x) | 21.77 ms -> 833.09 ms (38.3x) |

The separate entry metric for the blocking-C groups improved by 79% and 72%
for three and eight calls on Darwin, and by 84% and 75% under translated
Linux. Ordinary scalar, aggregate, errno, and libm direct calls remained
allocation-free. Aggregate, errno, and libm timing was neutral on Darwin;
all four short-call timings were neutral in the translated environment. The
translated environment amplifies the ordinary cgo handoff, so the native
Darwin result is the more representative magnitude.

The fixed-work scheduler remained statistically level with official Go at two
and four Ps on Darwin and at one, two, and four Ps under translated Linux.
Darwin's single-P row measured 4.4% faster, but the official sample was
variable and is not classified as a general speedup. Steady sequential and
burst task probes allocated nothing in either implementation. Their Darwin
times favored the coroutine path in this run while translated Linux was
neutral, so they are likewise retained as controls rather than speed claims.

The deterministic allocation results better isolate the remaining costs:

| Probe | Official Go | Coroutine |
| --- | ---: | ---: |
| one public coroutine entry | 0 B, 0 allocs | 224 B, 3 allocs |
| recursive yield, depth 4,096 | 0 B, 0 allocs | 338,392 B, 1,933 allocs |
| park and wake 100 tasks | 112 B, 1 alloc | about 18.2 KiB, 137 allocs |
| channel round trip | 0 B, 0 allocs | 224 B, 2 allocs |
| ready `select` | 0 B, 0 allocs | 132 B, 3 allocs |
| `Sleep(1ns)` | 0 B, 0 allocs | 0 B, 0 allocs |
| ready file or TCP read | 0 B, 0 allocs | 64 B, 1 alloc |
| blocking file or TCP read | 0 B, 0 allocs | about 596 B, 16 allocs |
| ordinary direct C call | 0 B, 0 allocs | 0 B, 0 allocs |

A separate rate-one allocation profile over 10,000 positive sleeps attributed
one 96-byte allocation per wait to the `new(timer)` call. The remaining
amortized bytes were consistent with boxing the numeric operation identity for
the timer callback argument. Operation-header reuse was already active, so the
next checkpoint changed ownership and identity around the existing runtime
timer heap rather than adding a second timer implementation.

The operation now retains a stable timer owner and passes a generation through
the existing timer callback. An atomic active generation gives either expiry
or cancellation the right to complete the operation; a callback from an older
generation is inert even after cross-kind operation reuse. Generation wrap
retires the owner. This removes both the global operation lookup and steady
timer callback identity boxing.

On native Darwin/arm64, the exact-parent and candidate binaries used three
warm-up pairs followed by 20 alternating 500 ms samples at one P. Positive
sleep allocation fell from 103--104 B and one or two integer-reported
allocations per wait to 0 B and 0 allocations. The source profile and longer
samples approach the expected pre-change steady state of 104 B and two
allocations; shorter samples include the runtime's static small-integer boxes.
Time was statistically neutral: 90.09 us versus 92.68 us (`p=0.883`).

The controlled Linux/amd64 environment used the same sample order and exact
runtime/compiler source pair. It independently reproduced 104 B and two
allocations versus 0 B and 0 allocations. Time was again neutral: 39.42 us
versus 39.32 us (`p=0.841`).

A process-cold, single-iteration control measured 1,888 B and 12 allocations
before versus 1,920 B and 12 allocations after. The reusable owner therefore
adds 32 bytes to the first operation's retained timer object without adding an
allocation; subsequent positive waits on the cached operation allocate
nothing.

The corresponding rate-one ready-file profile attributed its single 64-byte
allocation per read to the compiler-generated call closure at the public
`os.File.Read` site. The operation header is already reused, after which the
closure still crosses the shared worker queue. This isolates the public
lowering boundary and worker handoff, rather than the operation cache, as the
file and socket target.

The next runtime-only checkpoint removes that worker path from the explicit
`runtime/coro.FileRead` companion. Its syscall remains on the calling M while
compensated syscall entry lets a replacement executor run logical siblings.
It completes through the common early-ready protocol without registering the
operation, and it does not count the syscall as a cgo call. The ordinary
`os.File.Read` benchmark and its 64-byte closure allocation above are unchanged;
connecting that public source call is the next compiler/library boundary.

The dedicated runtime adapter benchmark used three warm-up pairs followed by
20 alternating 500 ms samples at one P. On native Darwin/arm64 it changed from
28.14 us to 748.4 ns (-97.34%, `p<0.001`). Both exact binaries reported 0 B
and 0 allocations per read. The controlled translated Linux/amd64 run changed
128.19 us to 561.6 ns (-99.56%, `p<0.001`). All Linux samples reported zero
allocations; parent process setup amortized to 0--6 B/op, while every candidate
sample reported 0 B/op. Because that environment identifies its CPU as
VirtualApple, the timing is directional rather than a native x86 result. Tests
additionally hold a file syscall blocked, run a sibling and a stop-the-world
GC on replacement capacity, and only then release the read.

Darwin controls for entry, yield, timer, channel, and socket read were all
statistically neutral across the same 20 alternating samples; the timing
geomean changed by -1.05%, and every allocation result matched exactly.

For 10,000 simultaneously parked tasks, live heap fell from 623.1 B to
356.6 B per task and live stack fell from 2,048 B to 6.554 B per task. Live
objects rose from 2.002 to 3.253 per task, and the operation performed 40,040
allocations instead of 20,020. This preserves the experiment's central space
advantage while making the current park/wake time and object cost explicit.

Compared with the 2026-08-02 exact-upstream checkpoint, subsequent bounded
reuse and saturated-lineage work reduced public-entry allocation from 584 B
and eight allocations to 224 B and three allocations. Depth-4,096 recursion
fell from about 464.6 KiB and 16,393 allocations to 330.5 KiB and 1,933
allocations. The 100-task park/wake probe fell from 20,225 B and 337
allocations to about 18.2 KiB and 137 allocations. Live objects per parked
task fell from 4.253 to 3.253. Timing is not compared across those two host
runs because their load and upstream revisions differ.

#### Disabled-experiment control

A third build compiled the merged tree with `GOEXPERIMENT=nocoro` and compared
it with the official toolchain. The control used prebuilt binaries, three
warm-up pairs, ten alternating-order 500 ms samples, the same processor-count
matrix, and the same six-process footprint measurement. On each platform, 27
of the 28 timing rows were statistically neutral. The lone fixed-layout
difference was not the same operation: aggregate C calls measured 31% slower
on Darwin (`p=0.007`), while depth-4,096 recursion measured 106% slower
under translated Linux (`p=0.035`).

Those two rows were repeated across linked layouts. Twelve matched Darwin
`-randlayout` seeds made the aggregate C result neutral (`p=0.671`), and ten
matched translated-Linux seeds made the recursion result neutral (`p=0.739`).
All operation allocation counts matched, as did live heap and object counts;
live stack was also statistically level. The different fixed-layout outliers
and neutral layout controls provide no evidence of a systematic regression
when the experiment is disabled.

#### Public read library boundary

Revision `98a0445e59` connects ordinary `os.File.Read` and
`net.TCPConn.Read` calls to library-owned start and finish methods. Its exact
parent is `7b768d219c`, and official revision `9549c91031` is the merge base.
The native Darwin/arm64 comparison used separate binaries built from the same
probe sources, three warm-up rounds, and 12 Latin-square-ordered 500 ms
samples at one P. The Apple M4 Max host had substantial unrelated load; the
wide timing distributions are retained in the statistical comparison rather
than treated as release thresholds.

| Probe | Exact parent -> candidate | Official -> candidate | Parent allocation -> candidate |
| --- | ---: | ---: | ---: |
| ready file read | 90.758 us -> 3.035 us (-96.66%, `p<0.001`) | 1.985 us -> 3.035 us (`p=0.114`, neutral) | 64 B, 1 alloc -> 0 B, 0 allocs |
| ready TCP read | 105.911 us -> 2.915 us (-97.25%, `p<0.001`) | 2.223 us -> 2.915 us (`p=0.291`, neutral) | 64 B, 1 alloc -> 0 B, 0 allocs |
| blocking file read and sibling release | 162.4 us -> 195.2 us (`p=0.347`, neutral) | 12.05 us -> 195.17 us (+1,520%, `p<0.001`) | 596 B, 16 allocs -> 584 B, 16 allocs |
| blocking TCP read and sibling release | 444.5 us -> 372.5 us (`p=0.713`, neutral) | 178.0 us -> 372.5 us (+109%, `p=0.024`) | 596 B, 16 allocs -> 584 B, 16 allocs |

The ready paths therefore remove the worker closure and recover both
zero-allocation behavior and timing statistically indistinguishable from the
official implementation on this host. All blocking samples completed without
a watchdog timeout, and neither blocking result regressed statistically from
the exact parent. The remaining gap to official Go is the coroutine scheduler
handoff and logical-operation cost around a genuinely blocking call, not this
library boundary.

#### Channel waiter cache reuse

This checkpoint retains one cleared channel or select waiter on each cached
stackless operation. Its exact parent is the merged public-read revision
`0532f91256`. Completion clears all operation and queue references before
reuse. Select completion performs that cleanup while its ordered channel locks
are held, drops those locks, and then retains one waiter; additional select
waiters become garbage with the selection descriptor. The ordinary per-P
`sudog` cache remains reserved for goroutine-owned waiters. The retained
pointer remains a GC root while active; additional select waiters are rooted
in the selection descriptor before allocation can reach a safe point.

Fixed-count, one-P runs of 10,000 operations give the following deterministic
allocation result. The Linux file-byte difference is benchmark rounding: the
object-count reduction agrees on both targets.

| Probe | Darwin/arm64 parent -> candidate | Linux/amd64 translated parent -> candidate |
| --- | ---: | ---: |
| channel round trip | 224 B, 2 allocs -> 0 B, 0 allocs | 224 B, 2 allocs -> 0 B, 0 allocs |
| ready select | 132 B, 3 allocs -> 132 B, 3 allocs | 132 B, 3 allocs -> 132 B, 3 allocs |
| blocking file and release | 584 B, 16 allocs -> 360 B, 14 allocs | 585 B, 16 allocs -> 361 B, 14 allocs |
| blocking TCP and release | 584 B, 16 allocs -> 360 B, 14 allocs | 584 B, 16 allocs -> 360 B, 14 allocs |

Twenty alternating 300 ms Darwin/arm64 samples were also collected on the
loaded Apple M4 Max host. Channel round-trip medians were 1,606.5 ns for the
parent and 1,346 ns for the candidate; the candidate won 15 of 20 pairs, with
a paired median change of -16.96% and an exploratory sign-test `p=0.041`.
Ready select and both blocking probes split their pairs 10 to 10. Their timing
is therefore treated as neutral. The stable allocation counts, rather than
these host-sensitive timings, are the checkpoint's performance result.

A 10,000-operation allocation profile attributes all remaining blocking-file
objects to the generated child factories: eight objects per read task and six
per release task. No channel waiter remains in the allocation root. That makes
compiler frame and captured-cell fusion the next independent allocation
target.

#### Explicit frames for timer and I/O transitions

Revision `f50e5d25e6` admits timer, file, and poll transition sites to the
existing explicit typed-frame factory. The change uses the existing factory
ABI and runtime frame cache; functions with spawn or foreign transitions,
source closures, channel ranges, defer, or terminal behavior still use the
conservative closure-backed path.

The exact parent is `d259cdcf7f`, the final revision merged by pull request 73.
Fixed-count, one-P runs of 10,000 lowered operations produced identical object
counts on native Darwin/arm64 and translated Linux/amd64:

| Probe | Parent | Explicit I/O frame |
| --- | ---: | ---: |
| positive timer | 0 B, 0 allocs | 0 B, 0 allocs |
| ready file read | 0 B, 0 allocs | 0 B, 0 allocs |
| ready TCP read | 0 B, 0 allocs | 0 B, 0 allocs |
| blocking file and release | 360 B, 14 allocs | 104 B, 2 allocs |
| blocking TCP and release | 360 B, 14 allocs | 104 B, 2 allocs |

An allocation-rate-one profile assigns one remaining object to
`waitForEpoch.coro` and one to `blockingFileRelease.coro` per round. The read
task itself is now allocation-free. Timing from the loaded Darwin host and
translated Linux is retained only as diagnostic data; the exact twelve-object
reduction is the performance result.

#### Preserve distinct cached frame identities

Revision `d103ba4e1d` prevents a new frame identity from consuming a completed
typed-frame task while the bounded cache still has room. The allocator first
reuses an ordinary free task; if none exists, it adds a cache-owned task rather
than discarding another resume identity. Behavior at the existing 256-task or
32 KiB limit is unchanged, as are the scheduler and task layouts. Race builds
still disable identity reuse.

The exact parent is `cf30715f3e`. Fixed-count, one-P runs of 10,000 lowered
operations matched on native Darwin/arm64 and translated Linux/amd64:

| Probe | Parent | Preserved identities |
| --- | ---: | ---: |
| channel round trip | 0 B, 0 allocs | 0 B, 0 allocs |
| ready select | 132 B, 3 allocs | 132 B, 3 allocs |
| positive timer | 0 B, 0 allocs | 0 B, 0 allocs |
| ready file or TCP read | 0 B, 0 allocs | 0 B, 0 allocs |
| blocking file and release | 104 B, 2 allocs | 0 B, 0 allocs |
| blocking TCP and release | 104 B, 2 allocs | 0 B, 0 allocs |

A rate-one profile shows only cold calibration and setup objects; no factory
allocation scales with the 10,000 operations. Ten alternating 300 ms Darwin
pairs found file timing neutral at 8.626 us versus 8.739 us (`p=0.739`) and
TCP timing neutral at 15.80 us versus 15.33 us (`p=0.280`). The timing geomean
changed by -0.87%. Under checkptr, the file probe similarly changed from
105 B and three allocations to 1 B and one allocation, removing the same
104 B and two target objects.

#### Local readiness before native park

The blocking file and TCP probes use one logical task to make an older read
ready. At exact parent `86706396a0`, the reader still parks its locked native
executor and completes through the platform poller and bridge G. CPU profiles
therefore attribute most of the remaining time to condition wait and wake,
even though the readiness source belongs to the same stackless scheduler.

Revision `dd8f3f3422` adds a bounded cold-path retry before a native executor
parks with an empty logical ready queue. It skips the task that just armed,
scans no more than the 64-operation cache bound, claims only a still-armed
same-scheduler poll read, and makes at most four rotating attempts. An
unavailable read is rearmed, and every miss or race retains the ordinary
netpoll behavior. No scheduler, task, native-context, operation, or compiler
ABI layout changes.

Twenty alternating 500 ms samples at one P produced:

| Probe | Darwin/arm64 parent -> candidate | Linux/amd64 translated parent -> candidate |
| --- | ---: | ---: |
| ready file read | 348.7 ns -> 347.7 ns, neutral | 410.0 ns -> 418.6 ns, neutral |
| ready TCP read | 313.9 ns -> 312.4 ns, neutral | 492.4 ns -> 487.4 ns, neutral |
| blocking file and release | 8.305 us -> 1.266 us (-84.76%) | 52.773 us -> 2.343 us (-95.56%) |
| blocking TCP and release | 15.092 us -> 5.508 us (-63.51%) | 59.358 us -> 4.062 us (-93.16%) |

Every sample reports 0 B and 0 allocations. Ready-file and ready-TCP timing is
statistically neutral on both platforms. The Linux/amd64 results were
collected under VirtualApple translation and are directional. Normal, race,
and `checkptr=2` runtime suites and the complete probe audit pass on both
targets. Focused coverage is 97.9% for `runTasks`, 93.3% for the bounded scan,
92.9% for the cold retry helper, and 100% for token claim.

#### Bounded select storage reuse

A steady ready `select` still used 132 B and three allocations after the
surrounding operation, waiter, and typed-frame caches were warm. Profiles and
disassembly identify a select descriptor, a waiter slice, and lock-order
backing storage; the measured small select's poll order remains on the native
executor stack.

Revision `c37ea928da` retains a cleared descriptor with each scheduler-local
cached operation. Backing arrays for at most 16 cases survive completion;
larger arrays are discarded. Every channel, element, waiter, result, and
arbitration field is cleared before reuse, and tests cover forced GC,
large-select discard, and select-channel-select reuse. The 64-entry operation
cache does not cross public roots and remains disabled under the race
detector. On 64-bit targets the operation shrinks from 184 B to 176 B, making
the maximum net logical retention 16 KiB if all 64 entries have used a small
select. The 32-bit operation remains 104 B.

The exact parent is `7e6d48c590`, with executable runtime revision
`dd8f3f3422`, and the candidate is `c37ea928da`. Twenty alternating 500 ms
samples at one P produced:

| Probe | Darwin/arm64 parent -> candidate | Linux/amd64 translated parent -> candidate |
| --- | ---: | ---: |
| channel round trip control | 269.9 ns -> 268.2 ns, neutral | 477.8 ns -> 490.3 ns, neutral |
| ready select | 292.9 ns -> 170.7 ns (-41.74%) | 518.5 ns -> 309.8 ns (-40.24%) |

Ready select falls from 132 B and three allocations to zero on both targets;
the channel control remains allocation-free. A seven-probe Darwin control run
found positive timers and ready and locally blocking file and TCP reads
statistically neutral with unchanged allocation counts. Translated Linux
timing is directional, while its allocation result and channel control match
Darwin.

An additional paired Darwin comparison measured official Go at 41.99 ns for
ready select and 165.5 ns for channel round trip, versus 156.70 ns and 238.9 ns
for the stackless candidate. All four measurements use zero allocations. The
allocation gap is closed, but the remaining scheduler time is a separate
target.

Normal, race, and `checkptr=2` stackless runtime suites and the architecture
probe pass on both targets; the compiler coroutine suite also passes, and
Linux/386 verifies the 104 B operation layout. Focused coverage is 100% for
select activity, cache validation, clearing, and publication, 94.1% for
preparation, and 88.4% for the complete select start path.

#### Quiescent public-root scheduler reuse

Revision `903b2090f5` retains at most four completed, operation-free public
root schedulers. Release happens only after replacement executors have stopped
and the root result has been transferred. Race builds and nested defer
schedulers remain allocation-backed, and any started logical operation,
detached ready task, reservation, terminal result, or returning executor makes
release fail closed. The complete scheduler is zeroed before reuse, and a new
operation marker fits existing padding so its 64-bit size remains 192 bytes.

The exact executable parent is `c37ea928da`. Three warm-up pairs followed by
20 alternating 500 ms fixed-layout samples at one P produced:

| Probe | Darwin/arm64 parent -> candidate | Linux/amd64 translated parent -> candidate |
| --- | ---: | ---: |
| public yield entry | 1.204 us -> 1.119 us (-7.02%, `p=0.027`) | 920.0 ns -> 832.2 ns (-9.55%, `p<0.001`) |
| allocation | 224 B, 3 allocs -> 32 B, 2 allocs | 224 B, 3 allocs -> 32 B, 2 allocs |

The fixed Darwin binary also made ready file, ready TCP, and scalar C calls
6.50%, 7.47%, and 6.03% slower. The fixed translated-Linux binary made
`YieldBatch` 14.38% slower. Those benchmarks do not execute scheduler release
in their timed steady-state loops, so matched randomized layouts were used to
test whether the movements came from text layout. Twelve Darwin seeds and
eight Linux seeds produced:

| Probe | Darwin/arm64 parent -> candidate | Linux/amd64 translated parent -> candidate |
| --- | ---: | ---: |
| public yield entry | 1.292 us -> 1.211 us, neutral (`p=0.054`) | 920.2 ns -> 835.0 ns (-9.26%, `p<0.001`) |
| `YieldBatch` | 20.63 ns -> 20.31 ns, neutral | 32.27 ns -> 32.46 ns, neutral |
| timing geomean | -0.63% | -1.39% |

Channel round trips, positive timers, ready file and TCP reads, scalar C calls,
and blocking-C handoff were statistically neutral across the matched layouts
on both platforms. Every fixed and randomized sample reproduced 32 bytes and
two allocations instead of 224 bytes and three allocations. The Linux host
reports a VirtualApple CPU, so its absolute timings are directional; its exact
allocation result independently matches Darwin.

Normal, race, and `checkptr=2` runtime tests and the complete probe audit pass
on both targets, as do the compiler and library suites. FreeBSD and Windows
amd64 cross-compilation and `GOEXPERIMENT=nocoro` gates pass. Focused coverage
is 100% for acquisition, finish, and fail-closed release. The shared
initializer reports 88.9% only because its pre-existing fatal nil-resume branch
cannot write coverage after process termination; a subprocess test verifies
that branch's behavior.

#### Park idle replacement executors on ordinary goroutines

An executor that replaced a thread blocked in direct C previously remained on
its synthetic fixed native stack after draining the ready queue. Its locked M
then parked through `stoplockedm` and resumed through `startlockedm`. A Darwin
CPU profile attributed nearly all runnable-sibling handoff time to the
resulting `pthread_cond_wait` and `pthread_cond_signal` calls.

Revision `a0248ea149` keeps every logical resume and direct C call on a fixed
native stack, but ends a replacement native activation when its ready queue is
empty. The ordinary caller G waits for the next episode and reenters the fixed
stack after wakeup. The public root activation remains persistent. This needs
neither task capability classification nor source annotations, and the
managed-stack fallback remains unchanged on unsupported and race targets.

Revision `62a3add088` adds a deterministic two-episode native-drain test and
hardens the frame-cache test exposed by the shorter handoff. A child publishes
its terminal state before its executor recycles the typed frame, so cache
inspection now yields until recycling is visible and performs test failures on
the ordinary test goroutine rather than a synthetic executor.

The exact target is `09590b5e0b`; its tree is identical to executable revision
`1e50d7ebe1`. Three warm-up pairs followed by 20 alternating 500 ms Darwin
samples at one P produced:

| Probe | Target | Native-drain candidate | Change |
| --- | ---: | ---: | ---: |
| direct C steady call | 13.15 ns | 13.12 ns | neutral |
| direct C blocking handoff | 2.283 us | 2.377 us | neutral |
| ordinary cgo runnable handoff | 59.99 us | 60.02 us | neutral |
| direct C runnable handoff | 26.54 us | 15.23 us | -42.60% |
| direct C runnable progress | 7.944 us | 5.557 us | -30.04% |

All Darwin samples report 0 B and 0 allocations. The direct runnable changes
have `p<0.001`; the three controls are statistically neutral.

Translated Linux/amd64 used identical fixed counts of 4,096 operations, two
warm-up pairs, and ten alternating samples. Fixed counts avoid a Rosetta
benchmark-calibration busy loop in ordinary cgo:

| Probe | Target | Native-drain candidate | Change |
| --- | ---: | ---: | ---: |
| ordinary cgo runnable handoff | 211.1 us | 213.9 us | neutral (`p=0.052`) |
| direct C runnable handoff | 133.85 us | 42.45 us | -68.29% |
| direct C runnable progress | 30.63 us | 15.20 us | -50.38% |

The Linux direct changes have `p<0.001`, and every sample reports zero
allocations per operation. Absolute translated timings are directional; the
independent result confirms that the native-stack implementation on both
architectures avoids the persistent locked-M wait.

Normal, race, and `checkptr=2` stackless runtime suites, the compiler coroutine
suite, transparent direct-C tests, and the complete timer, file, and TCP probe
audit pass on Darwin/arm64 and Linux/amd64. Full Darwin runtime and `nocoro`
suites pass. The translated Linux runtime suite passes with three
Rosetta-incompatible intentional crash or oversized-mmap tests excluded; the
remaining tests and the separately constrained memory-limit tests pass.
FreeBSD and Windows amd64 cross-compilation also pass. Native and race coverage
profiles together execute every statement introduced in the scheduler and
native-stack paths; focused native coverage is 96.0% for `runTasks`, 80.0% for
`replacementExecutor`, and 90.0% for each native entry helper, with the race
profile covering the managed fallback.

The next independent performance change is to remove the remaining
compiler-owned public-entry frame and result allocations. Its regression gates
remain the direct-C path, multi-P fixed-work behavior, disabled-experiment
control, and live task footprint.

#### Reuse compiler-owned public-root frames

Revision `2a950f17c0` removes the remaining steady-state public-entry
allocations without changing the internal explicit-frame factory ABI. A public
factory first looks up one of at most four quiescent typed frames by exact
resume identity and size. On a miss, under the race detector, or above the
existing 32 KiB frame-cache limit, it retains the ordinary typed allocation.
Internal await and spawn factories continue to use their scheduler-local frame
reservations.

Public result targets now refer to result values inside the typed frame. The
wrapper copies those values only after the root scheduler has finished, clears
pointer-bearing results, and publishes the frame only when scheduler release
proves the old root quiescent. Generated completion still clears every other
pointer field. Child factories distinguish their external result targets from
the public root's self-target, so they retain the existing copy-before-reuse
behavior without a runtime root query or a source annotation.

The exact parent is the `a1bf0645ae` merge of the native-drain checkpoint.
Three warm-up pairs followed by 20 alternating 500 ms fixed-layout samples at
one P produced:

| Probe | Darwin/arm64 parent -> candidate | Linux/amd64 translated parent -> candidate |
| --- | ---: | ---: |
| public yield entry | 986.7 ns -> 982.3 ns (-0.45%) | 1.022 us -> 1.019 us (-0.39%) |
| allocation | 32 B, 2 allocs -> 0 B, 0 allocs | 32 B, 2 allocs -> 0 B, 0 allocs |

The sub-percent entry-time movements are practically neutral; exact
steady-state allocation removal is the result. Five fixed-layout controls had
timing geomeans of
-0.22% on Darwin and +0.34% on translated Linux, with every allocation count
unchanged. The fixed Linux `YieldBatch` control moved +2.29%, but eight matched
randomized linker layouts made it neutral (`p=0.382`). Channel round trips,
ready file and TCP reads, and scalar direct-C calls otherwise remained neutral.
The 64-task multi-P probe was also neutral on both targets.

Five independent 10,000-parked-task samples retained the same steady live
footprint: the stable samples report 340.1 live heap bytes and 3.252 live
objects per task, zero measured live stack bytes, about 3.244 MiB allocated,
and about 40,017 allocations. Disabled-experiment binaries contain no
coroutine resume symbol; public entry, file-read, and scalar-C controls remain
statistically neutral with identical zero-allocation counts.

The architecture check now runs three fixed-count public-entry samples and
requires each to report zero bytes and zero allocations. The compiler
coroutine suite, normal, race, and `checkptr=2` stackless runtime suites, and
the complete architecture audit pass on native Darwin/arm64 and translated
Linux/amd64. Full Darwin runtime suites pass with both `coro` and `nocoro`, and
FreeBSD and Windows amd64 plus Darwin amd64 and Linux arm64 cross-compilation
passes. Compiler coverage is 93.3% overall and 100% for the changed explicit
frame lowering; the new root-frame cache eligibility, lookup, release, and
quiescent scheduler release paths each have 100% focused coverage.

#### Hand off structured child completion directly

Revision `de39ab3163`, based on the exact `3db190a434` merge, removes the
ready-queue round trip between a normally completed structured child and its
waiting parent. If the queue is empty and the parent is fully suspended, the
current executor marks the parent running, recycles the child, and resumes the
parent immediately. An already-runnable sibling retains queue order. A parent
still returning `Wait` retains `readyPending`, and race builds retain the
separate publication and acquisition boundary. Panic, `Goexit`, external
readiness, and blocking-C replacement executors are not part of this fast
path.

The follow-up moved the direct resume branch into the `Complete` action itself,
so unrelated queue iterations do not test a handoff variable. It adds no task
or scheduler fields, changes no allocation count, and needs no compiler or
source annotation. The 64-bit task and scheduler remain 48 and 192 bytes.

On Darwin/arm64, twenty alternating fixed-layout 500 ms samples improved
structured await by 1.45%, 4.93%, 9.02%, 9.33%, and 12.25% at depths 1, 8,
64, 256, and 4096. The steady structured-await probe improved 13.61%.
Twelve matched randomized linker layouts reproduced a depth-dependent effect
from 2.22% to 12.41% and improved the steady probe 16.94%. Entry, yield,
positive timer, ready and blocked file and socket I/O, and channel controls
were neutral under randomized layouts. All byte and allocation counts were
identical.

Translated Linux/amd64 fixed layouts improved the less noisy depth-64,
depth-256, depth-4096, and steady probes by 12.86%, 10.56%, 15.09%, and
12.00%. Eight matched randomized layouts reproduced improvements through
11.34% at depth 4096 and 8.94% for steady await; entry, yield, and every I/O
control remained neutral. The translated timing is directional, while its
unchanged allocation results independently agree with native Darwin.

Normal, race, and `checkptr=2` stackless runtime suites, the compiler coroutine
suite, and the architecture probe pass on both platforms. Full Darwin runtime
suites pass with both `coro` and `nocoro`; the translated Linux suite passes
with only its three known Rosetta-incompatible tests excluded. FreeBSD and
Windows amd64 plus Darwin amd64 and Linux arm64 cross-compilation pass. The
runtime coverage profile executes every new direct-handoff statement and each
tested fallback; `runTasks` reports 96.2% and `complete` reports 84.8%.

#### Fuse structured self-await frames

Revision `d6a4bac3dc`, based on the exact `f338b13f94` merge, lets one logical
task own a compiler-proven explicit-frame self-await chain. Each generated
frame carries its parent, an optional cache owner, and a compact direct/chunk
marker. The cache prefix remains typed and reusable; after saturation, six
direct frames and exact four-frame chunks replace the former frame-plus-task
allocation pairs. The benchmark recursive frame grows from 40 to 56 bytes,
while the task and scheduler remain 48 and 192 bytes.

An empty ready queue returns a private frame-switch action so the executor can
enter the child or restored parent immediately. A queued sibling retains
priority through the ordinary yield path. Race and pull-comparison builds,
nonmatching callers, closure-frame factories, and self-spawn candidates retain
the old task path. Panic unwinds the parent links and discards cache owners.
The compiler-private factory ABI remains version 3 and no source annotation is
added.

Two warm-up pairs followed by eight alternating 500 ms fixed-layout samples
on native Darwin/arm64 produced:

| Probe | Exact parent | Fused frames | Change |
| --- | ---: | ---: | ---: |
| logical yield control | 19.43 ns | 19.59 ns | neutral |
| public entry control | 1.107 us | 1.162 us | neutral |
| recursive yield, depth 64 | 4.794 us | 3.855 us | -19.59% |
| recursive yield, depth 256 | 15.43 us | 11.47 us | -25.66% |
| recursive yield, depth 262 | 17.47 us | 12.67 us | -27.46% |
| recursive yield, depth 264 | 18.08 us | 12.85 us | -28.91% |
| recursive yield, depth 267 | 18.54 us | 13.29 us | -28.30% |
| recursive yield, depth 4096 | 430.7 us | 260.6 us | -39.49% |

Twelve matched randomized layouts made both controls neutral and reproduced
25.94%, 32.35%, and 42.16% improvements at depths 64, 256, and 4096. The
allocation boundary probes are exact and identical on native Darwin and
translated Linux:

| Depth | Exact parent | Fused frames | Change |
| ---: | ---: | ---: | ---: |
| 262 | 720 B, 12 allocs | 384 B, 6 allocs | -46.67% bytes, -50% allocs |
| 264 | 928 B, 14 allocs | 608 B, 7 allocs | -34.48% bytes, -50% allocs |
| 267 | 1,120 B, 15 allocs | 832 B, 8 allocs | -25.71% bytes, -46.67% allocs |
| 4096 | 338,144 B, 1,930 allocs | 215,200 B, 965 allocs | -36.36% bytes, -50% allocs |

Ten alternating translated Linux/amd64 samples improved depth 4096 from
758.6 us to 489.7 us (-35.45%) and reproduced the exact allocation result.
Smaller timing rows were sensitive to VirtualApple translation and are not
used as release thresholds.

Full `coro` and `nocoro` runtime suites pass on native Darwin/arm64 and native
Linux/arm64. The complete compiler coroutine suite passes on Darwin/arm64 and
Linux/amd64. Normal, race, `checkptr=2`, pull fallback, architecture, and
closure-frame recursive compatibility checks pass on their applicable native
targets. Rosetta repeatedly traps in unrelated Linux/amd64 address-space crash
tests, so only its targeted runtime, compiler, and benchmark results are used.
Linux/386, FreeBSD/amd64, Windows/amd64, and Darwin/amd64 runtime test binaries
cross-compile with the experiment enabled.
Compiler coverage is 93.4% overall and 100% for the changed explicit-frame
lowering. Normal, race, and pull runtime profiles cover every reachable added
line; the remaining raw-profile gaps are fail-closed `throw` bodies, with no
coverage exclusion or test-only production branch.

#### Fuse local heterogeneous structured frames

Revision `c450d8f21f`, measured against exact benchmark parent `cd8f6d8a51`,
extends frame fusion from self recursion to local structured awaits between
different explicit-frame functions. The compiler proves both ends use its
private version-3 factory layout. A heterogeneous child adds its resume entry
to the common parent, owner, and allocation-marker header, so the current
logical task can enter the child and restore its parent without allocating a
second task. Pure self recursion retains its smaller header and original
specialized runtime path.

The optimization is automatic and local to one compilation unit. Cross-package
factories retain the ordinary task path until export data describes the
extended header. Race builds and the tagged pull-comparison driver likewise
retain distinct task identities. There is no source annotation, public ABI
change, or alternate scheduler mode. A completed root may replace a stale,
free typed-frame cache entry left by an earlier deep lineage; active frames are
never evicted. The bounded public-root frame pool also replaces a quiescent old
identity when full, so an earlier workload cannot permanently force a new root
type to allocate. A fixed-count regression first saturates the internal cache
with depth-4,096 self recursion, warms one depth-64 mutual call, and then
requires repeated mutual recursion to allocate nothing.

Two warm-up pairs followed by ten alternating 500 ms samples at one P on
native Darwin/arm64 produced:

| Probe | Exact parent | Heterogeneous fusion | Change |
| --- | ---: | ---: | ---: |
| recursive self yield, depth 64 | 3.307 us | 3.321 us | neutral |
| recursive self yield, depth 4,096 | 231.3 us | 229.4 us | neutral |
| mutual yield, depth 64 | 4.489 us | 3.737 us | -16.75% |
| mutual yield, depth 4,096 | 404.6 us | 298.6 us | -26.21% |
| mutual depth-4,096 allocation | 360.0 KiB, 4,804 allocs | 300.0 KiB, 3,840 allocs | -16.68% bytes, -20.07% allocs |

The mixed six-probe run moved the logical-yield control by 2.56%. A separate
15-pair control made both logical yield (`p=0.690`) and public entry
(`p=0.878`) neutral, while all control allocation counts remained zero. The
result therefore extends the compact structured-frame benefit beneath the
production push scheduler without regressing its established homogeneous
fast path.

#### Exact-upstream performance checkpoint

Revision `ba32b8ecb3` was also compared with an unmodified toolchain built at
the exact merged Go development revision, `41a1646f6d`. This is a comparison
of the complete coroutine experiment, not an attribution to heterogeneous
frame fusion alone. The focused native Darwin/arm64 run used an Apple M4 Max,
disabled inlining, one P, two warm-up pairs, and 12 alternating 500 ms
samples. Host load remained elevated, but the focused rerun greatly narrowed
the earlier full-suite distributions and reproduced every architectural
difference below.

| Probe | Official Go | Coroutine experiment | Change |
| --- | ---: | ---: | ---: |
| logical yield batch | 48.86 ns | 18.89 ns | -61.33% |
| public task entry | 47.21 ns | 1.060 us | +2,145.29% |
| recursive yield, depth 4,096 | 18.68 us | 244.12 us | +1,206.69% |
| mutual yield, depth 4,096 | 18.19 us | 322.75 us | +1,673.91% |
| park 100 tasks | 14.58 us | 29.09 us | +99.57% |
| sleep for one nanosecond | 124.6 ns | 5.766 us | +4,525.35% |
| ready file read | 313.2 ns | 372.4 ns | +18.88% |
| ready TCP read | 270.1 ns | 322.0 ns | +19.24% |
| scalar C call | 17.72 ns | 13.89 ns | -21.59% |
| blocking C handoff | 60.95 us | 15.93 us | -73.86% |
| eight concurrent blocking C calls | 525.0 us | 164.1 us | -68.75% |

The blocking-C progress metric improved from 59.14 us to 5.900 us per
observed scheduler step (-90.02%), and the eight-call entry cost improved
from 58.07 us to 6.008 us per call (-89.65%). The current group-of-eight
path allocates 321 bytes in 24 allocations while the official cgo path
allocates nothing. Blocking file and TCP round trips were directionally
faster in both the full and focused runs, but the coroutine distributions
remained too wide to use those two rows as release thresholds. Ready file and
TCP paths allocate nothing in either toolchain.

Deep stackless calls still pay for explicit heap frames that the official
growable stack avoids. At depth 4,096, homogeneous recursion used 210.2 KiB
in 965 allocations and mutual recursion used 300.0 KiB in 3,840 allocations;
the official benchmark allocated nothing. Conversely, four independent
10,000-task footprint processes found a smaller parked representation:

| Parked-task metric | Official Go | Coroutine experiment | Change |
| --- | ---: | ---: | ---: |
| allocated bytes | 5.942 MiB | 3.248 MiB | -45.34% |
| allocation count | 20.02k | 40.04k | +100.00% |
| live heap bytes per task | 623.1 | 340.6 | -45.34% |
| live objects per task | 2.002 | 3.253 | +62.49% |
| live stack bytes per task | 2,048.0 | 6.554 | -99.68% |
| create and park 10,000 tasks | 6.648 ms | 164.484 ms | +2,374.19% |

The full eight-pair run also kept fixed-work multi-P scaling close to
official Go: 535.4 versus 539.6 us at one P, 290.8 versus 294.3 us at two P,
and 165.6 versus 168.6 us at four P. The remaining priorities are therefore
entry and parking construction cost, timer wakeup, ready-I/O overhead, and
further compact-frame allocation reduction. Transparent C calls and blocking
M replacement already show the intended performance advantage. The
push-versus-pull measurements in `PUSH_PULL.md` separately show that compact
structured ownership, rather than pull polling itself, is the productive way
to reduce the deep-frame and GC costs while retaining push-based goroutine
semantics.

#### Share overflow operation storage

Revision `7b524fc862`, based on exact parent `8a329a4522`, retains the first
64 completed operations in each logical scheduler and shares the remaining
192 entries of the bounded 256-task working set process-wide. The local fast
path and the task, operation, and scheduler layouts on 64-bit targets are
unchanged. A shared leaf-rank mutex is reached only after releasing the
scheduler lock and only on a local-cache miss or overflow. Race builds
continue to give every channel operation a distinct synchronization address
and therefore bypass both caches.

The previous 64-entry bound made a steady burst of 100 parked tasks allocate
36 operations and 36 channel waiters in addition to the release channel: 73
allocations and about 10.5 KiB per iteration. The shared pool retains those
overflow objects for later schedulers without multiplying a 256-entry cache
by the number of schedulers. A direct 15-pair comparison with the alternative
256-entry scheduler-local cache was neutral (-0.83% paired median, `p=1.0`)
and reported one allocation for both implementations.

The parent and candidate binaries below used identical compiler and linker
executables, the same source probes, disabled inlining, one P, the same
randomized linker-layout seed, and 15 alternating 200 ms samples.
Darwin/arm64 ran natively on an Apple M4 Max. Linux/amd64 ran under Rosetta
translation, so its timing is directional while its allocation counts are
exact.

| Probe | Darwin/arm64 parent -> candidate | Linux/amd64 parent -> candidate |
| --- | ---: | ---: |
| park and wake 100 tasks | 26.344 us -> 18.762 us (-28.66%) | 61.731 us -> 42.943 us (-25.85%) |
| allocation | 10,482 B, 73 allocs -> 113 B, 1 alloc | 10,484 B, 73 allocs -> 116 B, 1 alloc |

All 15 pairs improved on both platforms (`p=0.000061`). Eleven fixed-layout
controls covering entry, yield, sequential and burst spawning, channel,
select, timer, file, TCP, and direct and blocking C calls had no statistically
significant regression. Every control retained its previous zero-allocation
result.

Regression tests cover empty, populated, and full shared caches; the exact
257-operation bound; cross-scheduler and cross-operation-kind reuse; normal
completion and panic; and race-mode non-reuse. Normal, race, and `checkptr=2`
stackless runtime tests and the complete timer, file, TCP, lowering, symbol,
allocation, and public-read coverage audit pass on native Darwin/arm64 and
translated Linux/amd64. The complete coroutine compiler suite also passes on
both. The translated full Linux runtime reaches only the existing Rosetta
failure in `TestCheckFDs`; native Linux CI remains the complete runtime gate.

Combined normal and race profiles execute 49 of 53 added runtime statements
(92.45%). The four unexecuted statements are the two fail-closed cache
invariant `throw` bodies; their validator, every state transition, both cache
directions, overflow disposal, panic path, and race path are covered.

#### Resume early operations directly

Revision `d27a9580e1`, based on exact parent `2cef2cc392`, avoids a ready-queue
round trip when an operation completes before its initiating resume call
returns `Wait`. If the current native executor still owns the task and no
logical work competes with it, the scheduler changes the task from `Waiting`
back to `Running` and resumes it in place. A queued task, a completed root, a
managed executor, a race build, or the pull-comparison build retains the
ordinary publication path.

The producer does not signal while the resume is active. The executor either
consumes the pending readiness directly or publishes it and supplies the
deferred signal. A later producer still signals normally. This removes stale
wake tokens without losing the delayed-completion wake. It also preserves
FIFO ordering when another task is ready. Normal and panic operation
completion share the protocol. No runtime object changes size, and the
compiler ABI, lowering, operation sources, and annotation-free source model
are unchanged.

The exact binaries used identical compiler and linker executables, disabled
inlining, one P, and the stable import path
`cmd/compile/testdata/corobench`. Parent and candidate executions alternated.
Darwin/arm64 ran natively on an Apple M4 Max with 12 independent matched
`-randlayout` seeds at 300 ms per probe. Linux/amd64 ran under Rosetta with 16
independent matched layouts at 500 ms per probe, so its timing is directional
while its allocation results are exact.

| Probe | Darwin/arm64 parent -> candidate | Linux/amd64 parent -> candidate |
| --- | ---: | ---: |
| ready select | 137.65 ns -> 114.15 ns (-17.95%, `p=0.0063`) | 236.85 ns -> 201.45 ns (-16.19%, `p<0.001`) |
| ready file read | 340.70 ns -> 331.10 ns (-2.74%, `p=0.0063`) | 248.45 ns -> 229.10 ns (-7.53%, `p=0.0042`) |
| ready TCP read | 306.75 ns -> 299.95 ns (-3.48%, `p=0.146`) | 287.95 ns -> 265.30 ns (-7.88%, `p=0.021`) |

Ready select improved in 11 of 12 Darwin layouts and 15 of 16 Linux layouts;
file improved in 11 of 12 and 14 of 16, and TCP improved in 9 of 12 and 13 of
16. Channel, positive-timer, task, yield, scalar-C, and blocking-C controls
had no statistically significant regression. Every timing probe retained its
parent allocation result; the three operation probes remained at zero bytes
and zero allocations per iteration.

Fifteen alternating 200 ms fixed-layout samples independently reproduced the
target-path medians. Translated Linux also reported a 65.47% fixed-layout
`YieldBatch` slowdown even though `runTasks` started at the same address and
its private-yield hot-loop bytes were identical. Across the 16 matched
layouts that control was neutral (-0.63% median, 8 improvements and 8
regressions, `p=1.0`). The fixed result is therefore retained as a Rosetta
code-address artifact rather than attributed to the handoff.

Regression tests cover direct ownership, managed fallback, queued-task FIFO,
delayed signaling, race fallback, pull comparison, normal and panic channel
and select completion, and concurrent file and socket operation. Normal,
race, and `checkptr=2` stackless runtime tests, the complete compiler
coroutine suite, `go vet runtime`, and the lowering, symbol, allocation, and
public-read audit pass on native Darwin/arm64 and translated Linux/amd64. Full
`coro` and `nocoro` runtime suites pass on Darwin. The translated Linux full
runtime is not a gate because Rosetta traps in unrelated address-space crash
tests; its focused suites pass, and native Linux CI remains the full-runtime
gate. Linux/386, Linux/arm64, FreeBSD/amd64, Windows/amd64, and Darwin/amd64
runtime test binaries also cross-compile with the experiment enabled.

Combined normal, race, and pull profiles report 97.3% coverage for
`runTasks`, 100% for normal operation completion, 88.2% for panic operation
completion, and 89.5% for the wait transition. Every executable production
statement added by this checkpoint is covered; the remaining function-level
gaps are pre-existing fail-closed invariant throws.

#### Defer short positive sleep timers

Revision `ddf031c260`, based on exact parent `d5c809ceec`, avoids a timer
callback and wake-channel round trip when a positive sleep expires while the
native driver dismantles its fixed-stack episode. The optimization applies
only when the current native executor owns the scheduler and no replacement
executor or runnable logical sibling competes with it. The operation retains
the original absolute deadline. A deadline still in the future arms the
existing runtime timer; an elapsed deadline readies the task directly after a
host `Gosched`, so even a one-nanosecond sleep remains a scheduling point.

Spawned work, managed executors, race mode, pull comparison, and platforms
without the native fixed-stack driver retain the ordinary timer path. No
compiler ABI, lowering, source annotation, timer-owner protocol, or shared
scheduler layout changes. The 64-bit scheduler remains 192 bytes, while the
private native context adds one operation pointer. This checkpoint also adds
`BenchmarkSleepMicrosecond` so both the elapsed-during-teardown and
still-future deadline behaviors remain visible.

The exact parent and candidate used the same compiler and linker source and
flags, disabled inlining, one P, and the same source probes. Six independent
matched `-randlayout` seeds each ran one warm-up and two alternating 200 ms
samples, for 12 pairs. Native Darwin/arm64 used an Apple M4 Max. Linux/amd64
ran under Rosetta, so its timing is directional while its functional and
allocation results remain useful.

| Probe | Darwin/arm64 parent -> candidate | Linux/amd64 translated parent -> candidate |
| --- | ---: | ---: |
| sleep for one nanosecond | 218.50 ns -> 139.65 ns (-36.59%) | 317.00 ns -> 186.45 ns (-39.98%) |
| sleep for one microsecond | 3.840 us -> 4.209 us (+3.26%, neutral) | 892.156 us -> 497.440 us (-46.76%) |

Every one-nanosecond pair improved on both platforms (`p=0.000488`). Every
Linux microsecond pair also improved; the Darwin microsecond distribution was
neutral (`p=0.388`). Channel round trip, ready select, ready file and TCP read,
task sequence, public entry, and amortized yield had no statistically
significant regression on either platform. All probes retained zero
allocations per operation. The translated Linux amortized-yield percentage
was layout-noisy, but its pooled medians were effectively equal at 3.800 and
3.798 ns and its sign test was neutral (`p=0.388`).

A separate 12-pair native Darwin comparison used unmodified Go at exact
upstream revision `da7c67f595`. This measures the complete coroutine
experiment rather than attributing every difference to this change:

| Probe | Official Go | Coroutine experiment | Change |
| --- | ---: | ---: | ---: |
| sleep for one nanosecond | 117.30 ns | 142.75 ns | +21.27% |
| sleep for one microsecond | 3.619 us | 4.189 us | +12.33%, neutral (`p=0.146`) |

The one-nanosecond path has moved from about 1.86 times official Go in the
exact parent to about 1.22 times official Go. Its remaining cost is the
fixed-stack episode boundary and host fairness point, not a redundant timer
wake.

Normal, race, pull, pull-plus-race, and `checkptr=2` stackless runtime tests,
the complete coroutine compiler suite, `go vet runtime`, the architecture
probe audit, and the Linux/386 runtime cross-test pass on native Darwin/arm64
and translated Linux/amd64 as applicable. Full `coro` and `nocoro` runtime
suites pass on Darwin. The translated full Linux runtime reaches unrelated
Rosetta address-space crash tests, so native Linux CI remains the complete
runtime gate.

The focused profile reports 100% coverage for the sleep start, absolute
deadline, timer-arm, scheduling, and native deferral helpers and 92.3% for the
native wait path. It executes 57 of 63 added executable lines (90.48%); the
remaining six lines are fail-closed invariant or unreachable bounded-cache
fallback bodies rather than omitted functional branches.

#### Complete ready selects synchronously

Revision `14199affe6`, based on exact parent `7934c61ced`, lets a stackless
coroutine continue in its current resume call when a select is already ready.
The runtime first uses the existing lock-free channel readiness hints. It then
calls the ordinary `selectgo` arbitration with `block=false`, writes the
selected index and receive result into the coroutine frame, clears the case
descriptors so they cannot retain frame pointers, and tells compiler-generated
code whether it must return the wait action. A genuinely blocked select keeps
the original retained-operation path.

The direct path is limited to non-race, non-pull-comparison selects with at
most 16 cases, which keeps the complete arbitration order on the native stack.
Large selects, race instrumentation, pull comparison, and special cases that
cannot be ruled ready by the hint retain the previous protocol. Synctest
bubble channels conservatively enter `selectgo`, where the existing bubble
checks remain authoritative. Closed-send panics still pass through the
existing native-panic machinery. No runtime object layout, source annotation,
or public compiler interface changes.

The exact parent and candidate used identical compiler and linker source,
disabled inlining, one P, one warm-up, alternating execution, and matched
randomized linker layouts. Native Darwin/arm64 used eight independent layouts
at 300 ms per probe. Linux/amd64 used six independent layouts under Rosetta,
so its timings are directional while its functional and allocation results
remain useful.

| Probe | Darwin/arm64 parent -> candidate | Linux/amd64 translated parent -> candidate |
| --- | ---: | ---: |
| ready select | 114.400 ns -> 80.805 ns (-29.37%) | 188.250 ns -> 133.250 ns (-29.22%) |
| blocked select | 133.400 ns -> 139.800 ns (+4.80%) | 237.100 ns -> 247.700 ns (+4.47%) |
| channel round trip | 212.900 ns -> 210.150 ns (-1.29%) | 364.650 ns -> 366.850 ns (+0.60%) |
| sleep for one nanosecond | 139.650 ns -> 139.150 ns (-0.36%) | 189.750 ns -> 188.400 ns (-0.71%) |

Ready select improved in every matched layout on both platforms. All timing
and allocation probes retained their parent zero-allocation results. The
blocked-select row intentionally measures an immediate wake, making the
roughly 6--11 ns lock-free readiness check visible; real external blocking
latency dominates that fixed cost. The hint reduced the earlier full
double-scan implementation's approximately 15% blocked-select regression to
less than 5%. Amortized yield was within 1.76% on native Darwin. Its translated
Linux result was address-layout noisy and is not attributed to this change.

A prior separate 12-sample native Darwin run measured unmodified Go at exact
upstream revision `da7c67f595` at 42.38 ns per ready select. This is directional
whole-experiment context rather than a matched attribution: the candidate is
about 1.91 times official Go, down from about 2.70 times in its exact parent.

Regression tests cover buffered and closed sends and receives, queued peers,
default selection, direct closed-send panic, genuinely blocked and large
select fallback, descriptor clearing, race and pull fallback, and the
readiness hint. Normal, race, pull, pull-plus-race, and `checkptr=2` stackless
runtime tests, the complete coroutine compiler suite, `go vet runtime`, and
the benchmark audit pass on native Darwin/arm64 and translated Linux/amd64 as
applicable. Full `coro` and `nocoro` suites pass on Darwin. Linux/386,
Linux/arm64, FreeBSD/amd64, Windows/amd64, and Darwin/amd64 runtime tests also
cross-compile with the experiment enabled. The translated full Linux runtime
reaches only the existing Rosetta failure in `TestCheckFDs`; native Linux CI
remains the complete runtime gate.

Combined normal, race, and pull profiles report 100% coverage for `coroSelect`,
92.3% for the lock-free readiness helper, 93.3% for direct arbitration, and
100% for retained-select initiation. The remaining gaps are conservative
synctest and fail-closed invariant branches or pre-existing retained-operation
competition paths rather than omitted direct-path behavior.

#### Complete ready channel operations synchronously

Revision `bd2ae512bc`, based on exact parent `8f884de403`, lets a stackless
coroutine continue in its current resume call when a send or receive is already
ready. The compiler now treats both runtime helpers as conditional operations:
it returns the wait action only when the helper reports that the retained
operation protocol was started, and otherwise jumps directly to the next
state.

The runtime direct path calls the ordinary `chansend` and `chanrecv` machinery
with `block=false`. This reuses the standard channel lock, buffering, timer
channel, closed-channel, and waiter-matching semantics instead of duplicating
them. A send or receive that cannot complete immediately still enters the
existing retained stackless operation path. Race builds retain that path so
their logical operation identity and instrumentation remain unchanged, and the
pull-comparison build retains it so push and pull measurements continue to
compare the same operation boundary.

The direct path handles buffered channels, closed receives, expired timer
channels, and queued ordinary or stackless peers. A closed send still panics
through the ordinary runtime path and is translated by the existing native
panic machinery. Nil channels and genuinely blocked operations fall back. No
runtime object layout, source annotation, public compiler interface, scheduler
protocol, or non-Go backend changes.

This checkpoint adds a fixed `BenchmarkReadyChannelPair` probe: every
iteration sends to and receives from the same capacity-one channel. Its result
is checked by `TestProbes`, and the lowering audit requires the helper to be a
coroutine primary without fallback. The exact parent and candidate used the
same probe source, disabled inlining, one P, one warm-up, alternating execution,
and matched randomized linker layouts. Native Darwin/arm64 used eight layouts
at 300 ms per probe. Linux/amd64 used six layouts under Rosetta, so its timings
are directional while its functional and allocation results remain useful.

| Probe | Darwin/arm64 parent -> candidate | Linux/amd64 translated parent -> candidate |
| --- | ---: | ---: |
| ready buffered send and receive pair | 79.975 ns -> 19.165 ns (-76.16%) | 137.500 ns -> 32.800 ns (-76.24%) |
| unbuffered channel round trip | 213.500 ns -> 131.100 ns (-38.86%) | 379.550 ns -> 248.800 ns (-35.19%) |
| ready select, including its ready input send | 81.940 ns -> 52.480 ns (-35.15%) | 136.500 ns -> 82.100 ns (-39.87%) |
| immediately released blocked select | 143.750 ns -> 117.700 ns (-16.86%) | 257.350 ns -> 216.500 ns (-16.26%) |

The ready pair and channel round trip improved in every matched layout on both
platforms. Linux ready and blocked select also improved in every layout;
Darwin improved in seven of eight. The select probes benefit because their
producer sends are now eligible for the channel fast path; their select
arbitration remains the preceding checkpoint's implementation. File read, TCP
read, one-nanosecond sleep, and amortized-yield controls had neutral paired
medians, within about 1.2% on Darwin and 0.7% on translated Linux. Every probe
retained zero bytes and zero allocations per iteration.

An independent eight-layout native Darwin comparison compiled the same minimal
ready-channel source with unmodified Go at exact upstream merge-base
`da7c67f595` and with the coroutine candidate. Official Go measured 16.60 ns
per pair and the candidate measured 19.29 ns, a remaining 16.98%
whole-experiment difference; both allocated zero bytes. The exact checkpoint
parent was about 4.82 times official Go, while the candidate is about 1.16
times official Go. This comparison provides end-to-end context rather than
attributing unrelated experiment differences to this change.

A ten-second candidate CPU profile moved the target's cost back to the ordinary
`chansend`, `chanrecv`, channel lock, and value-copy paths. The previous
operation registration, recycling, completion, and scheduler-wake functions no
longer appear among its sampled hotspots.

Regression tests cover buffered send and receive, closed receive, expired timer
receive, queued ordinary and stackless senders and receivers, direct closed-send
panic, operation-count stability, genuinely blocked send and receive fallback,
race and pull fallback, and all five compiler-generated channel helper calls.
Combined normal, race, and pull profiles report 100% statement coverage for
both direct helpers and both conditional runtime wrappers.

Full experiment-off and experiment-on compiler/runtime matrices pass on native
Darwin/arm64. On translated Linux/amd64, the isolated complete coroutine
compiler package passes, as do normal, race, pull, pull-plus-race, and
`checkptr=2` stackless runtime tests, `go vet runtime`, and the complete
lowering, symbol, allocation, and public-read audit. The translated full
runtime still traps in Rosetta address-space crash tests and is not a reliable
gate. Linux/386, Linux/arm64, FreeBSD/amd64, Windows/amd64, and Darwin/amd64
runtime tests cross-compile with the experiment enabled; native Linux CI
remains the complete runtime gate.

#### Adapt recursive frame chunk sizes

Revision `d4f127bcd5`, based on exact parent `e350225e50`, uses eight-element
typed chunks for small recursive frames while retaining four-element chunks
for larger frames. The compiler and runtime derive the boundary from the
allocator's real small-object payload limit,
`gc.MaxSmallSize-gc.MallocHeaderSize`, rather than duplicating a numeric
constant. Frames through 4095 bytes use `[8]frame`, frames from 4096 through
8190 bytes use `[4]frame`, and larger frames retain the existing per-frame
fallback.

The two chunk classes use disjoint task-local marker ranges after the shared
six-frame direct prefix. Fused-frame markers use two previously spare bits for
the four-element class and for successful typed-chunk validation. The
validation bit is inherited only by runtime-created child transitions, which
lets repeated self recursion trust the established type and class without
repeating the cold check. The allocation field remains four bits, existing
runtime object sizes and factory ABIs do not change, and the lowering adds no
source annotation or public interface.

A global eight-element prototype was rejected because it made 5 KiB frames
ineligible for chunking. At depth 512, the existing four-element implementation
used 2,752,256 bytes and 131 allocations per operation; the global-eight
prototype used 2,732,544 bytes but 762 allocations, or 5.82 times as many.
The adaptive implementation preserves the former 5 KiB result exactly while
halving the allocation frequency for small deep recursion:

| Probe | Exact parent | Adaptive candidate |
| --- | ---: | ---: |
| recursive depth 64 | 0 B, 0 allocs | 0 B, 0 allocs |
| recursive depth 4096 | 215,200 B, 965 allocs | 215,424 B, 486 allocs |
| 5 KiB frame, depth 512 | 2,752,256 B, 131 allocs | 2,752,256 B, 131 allocs |

The deep-recursion allocation count falls by 49.64% for a 224-byte increase
in retained allocation. The benchmark gate fixes all three probes: depth 64
must remain allocation-free, depth 4096 must stay below 216,000 bytes and 500
allocations, and the 5 KiB probe must stay below 2,850,000 bytes and 140
allocations.

The exact parent and candidate used identical benchmark source, disabled
inlining, one P, and matched linker randomization. Twelve independent layouts
each ran a 100 ms warm-up followed by two 750 ms samples in parent, candidate,
candidate, parent order. A predeclared control rejected a layout if unchanged
`YieldEntry` moved by more than 7.5%; all 12 layouts passed.

| Probe | Parent median | Candidate median | Paired median change |
| --- | ---: | ---: | ---: |
| unchanged public entry | 76.685 ns | 76.788 ns | -0.62% |
| recursive depth 64 | 2.328 us | 2.326 us | +0.41% |
| recursive depth 4096 | 222.599 us | 205.473 us | -6.32% |

The public-entry control and shallow recursion are neutral by a two-sided sign
test (`p=0.774` for both). Deep recursion improved in 11 of 12 matched layouts
(`p=0.00635`). This removes the shallow regression observed before validation
was cached while retaining the deep-recursion allocation and latency gains.

The full experiment-on normal and race runtime suites pass on native
Darwin/arm64. Pull, pull-plus-race, and `checkptr=2` stackless runtime tests,
the complete coroutine compiler suite, `go vet runtime`, experiment-off
compilation, and the complete benchmark audit pass on native Darwin/arm64 and
translated Linux/amd64 as applicable. The translated full Linux runtime still
reaches the existing Rosetta `TestCheckFDs` address-space failure. Linux/386,
Linux/arm64, FreeBSD/amd64, Windows/amd64, and Darwin/amd64 runtime tests
cross-compile at the final revision; native Linux CI remains the full runtime
gate.

The current focused profiles cover the compiler's new selection helper and
its lowering caller at 100%, as well as runtime chunk length, bounds, class,
type, and marker helpers at 100%. Across changed compiler and runtime blocks,
115 of 127 statements execute (90.55%). The only missing statements are the
`unlock` and `throw` bodies of six fail-closed marker invariants; four invariant
classes also have fatal subprocess tests, whose counters cannot be returned
after `throw` terminates the process.

#### Bucket concurrent operation lookup

Revision `28adf7dc48`, based on exact parent `af5fe7c5bd`, replaces the single
stackless-operation registry chain with 16 chains selected by the monotonically
assigned operation ID. The old registry prepended new operations. Closing a
channel wakes its logical waiters in FIFO order, so completing 100 parked tasks
repeatedly searched near the end of that LIFO chain and performed quadratic
work. Each bucket retains the existing intrusive link and lock; operation IDs,
completion validation, recycling, and object layout do not change.

The pointerful bucket array and the bounded netpoll scan cursor live in one
process-lifetime registry object allocated during runtime initialization. The
static registry handle therefore remains the same size as the former
single-list handle: 32 bytes on Darwin/arm64 and 24 bytes on Linux/amd64 in the
benchmark binaries. Sixteen buckets use 128 bytes of pointer slots on 64-bit
targets and distribute 64 consecutive operations over four links per chain.
The netpoll idle retry still examines at most 64 operations, but rotates its
starting bucket after a bounded or successful pass. Larger concurrent sets
remain correct through the chains.

The exact parent and candidate used identical compiler flags and probe source,
disabled inlining, one P, one 50 ms warm-up, alternating execution order, and
matched randomized linker layouts. Native Darwin/arm64 used ten layouts with
two 300 ms samples per layout. Linux/amd64 used six layouts under Rosetta with
the same sampling; its absolute times are directional.

| Probe | Darwin/arm64 parent -> candidate | Linux/amd64 translated parent -> candidate |
| --- | ---: | ---: |
| park and wake 100 tasks | 18.96 us -> 12.34 us (-34.88%) | 26.20 us -> 19.61 us (-25.15%) |

The Darwin result improved in all ten layouts (`p<0.001`). Five of the six
initial Linux layouts improved; the remaining sample coincided with external
host load, and a five-pair isolated repeat of that exact layout measured
26.68 us -> 19.86 us (-25.55%, `p=0.008`). The allocation count remains one per
benchmark iteration, and the runtime operation-size regression test is
unchanged.

Yield entry, channel round trip, ready select, ready file and TCP reads, scalar
C calls, and blocking-C handoff had no significant regression. Because the
one-nanosecond timer does not access this registry but was order-sensitive in
the combined run, it was also measured alone in fresh processes for every
layout. Darwin measured 147.3 ns -> 148.3 ns (+0.68%, boundary `p=0.045`), and
translated Linux measured 205.1 ns -> 209.2 ns (neutral, `p=0.418`); both
retained zero bytes and zero allocations. The small unrelated Darwin movement
is treated as address-layout noise rather than registry-path cost.

A focused native Darwin CPU profile reduced
`takeStacklessCoroOperation` from 30.80% to 1.89% flat CPU and from 32.13% to
3.79% cumulative CPU. Its five-second task-park measurement improved from
19.154 us to 12.599 us while retaining one allocation per benchmark iteration.

Regression tests force two IDs into one bucket, find and remove both positions
in the collision chain, reject stale and repeated removal, and force more than
the 64-operation netpoll scan bound into one bucket. The focused profile covers
registration at 90%, take, find, and bucket selection at 100%, and bounded
netpoll scanning at 96%; the uncovered registration statement is the 64-bit ID
wrap defense.

The full runtime, race, pull, pull-plus-race, and `checkptr=2` tests pass on
native Darwin/arm64. On translated Linux/amd64, the focused normal and race
tests, pull, pull-plus-race, `checkptr=2`, `go vet runtime`, experiment-off
compilation, and the complete lowering, symbol, allocation, and public-read
audit pass. Linux/386, Linux/arm64, Linux/riscv64, FreeBSD/amd64,
Windows/amd64, and Darwin/amd64 runtime tests cross-compile with the experiment
enabled. The preceding `coro/main` merge is green in the focused macOS and
Ubuntu jobs and the native Linux `all.bash` job; the pull request CI remains
the native full-suite gate for this revision.
