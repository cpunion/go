# Push queues and wake-and-poll pull

This experiment compares the current stackless coroutine scheduler with a
Rust-like wake-and-poll model using the same Go compiler, runtime, native
executor, resume ABI, and event sources. It is a measurement prototype, not a
second supported coroutine mode.

“Pull” here does not mean busy polling. An event source records completion and
rings a root task's doorbell. The executor then polls the structured
coroutine tree until it reaches the ready leaf or returns pending, like a
Rust `Future` driven by a `Waker`.

The pull-aware queue and comparison exports compile only with the
`coropullcompare` build tag. A normal coroutine build uses the production push
scheduler and contains no comparison marker, hook, or build-tag-specific
hot-path branch. The tag is private to this measurement driver.

## Models

The probes distinguish two independent design choices: how readiness is
delivered and how a structured call tree is represented.

| Model | Readiness | Structured frame representation |
| --- | --- | --- |
| `Stackful` | ordinary Go scheduler | native goroutine stack |
| `Push` | completion queues the exact logical task | one task plus one typed frame per await |
| `Pull` | completion wakes the root, which polls its current leaf | exactly the same tasks and typed frames as `Push` |
| `CompactPull` | root wake and iterative tree polling | one parent-linked frame per structured await; no task object per child |

`Push` and `Pull` are the controlled comparison. Any difference between
them is attributable to exact-task queueing versus root wake-and-poll.
`CompactPull` is a compiler/runtime co-design probe. It measures the
additional opportunity from fusing task metadata into a structured future
frame; it must not be described as a consequence of polling alone.

The compact probe uses a reduction budget of 256 ready frames per episode.
Deep ready trees therefore yield to the Go scheduler instead of monopolizing
a P. Its explicit parent link keeps descent and completion iterative, so the
native Go stack does not grow with logical depth.

## Scope and invariants

All stackless variants use:

- the same `stacklessCoroScheduler` root and native-stack executor;
- the same unpooled root allocation and lifetime for the controlled `Push`
  and `Pull` pair, so public-root cache policy is not a model difference;
- the same resume action ABI and typed test frames;
- the same runtime timer, public regular-file read, and socket netpoll
  sources;
- the same operation publication and root doorbell;
- `GOMAXPROCS=1` for scheduler comparisons;
- no source annotation to select coroutine functions.

The same-structure pull driver deliberately supports one structured await
lineage. Its probes do not use independent spawn, which this measurement
driver does not support. Independent `go` tasks still require a runnable-root
queue in a pull design; only the interior structured future tree is polled.
This is also the useful boundary for a possible hybrid implementation: push
scheduling between roots, pull polling within a root.

The pull driver does not model a blocking foreign call. A genuinely blocking
C call must still leave the blocked call on its M and make another M available
to run ready work. Wake-and-poll can reduce logical-task routing overhead, but
it cannot remove that handoff requirement. The C probes therefore compare:

- ordinary cgo;
- the existing direct, declared-nonblocking C entry;
- push and pull scheduler episodes around that nonblocking entry.

Blocking-C capacity and handoff remain covered by the main coroutine probes,
not by this pull prototype.

## Probes

The runtime comparison includes:

- one root entry and an amortized yield batch;
- structured await depths 1, 8, 64, 256, and 4096;
- one-nanosecond runtime timers;
- ready `/dev/zero` reads and forced-blocking pipe reads;
- ready and forced-blocking `socketpair` reads through netpoll;
- ordinary cgo and direct System-ABI C calls, both steady and one call per
  scheduler episode;
- parked structured trees at depths 0 and 4096.

Timer and I/O probes use the controlled `Push` versus same-representation
`Pull` pair. `CompactPull` currently isolates structured await and parked
tree representation; it is not used to claim timer or I/O results.

Depth zero records the fixed parked-root cost. Depth 4096 exposes the marginal
frame, object, pointer-scan, and native-stack costs. The footprint probe
reports live heap bytes, live heap objects, stack bytes, GC-scanned heap and
stack bytes, and the average duration of five forced GC cycles while the tree
is parked. Entry and await probes also report bytes and allocations per
operation.

The forced-blocking I/O tests do not rely on timing alone. A writer waits until
the read is armed; the socket writer additionally waits until the runtime
netpoll waiter count has increased. This verifies that the scheduler really
survives a pending event rather than repeatedly measuring a ready syscall.

## Running

Build the tree with the experiment enabled, then run:

```
cd src
GOROOT_BOOTSTRAP=/path/to/bootstrap GOEXPERIMENT=coro ./make.bash

cd cmd/compile/testdata/corobench
./run_push_pull.bash /tmp/coro-push-pull
```

The runner builds the runtime test binary once, performs three unrecorded
warm-up pairs, and collects ten alternating-order 500 ms samples by default.
It runs each footprint model in a fresh process and reverses model order on
alternating samples. Environment variables can override `SAMPLES`,
`WARMUP_SAMPLES`, `BENCHTIME`, and `FOOTPRINT_SAMPLES`.

The manifest records the revision, toolchain, host, load average, worktree
status, and diff summary. Timing collected under material host load is only a
smoke result. Compare timing files with `benchstat`; inspect footprint
metrics as separate-process distributions rather than relying on one
multi-subbenchmark process.

Correctness alone can be checked with:

```
GOEXPERIMENT=coro go test -tags=coropullcompare runtime \
  -run '^TestStacklessCoroPushPull(Comparison|DirectCComparison)$'
```

## Results, 2026-08-14

Revision `94516db13b3634cfb348c99963c98e1aa04a0cc5` was measured after
merging the public-root scheduler reuse work. The controlled `Push` driver
deliberately bypasses that pool, matching the allocation and lifetime of the
private `Pull` driver.

### Darwin arm64

The primary run used an Apple M4 Max, twelve alternating 500 ms samples, and
six independent-process footprint samples. Load averages were 15.61, 25.08,
and 19.83 at startup and declined during the run. A focused confirmation used
twenty-four alternating one-second samples while the one-minute load declined
from about 11 to 8.

The same-representation models had identical object costs in every sample.
At structured depth 4096:

| Metric | `Push` | same-representation `Pull` |
| --- | ---: | ---: |
| allocated bytes/op | 315,616 | 315,616 |
| allocations/op | 7,938 | 7,938 |
| parked heap bytes | 470,728 | 470,728 |
| parked heap objects | 8,253 | 8,253 |
| GC-scanned heap bytes | 395,072 | 395,072 |
| parked native-stack bytes | 65,536 | 65,536 |

The primary run found no significant `Push`/`Pull` difference for entry,
yield, timer, ready or blocked file I/O, ready or blocked socket I/O, or either
direct-C shape. It did show a 7.07% improvement at await depth 64. The focused
run confirmed a smaller but consistent structured-await effect:

| Await depth | `Push` | `Pull` | Change |
| ---: | ---: | ---: | ---: |
| 1 | 1.134 µs | 1.122 µs | -1.06% |
| 8 | 1.771 µs | 1.713 µs | -3.30% |
| 64 | 6.380 µs | 5.994 µs | -6.05% |
| 256 | 21.36 µs | 20.56 µs | -3.75% |
| 4096 | 407.4 µs | 376.0 µs | -7.72% |

Every focused timing difference had `p<0.001`; bytes and allocations remained
identical. Same-representation pull avoids the runnable-count atomic updates
and queue-pointer maintenance when a structured child or parent resumes. This
measures a real structured-handoff cost, but it is not unique to a public pull
model: a push scheduler could recover it with a proven single-owner direct
handoff inside a root.

`CompactPull` produced the larger result by changing representation as well
as delivery:

| Depth-4096 metric | `Push` | `CompactPull` | Change |
| --- | ---: | ---: | ---: |
| time/op | 412.0 µs | 72.00 µs | -82.52% |
| allocated bytes/op | 315,616 | 131,312 | -58.40% |
| allocations/op | 7,938 | 4,098 | -48.37% |
| parked heap bytes | 470,728 | 274,136 | -41.76% |
| parked heap objects | 8,253 | 4,157 | -49.63% |
| GC-scanned heap bytes | 395,072 | 198,488 | -49.76% |
| parked native-stack bytes | 65,536 | 65,536 | 0% |

At depths 1 through 256, compact frames used 16 more allocated bytes per
operation and the same allocation count as `Push`. The allocation reduction
at depth 4096 aligns with the push scheduler's 256-task cache limit: fused
frames avoid the additional task object for structured children beyond that
cache. The time improvement starts earlier because compact polling also
removes per-level task scheduling.

For context, the depth-4096 stackful baseline parked 10,232 heap bytes in 52
objects plus 262,144 stack bytes. It scanned 432 heap bytes and 196,944 stack
bytes. `CompactPull` brought total GC-scanned memory close to the stackful
baseline while retaining a smaller native stack, but still used more combined
heap and stack memory: 339,672 bytes versus 272,376 bytes. The unfused `Push`
total was 536,264 bytes.

The nonblocking foreign-call reference took 2.863 ns/call versus 14.245
ns/call for ordinary cgo (-79.90%, `p<0.001`). Both paths allocated nothing.
Wrapping the direct call in `Push` or `Pull` was statistically
indistinguishable for both steady calls and separate scheduler episodes.

### Linux amd64 validation

The same revision was rebuilt with a Go 1.26.5 bootstrap and tested in an
amd64 Rosetta container. The twelve-sample run reported a `VirtualApple` CPU
and startup load averages of 3.99, 4.35, and 3.34. Translation made timing
distributions much wider: no same-representation `Push`/`Pull` timing was
significant, so this run is not evidence against the native Darwin await
result.

Allocation and footprint results did reproduce exactly within the Linux
platform. `Push` and `Pull` both allocated 315,616 bytes in 7,938 allocations
at depth 4096 and both parked 476,304 heap bytes in 8,260 objects. `CompactPull`
reduced allocated bytes by 58.40%, allocation count by 48.37%, parked heap by
41.27%, parked objects by 49.59%, and GC-scanned heap by 49.43%. Its translated
depth-4096 time was 81.30% lower. Direct C was 2.380 ns/call versus 26.245
ns/call for ordinary cgo (-90.93%, `p<0.001`); treat that ratio as directional
because both paths ran through translation.

The full Darwin runtime and compiler suites passed. Focused comparison tests,
the complete stackless coroutine test set, race, and `checkptr=2` passed on
both platforms; the Linux compiler/coro suite also passed. A `nocoro` runtime
run passed on Darwin. The default build's decoded `readyLocked` and `take`
instruction flow matched `coro/main`, and its symbol table contained neither
the comparison marker nor a mode helper. The tagged default and pull branches
covered every changed executable runtime line.

These results favor a hybrid direction: retain push scheduling between
independently runnable roots and for external event delivery, add a
single-owner structured handoff optimization where compiler proof permits,
and investigate task/frame fusion for structured await chains. None of these
requires source annotations or a separately exposed Rust-style pull API, and
none changes the M-handoff requirement for a blocking C call.

## Production structured handoff follow-up, 2026-08-15

Revision `de39ab3163`, relative to the exact `3db190a434` merge base,
implements the first hybrid step in the production push scheduler. When a
normally completed structured child finds its suspended parent and no other
task is already runnable, the same executor marks the parent running, recycles
the child, and resumes the parent directly. It avoids publishing a queue entry
that the same executor would immediately consume.

The optimization is deliberately completion-local. It falls back to the
ordinary ready queue when a sibling is already queued, the parent is still
returning `Wait`, readiness is pending, the race detector is enabled, or the
pull comparison driver is active. External readiness, panic, `Goexit`, root
completion, and blocking-C M replacement are unchanged. No task or scheduler
field was added: their 64-bit sizes remain 48 and 192 bytes, respectively.
The change requires neither a compiler annotation nor a second coroutine
mode.

Twenty alternating fixed-layout 500 ms samples on native Darwin/arm64 showed
the expected depth-dependent improvement:

| Probe | Parent | Candidate | Change |
| --- | ---: | ---: | ---: |
| await depth 1 | 1.137 us | 1.120 us | -1.45% |
| await depth 8 | 1.776 us | 1.688 us | -4.93% |
| await depth 64 | 6.320 us | 5.750 us | -9.02% |
| await depth 256 | 21.78 us | 19.75 us | -9.33% |
| await depth 4096 | 411.9 us | 361.5 us | -12.25% |
| steady structured await | 45.76 ns | 39.54 ns | -13.61% |

Twelve matched randomized linker layouts reproduced changes from -2.22% at
depth 1 to -12.41% at depth 4096, with the steady await improving 16.94%.
Entry, yield, timer, ready and blocked file I/O, ready and blocked socket I/O,
and channel controls were neutral. The fixed-layout channel result moved
1.57%, but became neutral (`p=0.147`) under randomized layouts. Every bytes/op
and allocations/op result was identical between revisions.

Translated Linux/amd64 validation was noisier but directionally agreed. The
fixed-layout run improved depth 64 by 12.86%, depth 256 by 10.56%, depth 4096
by 15.09%, and steady await by 12.00%; entry, yield, timer, file, socket, and
channel controls were neutral. Eight matched randomized layouts reproduced
5.40% at depth 8 through 11.34% at depth 4096, with steady await improving
8.94%, and again changed no allocation result.

Normal, race, and `checkptr=2` stackless runtime tests, the compiler coroutine
suite, and the architecture probe pass on Darwin/arm64 and translated
Linux/amd64. Full `coro` and `nocoro` runtime suites pass on Darwin. The
translated Linux runtime suite passes with the same three known
Rosetta-incompatible intentional-crash or oversized-mmap tests excluded;
native CI remains unfiltered. FreeBSD and Windows amd64 plus Darwin amd64 and
Linux arm64 cross-compilation pass. Focused coverage is 96.2% for `runTasks`
and 84.8% for `complete`; every executable statement added for direct handoff
and each queue fallback is covered.

## Interpretation gates

A pull implementation is worth pursuing only if the measurements separate
these outcomes:

1. If same-structure `Pull` materially improves timer or I/O wake latency
   without regressing ready work, the root polling policy has independent
   value.
2. If `Push` and `Pull` are similar but `CompactPull` improves deep
   allocation and GC scan cost, the benefit belongs to frame fusion and
   structured ownership, not to replacing the global scheduling policy.
3. If compact frames help only artificial deep recursion while ordinary
   timer, file, network, and C workloads do not improve, adding a second
   language-level async model is unlikely to justify its compiler and library
   complexity.
4. Independent goroutines, blocking C calls, preemption, panic/defer/Goexit,
   and multi-P fairness must remain compatible with the push root scheduler.
   This experiment does not authorize weakening those semantics.

The likely production direction, if the compact results hold, is therefore
hybrid and feature-gated: preserve push-based stackless goroutines as the
public execution model, then consider compiler fusion of provably structured
await chains. A separately exposed Rust-style pull API would need additional
evidence from cancellation, ownership, stream/backpressure, and ecosystem
integration; those are outside this benchmark.
