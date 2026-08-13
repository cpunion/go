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
GOEXPERIMENT=coro go test runtime \
  -run '^TestStacklessCoroPushPull(Comparison|DirectCComparison)$'
```

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
