# Push queues and wake-and-poll pull

This experiment compares the current stackless coroutine scheduler with a
Rust-like wake-and-poll model using the same Go compiler, runtime, native
executor, resume ABI, and event sources. It is a measurement prototype, not a
second supported coroutine mode.

“Pull” here does not mean busy polling. An event source records completion and
rings a root task's doorbell. The executor then polls the structured
coroutine tree until it reaches the ready leaf or returns pending, like a
Rust `Future` driven by a `Waker`.

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

## Darwin arm64 results, 2026-08-13

Revision `77bd3a94d22b4f8e7792b3e22846a7a58cc62e3e` was measured on an
Apple M4 Max with ten alternating 500 ms timing samples and six
independent-process footprint samples. The machine was not quiet: its load
averages at the start were 45.51, 103.10, and 123.78. Allocation and live-set
metrics were stable across every sample, but absolute timings and timing
ratios require a repeat on an otherwise idle machine.

The controlled comparison produced identical object costs. At structured
depth 4096:

| Metric | `Push` | same-representation `Pull` |
| --- | ---: | ---: |
| allocated bytes/op | 315,616 | 315,616 |
| allocations/op | 7,938 | 7,938 |
| parked heap bytes | 470,616 | 470,616 |
| parked heap objects | 8,252 | 8,252 |
| GC-scanned heap bytes | 394,968 | 394,968 |
| parked native-stack bytes | 65,536 | 65,536 |

`benchstat` found no significant `Push`/`Pull` timing difference at the 0.05
level for entry, yield, any await depth, timer, ready or blocked file I/O,
ready or blocked socket I/O, or direct C calls. Consequently this run gives
no evidence that replacing exact-task delivery with root wake-and-poll has
an independent performance benefit.

`CompactPull` did reduce the cost of a deep structured chain:

| Depth-4096 metric | `Push` | `CompactPull` | Change |
| --- | ---: | ---: | ---: |
| allocated bytes/op | 315,616 | 131,312 | -58.40% |
| allocations/op | 7,938 | 4,098 | -48.37% |
| parked heap bytes | 470,616 | 274,024 | -41.77% |
| parked heap objects | 8,252 | 4,156 | -49.64% |
| GC-scanned heap bytes | 394,968 | 198,384 | -49.77% |
| parked native-stack bytes | 65,536 | 65,536 | 0% |

At depths 1 through 256, however, compact frames used 16 more allocated bytes
per operation and exactly the same allocation count as `Push`. The change at
depth 4096 aligns with the push scheduler's 256-task cache limit: compact
frames avoid the additional task object for structured children beyond that
cache. This is evidence for task/frame fusion or lifetime coalescing, not for
a public pull execution model.

For context, the depth-4096 stackful baseline parked 10,232 heap bytes in 52
objects plus 262,144 stack bytes. It scanned 432 heap bytes and 196,944 stack
bytes. `CompactPull` therefore brought total GC-scanned memory close to the
stackful baseline while retaining a smaller native stack, but still used more
combined heap and stack memory: 339,560 bytes versus 272,376 bytes. The
unfused `Push` total was 536,152 bytes.

The nonblocking foreign-call reference also preserved the existing
direct-call result. Its median was 20.20 ns/call versus 205.25 ns/call for
ordinary cgo (-90.16%, `p=0.000`, ten samples). Both distributions were very
wide under the recorded load, so this establishes the direction rather than
a release-quality absolute number. Wrapping the direct call in either
stackless scheduler did not allocate; `Push` versus `Pull` was not
statistically distinguishable.

All focused comparison and stackless coroutine tests passed on darwin/arm64
and linux/amd64, including focused race runs. The new runtime lines reached
100% patch coverage under the stackless coroutine test set. A full
darwin/arm64 `go test runtime` run reached its ten-minute package timeout
while unrelated standard stress tests were still running under the high host
load; it showed no comparison assertion failure. Linux timing was not used
because that validation ran through an amd64 container on an arm64 host.

These results favor a hybrid direction: retain push scheduling between
independently runnable roots and for event delivery, while investigating
compiler fusion of provably structured await chains inside a root. The
optimization does not require source annotations or a separately exposed
Rust-style pull API, and it does not change the M-handoff requirement for a
blocking C call.

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
