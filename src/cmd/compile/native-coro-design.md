# Native Stackless Coroutine Design

Status: proposed design and MVP plan; no native state-machine lowering has been
implemented

Last updated: 2026-07-26

Development branch: `cpunion/go:main`, which periodically merges the Go
development branch

Topic branch: `coro/native-state-machine`

## 1. Decision

This design keeps the Go parser, type checker, Unified IR, escape analysis,
generic SSA, machine backends, assembler, linker, and most of the runtime. It
replaces the LLVM coroutine part of the earlier experiment with a
compiler-generated, explicit state machine.

The experiment remains opt-in through `GOEXPERIMENT=coro`. A build without the
experiment must continue to use the unmodified Go ABI, goroutine
representation, compiler pipeline, and runtime behavior.

The coroutine is push-based:

```text
source or producer publishes a fact
                  |
                  v
        scheduler request/doorbell
                  |
                  v
       owner queues one runnable token
                  |
                  v
     executor invokes resume(frame, packet)
                  |
                  v
       one transition is returned
```

There is no public or internal `for resume() {}` protocol that repeatedly pulls
a continuation. A producer never calls a continuation and never retains a
frame or logical goroutine pointer. It publishes a stable operation identity
and requests scheduler service. Only the scheduler can resume or destroy a
frame.

The design deliberately retains the semantic contracts developed for the
LLGo coroutine runtime:

- a logical goroutine is a chain of stackless frames;
- the scheduler is the sole continuation executor;
- an operation producer owns a stable operation record, not a frame;
- child completion is stored in a parent-owned record;
- suspend, resume, result, cancellation, and destruction are exact
  transactions;
- blocking foreign calls may occupy native threads, while replacement native
  threads continue managed work;
- timer, poll, worker, channel, and host completion all publish facts through
  the same scheduler boundary.

The physical implementation is different. LLVM handles and CoroSplit frames
are replaced by typed Go heap objects, a program counter, generated resume
functions, and the existing Go machine-code backends.

The main physical substitutions are:

| LLVM route | Native Go route |
| --- | --- |
| coroutine handle | typed frame pointer plus generated resume symbol |
| `llvm.coro.suspend` | store state, return one scheduler action |
| `llvm.coro.resume` | scheduler invokes `resume(frame, packet)` |
| `llvm.coro.destroy` | generated cleanup and destroy states |
| CoroSplit frame discovery | compiler liveness and typed frame layout |
| LLVM IR emitter | ordinary Go IR, SSA, and architecture backend |

## 2. Goals

### 2.1 Language and execution goals

- Keep ordinary Go source. Do not add `async` or `await` syntax.
- Make suspension an inferred function effect.
- Implement `go f()` as a new stackless logical goroutine.
- Preserve synchronous call and result semantics in source.
- Give each source function one primary body. Do not maintain complete sync
  and coroutine copies of the same function.
- Preserve push-based scheduling across child calls, timers, I/O, and foreign
  operations.
- Do not retain a native activation across a managed suspension.
- Do not allocate a native stack for every logical goroutine.
- Reuse the Go garbage collector to scan typed coroutine frames.
- Reuse the Go architecture backends, assembler, object format, and linker.
- Keep the implementation modular enough to review as a possible future Go
  compiler experiment.

### 2.2 Foreign-call goals

Removing cgo call overhead is a primary architecture goal, not a later
micro-optimization.

For a supported direct call, the generated path must:

- use the platform C ABI;
- avoid a cgo-generated Go wrapper and C wrapper;
- avoid `runtime.cgocall`;
- avoid `runtime.asmcgocall` and its per-call switch to `m.g0`;
- avoid the general cgo callback path when completion only needs to wake a
  coroutine;
- retain the GC, scheduler, preemption, pointer-lifetime, race, and traceback
  bookkeeping that is required for correctness.

This is a reduction of runtime transition cost. It does not mean that all
foreign calls have zero cost, that a C compiler or external linker is never
needed, or that Go pointer rules may be bypassed.

### 2.3 Compatibility goals

- The default compiler and runtime path must remain unchanged.
- Coroutine archives must have an experiment and ABI identity distinct from
  ordinary archives.
- A coroutine toolchain must rebuild packages whose ABI or function-value
  representation changes.
- Compiler changes should be small packages or explicit hooks, not pervasive
  checks distributed across unrelated compiler passes.
- Names, comments, tests, and package boundaries should follow the conventions
  used by the Go compiler and runtime.

## 3. Non-goals

- Capturing an arbitrary native stack or resuming at an arbitrary instruction.
- Using split stacks, stack copying, `setjmp`, or `longjmp` as the logical
  continuation mechanism.
- Running arbitrary allocating Go code on `m.g0`.
- Removing every physical machine stack. Scheduler and resume episodes use a
  bounded stack owned by an executor.
- Supporting every C ABI type, variadic C calls, C++ exceptions, callbacks, or
  `longjmp` in the MVP.
- Treating an unknown foreign function as nonblocking or asynchronously
  completable.
- Completing the full standard library, reflection, debugging, race detector,
  plugins, or shared-library compatibility in the first implementation.
- Copying the LLGo implementation into the Go repository. Its contracts are a
  design reference; the implementation here should primarily reuse Go code
  and be written in the style and licensing context of the Go repository.

## 4. Why the current cgo path is not the target

The current generated path is, in simplified form:

```text
Go caller
  -> cmd/cgo Go wrapper
  -> pointer checks and argument frame
  -> runtime.cgocall
       -> entersyscall
       -> external-preemption and cgo state
       -> runtime.asmcgocall
            -> switch from user G stack to m.g0
            -> cgo-generated C wrapper
            -> actual C function
            -> restore user G stack
       -> exitsyscall
  -> unpack result
```

This path solves problems that ordinary Go goroutines create:

- a growable and movable goroutine stack is not a suitable C stack;
- the runtime must let another M run while C blocks;
- the collector and tracer need a valid saved Go stack;
- callbacks must re-enter a Go goroutine stack;
- a general argument frame handles arbitrary generated signatures.

A stackless coroutine executor changes the first two premises. A resume episode
can run on one fixed, nonmoving, C-compatible executor stack. A potentially
blocking call can release its managed execution permit before entering C, so a
replacement executor can make progress. It is therefore unnecessary to pay
for a cgo wrapper and a `g0` stack switch on every supported call.

The remaining responsibilities do not disappear. They become explicit,
smaller foreign-entry and foreign-exit protocols for each call class.

## 5. Core invariants

The compiler and runtime must enforce these invariants in every phase.

1. A suspended logical goroutine has no live native activation.
2. A frame has exactly one scheduler owner.
3. A frame is resumed or destroyed only after an exact scheduler action is
   issued for it.
4. A producer carries only pointer-free operation identity and payload allowed
   by its source contract.
5. A producer cannot call `resume`, `destroy`, or a user callback.
6. A completion fact is sticky until the owner acknowledges it.
7. A result has one terminal disposition: take, discard, or transfer.
8. A child writes completion to storage owned by the parent before the child
   frame can be destroyed.
9. A frame cannot be recycled until all producer admission, result, callback,
   and cancellation leases are quiescent.
10. No plain primary has a reachable managed suspension.
11. No physical C frame is captured as a coroutine continuation.
12. A direct C call uses the System ABI only when its signature, clobbers,
    pointer policy, callback policy, and blocking class are known.

Violations should fail closed in the compiler or runtime. They must not fall
back to guessing from a symbol name or code pointer.

## 6. Execution model

### 6.1 Logical and physical execution

The experiment initially keeps two scheduling levels:

- the existing Go runtime owns M, P, GC workers, system goroutines, signals,
  and thread creation;
- the coroutine scheduler owns stackless logical goroutines and their frames.

The existing runtime owns a bounded set of M-bound executor Gs. An executor G
is not placed on the ordinary movable G run queue. The target design gives each
executor the fixed, noncopying stack supplied by its OS thread. It is a
physical execution context, not a language-visible logical goroutine.

This is an incremental implementation strategy. It avoids changing all runtime
G states before the compiler transform is proven, while still meeting the
central requirement: the number of stacks follows active executors and
blocking foreign calls, not the number of logical goroutines.

The long-term implementation may integrate the coroutine ready queues more
deeply into the runtime scheduler. That is not required to prove the compiler
and foreign-call model.

### 6.2 Native executor stack and `m.g0`

`m.g0` has the desired native stack properties, but arbitrary Go code cannot
safely run there. Allocating, growing a stack, reaching a normal safe point, or
calling many runtime helpers from `g0` violates existing runtime assumptions.

A dedicated executor G permits ordinary typed Go frames, allocation, write
barriers, and GC safe points. Its stack differs from an ordinary goroutine
stack in two ways:

- it is the native OS thread stack and is never copied or grown;
- its size is charged per executor, not per logical goroutine.

This requires a runtime layout change. At M initialization, the runtime starts
on the OS stack as it does today. Before managed coroutine execution, it must
provide a separate internal scheduler stack for `g0`, associate the OS stack
with the executor G, and switch between them at runtime scheduling boundaries.
The exact bootstrap and teardown sequence is an MVP feasibility task.

The reason to prefer the OS stack is stronger than stack size alone. The
current `asmcgocall` path documents that `g0` is expected to be an
OS-allocated stack suitable for compiler-generated C code. Some libc,
unwinding, sanitizer, signal, and pthread stack-introspection behavior can
depend on that property. A separately mapped fixed stack can validate a
restricted C fixture, but it cannot establish general C compatibility.

The runtime must identify the executor stack explicitly. It must not silently
call `morestack` and copy it. Arbitrary Go code also cannot be moved wholesale
onto `g0`; the executor needs a distinct G identity and ordinary GC state.

Runtime-internal C calls also need an audit. Several platforms currently rely
on `asmcgocall` and the assumption that `g0` is the OS stack. The experiment
must either issue those calls from the native executor stack or preserve an
explicit OS-stack call capability distinct from the new scheduler `g0`. This
is especially important on Darwin and is part of the two-platform MVP gate.

### 6.3 Resume episode

A resume function runs until exactly one of these transitions:

```go
type coroActionKind uint8

const (
	coroActionInvalid coroActionKind = iota
	coroActionSuspend
	coroActionComplete
	coroActionStartChild
	coroActionSpawn
	coroActionYield
	coroActionEnterForeign
)
```

These names are illustrative, not a frozen runtime API.

The resume function returns one value describing the physical transition.
Returning an action is not a pull coroutine protocol. The scheduler invokes
the function only after a runnable token was pushed to its queue, and the
returned transition transfers ownership back to the scheduler.

Starting a child does not recursively invoke arbitrary child and parent
continuations on the same native stack. The scheduler trampolines the action.
A later synchronous-completion optimization must remain bounded and must not
reintroduce native recursion proportional to the logical call depth.

### 6.4 Scheduler ownership

Each logical G has one state:

```text
new -> runnable -> running -> waiting -> runnable
                         \-> complete
                         \-> cleanup -> complete
```

Only an executor that has claimed a runnable token may change `runnable` to
`running`. Operation sources publish facts to owner mailboxes and set a
coalescing scheduler request. The owner resolves the operation, materializes a
typed resume packet, and then queues the logical G.

An executor entry has a deterministic reduction budget. Queue dequeue, resume,
destroy, source fact, result reconciliation, and child completion each consume
declared work. Exhausting the budget publishes another scheduler request; it
does not recursively re-enter the executor. Native code may iterate in an
outer driver only after the previous entry returned to a stable boundary.

This separation is required for foreign threads, signal handlers, host
callbacks, and interrupt handlers. None of those contexts is allowed to
execute compiler-generated Go continuation code.

## 7. Compiler design

### 7.1 Orthogonal analyses

One taint bit is not sufficient. At minimum, the program model records:

```go
type suspendEffect uint8

const (
	noSuspend suspendEffect = iota
	maySuspend
)

type execFlags uint32

const (
	needsPreempt execFlags = 1 << iota
	mayBlockThread
	needsSystemABI
	threadAffine
)
```

It separately records:

- suspend effect and execution constraints;
- function-value representation and dynamic target facts;
- emission demand for a primary, root adapter, descriptor, or ABI adapter.

A direct nonblocking C call has `needsSystemABI` but does not by itself add
`maySuspend`. A direct potentially blocking call adds `mayBlockThread`, but it
does not suspend in the middle of the C frame. An asynchronous foreign
operation adds `maySuspend`, because the submit episode returns and a later
fact resumes the logical G.

### 7.2 Analysis and lowering pipeline

The proposed pipeline is:

```text
syntax, types2, Unified IR
  -> import coroutine summaries
  -> provisional effect analysis
  -> devirtualization, inlining, wrappers
  -> final effect, execution, value-flow, and demand fixed points
  -> freeze function and site plans
  -> native coroutine lowering
       - create typed frame types
       - split control flow at planned sites
       - spill values live across a split
       - create resume, entry, and cleanup functions
  -> ordinary escape analysis
  -> walk
  -> ordinary Go generic SSA
  -> ordinary target lowering, scheduling, register allocation
  -> ordinary Go object and linker
```

The important change from the LLVM design is the location of the physical
transform. The native transform must occur before escape analysis, so the
existing compiler can see typed frame allocations, pointer fields, write
barriers, and generated calls.

The existing pre-target-lowering SSA hook remains useful as a verifier. It is
not the primary state-machine transform.

### 7.3 Program model

`cmd/compile/internal/coro` should own a frozen, package-level model:

```go
type Program struct {
	Functions []Function
	Sites     []Site
	Calls     []Call
	Imports   []PackageSummary
}
```

The exact representation should reuse existing IR identities and avoid a
parallel symbol table. The package is responsible for:

- imported effect summaries;
- local fixed-point analysis;
- stable site identities;
- planned frame fields;
- call and operation recipes;
- verification;
- versioned export data.

Lowering code consumes this model. It must not rescan symbol names to decide
whether a call is a timer, channel, syscall, or foreign operation.

### 7.4 Generated frame

For a source function such as:

```go
func f(x int) int {
	y := x + 1
	yieldOnce()
	return y * 2
}
```

the conceptual output is:

```go
type fCoroFrame struct {
	header coroFrameHeader
	x      int
	y      int
	result int
}

func fCoroResume(frame *fCoroFrame, packet coroResumePacket) coroAction {
	switch frame.header.pc {
	case 0:
		frame.y = frame.x + 1
		frame.header.pc = 1
		return coroSuspendYield(&frame.header)
	case 1:
		coroConsumeResume(&frame.header, packet)
		frame.result = frame.y * 2
		return coroComplete(&frame.header)
	default:
		throw("bad coroutine pc")
	}
}
```

This example describes semantics, not the final source-level implementation.
The real compiler should construct IR directly.

Only values live across a suspension enter the frame. Values whose address,
defer lifetime, debug identity, or GC lifetime crosses a suspension also enter
the frame even if a simple SSA liveness calculation would otherwise omit them.

### 7.5 Structured calls

For a coroutine-to-coroutine call:

1. evaluate operands exactly once;
2. allocate and initialize the child frame;
3. initialize a completion record owned by the parent frame;
4. store the parent's next state;
5. return `StartChild` to the scheduler;
6. let child completion publish the parent-owned record;
7. queue the parent with an exact resume packet;
8. consume the result or terminal outcome before destroying or reusing either
   record.

This protocol carries return, panic, abort, shutdown, and eventually Goexit as
distinct outcomes. Ordinary return must not overwrite an earlier terminal
outcome.

### 7.6 Preemption

Stackless preemption is cooperative at compiler-planned safe points. Loop and
recursive-call analysis can add a poll site. A successful poll stores the next
state and returns a yield action; it does not capture the current stack.

Plain call regions between safe points must have a bounded latency contract.
Unknown long-running assembly or foreign functions are not silently treated as
bounded.

### 7.7 Dynamic calls

The MVP supports closed direct calls only. The complete design needs a typed
callable descriptor at open boundaries such as interfaces, function values,
reflection, callbacks, and package-visible storage.

The descriptor records a stable signature and a plain or coroutine entry
capability. It must not infer the entry kind from a code address. A source
function still has one primary body; descriptors and boundary adapters are
thin entry mechanisms, not copies of the body.

## 8. Frame, GC, and result ownership

### 8.1 Typed frames

Compiler-generated frame types are ordinary typed heap objects. The existing
collector scans pointer fields. Stores into pointer fields use ordinary write
barriers. This is the principal benefit of lowering before escape analysis and
before Go SSA construction.

The runtime scheduler must retain a typed root to each runnable or waiting
logical G. A foreign producer must not retain that root.

### 8.2 Operation identity

An operation identity is a small, pointer-free value containing at least a
source route, slot, and generation. It is safe to copy through a C callback,
worker queue, pipe, event port, or interrupt mailbox.

The logical wait ticket is separate from the physical operation identity. One
logical select may own several operation identities, and a physical source may
remain active after another candidate won. The wait ticket stays in
scheduler-owned logical-G state and never enters the producer ABI.

The identity resolves to an owner-controlled operation record. Reuse is
allowed only after:

- completion or cancellation is acknowledged;
- any result is taken or discarded;
- the physical source is detached or quiescent;
- all producer admissions have returned;
- the generation changes.

The exact bit layout should not be frozen by the MVP.

### 8.3 Resume packet

Before a logical G becomes independently runnable, the old owner materializes
the winning result into a typed, frame-local packet. This permits work
migration without requiring the new owner to access an old timer, poll, worker,
or foreign source.

Each resume site consumes exactly one matching packet. A stale, duplicate, or
wrong-site packet is a runtime invariant failure.

The compiler places an exact resume gate before the resumed source state. The
gate first reconciles the site's completion, result lease, or child record.
Only then may task cancellation redirect control to shared cleanup. This order
prevents cancellation from losing an already selected payload or destroying a
child before its parent-owned completion is read.

## 9. Stack policy

### 9.1 Stackless does not mean stack-free

No native stack belongs to a suspended logical goroutine. A bounded number of
executor stacks still exists. A blocking direct C call also retains the
executor stack until C returns.

The memory objective is:

```text
O(logical G headers + live frames + wait records)
  + O(active executors and blocking foreign calls)
```

It must not be:

```text
O(logical G count * reserved native stack)
```

### 9.2 Native executor stack

The recommended target adds a runtime-supported executor G whose fixed,
guard-paged, noncopying stack is the OS thread stack. Generated coroutine code
and its allowed plain callees run on this stack. Runtime scheduler work runs on
a separate internal `g0` stack.

The compiler must not emit a path that calls `morestack` for a frame already
running on an executor stack. The MVP can prove a static maximum for its small
call graph and reject a program that exceeds the configured executor-stack
budget.

The production problem is broader:

- direct recursion must be state-machined, statically bounded, or rejected;
- indirect calls need a summary bound or a conservative capability;
- assembly needs an explicit stack bound;
- foreign calls need a documented C-stack bound or a sufficiently large
  guarded reservation;
- panic and signal paths need reserved emergency space.

Globally applying `NOSPLIT` to arbitrary Go code is not a solution. It would
replace controlled stack growth with unchecked overflow.

### 9.3 MVP stack gate

For the complete MVP, use the OS thread stack and discover its real bounds
through the existing per-platform runtime initialization. Add a separately
allocated, bounded runtime scheduler stack for `g0`.

A preliminary compiler or ABI fixture may run on a separately mapped stack,
but its result proves only the restricted functions under test. It is not the
acceptance path for general direct C calls.

The supported call graph must fit a conservative bound and fail
deterministically on overflow. The test must also cover signal delivery,
stop-the-world, traceback, and an external C unwind that stops at the declared
boundary. Supporting 32-bit address spaces is outside the first platform gate.

## 10. Direct System C ABI

### 10.1 Call classes

Every foreign declaration has one of three explicit call classes:

| Class | Suspension | Thread behavior | Default use |
| --- | --- | --- | --- |
| `DirectNoBlock` | none | current executor keeps its permit | short, proven, no-callback leaf |
| `DirectMayBlock` | none inside C | blocks M; replacement runs | unknown-duration synchronous C |
| `AsyncOperation` | submit suspends | completion publishes a fact | evented or callback-based API |

These names are design terms. Public directives are not part of the MVP.

An unknown C declaration is never `DirectNoBlock`. The conservative
classification is `DirectMayBlock` when its signature and safety contract are
supported. Otherwise compilation fails in the direct-call mode.

### 10.2 `DirectNoBlock`

The fast path is:

```text
validate/copy arguments
  -> enter bounded external region
  -> direct System ABI call on executor stack
  -> leave external region
  -> keep required values alive
```

Entering a bounded external region still has work:

- prevent asynchronous Go preemption from interpreting a C PC as Go code;
- make traceback and signal handling aware of the external PC;
- establish race or sanitizer synchronization when enabled;
- preserve special Go registers according to the target ABI.

It does not release the managed execution permit, enter syscall scheduling, or
switch stacks. GC stop-the-world may wait for the proven short call to return,
so this class needs a bounded-duration and no-callback contract.

### 10.3 `DirectMayBlock`

The blocking path is:

```text
prepare replacement capacity
  -> detach managed execution permit from this executor
  -> mark executor and M in a foreign-blocking state
  -> direct System ABI call on the same fixed stack
  -> reacquire a permit
  -> restore executor ownership
  -> continue the same resume episode
```

No suspension occurs while the C frame is active, and the logical frame cannot
migrate until the call returns. The M and executor stack are temporarily
pinned by the C call. Other logical goroutines make progress on a replacement
M and executor.

The first correct implementation may reuse narrow parts of
`entersyscallblock` and `exitsyscall`. It must not route through
`runtime.cgocall` or `runtime.asmcgocall`. A later optimization can replace
general syscall state with a coroutine-specific handoff once equivalent GC,
trace, stop-the-world, and scheduler behavior is proven.

Replacement creation must be demand-driven and bounded. Each replacement that
also blocks may create another demand, subject to a process limit. Exceeding
the limit needs a defined backpressure policy; it must not silently deadlock
with a held scheduler lock.

### 10.4 `AsyncOperation`

The asynchronous path is:

1. allocate or reserve an owner operation record;
2. copy, pin, or root arguments according to the recipe;
3. call the nonblocking submit function through the direct System ABI path;
4. if it completes synchronously, materialize the result without suspension;
5. otherwise store the next frame state and return a suspend action;
6. let the C callback, event source, or worker publish only the operation
   identity and pointer-free completion payload;
7. let the scheduler owner resolve the fact and push the logical G;
8. consume or discard the result before releasing the operation record.

This call class is a suspend seed. Its effect propagates through ordinary
synchronous callers automatically.

The completion ingress must not call arbitrary Go code or resume a frame. If a
native callback thread is unavoidable, a small root adapter attaches enough
runtime state to publish the fact and ring the doorbell, then returns.

### 10.5 System ABI lowering

The long-term implementation should teach the compiler and machine backends
about a supported System ABI call. It needs:

- target-specific argument and result classification;
- C caller-saved and callee-saved register sets;
- stack alignment and red-zone rules;
- symbol relocation for external linking;
- errno or platform-error capture when requested;
- unwind and traceback metadata;
- liveness across an opaque foreign call;
- explicit no-callback and pointer policies.

The first implementation may use a compiler-generated, signature-specific
leaf ABI shim. Such a shim may rearrange registers and call the C symbol, but
it must not allocate an argument-frame object, call a generic cgo wrapper,
switch to `g0`, or invoke `runtime.cgocall`.

The MVP must not freeze a generic `uintptr` call API. A typed call description
is necessary for correctness and for eventual direct call-site lowering.

### 10.6 Declaration frontend

The MVP uses compiler-internal metadata for a fixed set of test declarations.
It does not introduce a public `//go:` directive.

A later design can let `cmd/cgo` remain a header and declaration frontend while
emitting typed direct-call metadata for safe `nocallback` signatures. That
would remove runtime cgo call overhead without requiring users to abandon
`import "C"`. Unsupported declarations may retain the ordinary cgo path only
when explicitly selected; the coroutine direct-call mode should not silently
fall back and invalidate performance or stack assumptions.

### 10.7 Pointer and callback safety

The direct path preserves, rather than weakens, Go's foreign-pointer rules.

The MVP accepts only scalar values and foreign pointers. It does not pass a Go
pointer to memory containing Go pointers.

For later typed calls:

- synchronous borrowed Go memory remains live through the call;
- asynchronously retained memory is copied to foreign storage or explicitly
  pinned and rooted until terminal acknowledgement;
- a C result pointer must have declared foreign or borrowed provenance;
- callbacks are separate root adapters with explicit reentry and affinity
  rules;
- C must not unwind or `longjmp` through a Go frame.

## 11. Language and runtime integration

### 11.1 Panic, defer, recover, and Goexit

These controls cannot rely on native stack unwinding across a suspension.
Every frame eventually needs explicit cleanup states and a typed terminal
outcome.

The MVP initially supports normal return only and rejects a suspendable
function containing unsupported defer, recover, Goexit, or panic edges. Later
phases add:

1. fixed direct defer sites;
2. dynamic frame-owned defer records;
3. panic propagation and recover;
4. Goexit cleanup;
5. implicit faults and complete standard-library behavior.

### 11.2 Channels and synchronization

Channel and select operations become typed operation recipes. A channel
producer may publish readiness or a committed result, but it may not resume a
frame directly. Mutexes, semaphores, notes, and condition variables need the
same park/wake boundary.

The MVP does not need full channel semantics before proving yield, timer, file,
and network paths. It does need a scheduler-internal ready queue that is safe
under concurrent foreign completion.

### 11.3 Timers

Timer completion publishes a stable operation identity and requests the owner
executor. The timer callback cannot invoke the continuation.

The first timer path should reuse the existing runtime clock and timer heap
where practical. It needs an adapter from timer expiry to a coroutine
operation record, followed by an ordinary push wake.

### 11.4 File I/O

Regular files cannot be treated as ordinary readiness sources on POSIX,
because poll APIs often report them continuously ready even when an operation
can block.

The initial file path uses a bounded blocking-operation worker or a
`DirectMayBlock` execution domain:

```text
logical G prepares typed request
  -> worker or replacement executor performs blocking read
  -> result and errno are published under an operation identity
  -> owner pushes the logical G
```

A later platform backend may use `io_uring`, completion ports, or another true
asynchronous file API without changing the compiler suspend protocol.

### 11.5 Network I/O

Sockets remain nonblocking. A read or write tries the direct syscall first. On
`EAGAIN`, the logical G registers an operation, returns to the scheduler, and
is pushed after the poller publishes readiness. The resumed code retries the
operation.

This path needs close, deadline, stale descriptor generation, simultaneous
read/write, and cancellation transactions. The MVP covers a narrow TCP echo
path before broad `net` package conformance.

### 11.6 Thread affinity

`runtime.LockOSThread`, foreign thread-local state, UI runtimes, and some
callbacks require an execution-affinity flag separate from suspend effect.
Affinity binds a logical G to an executor/M identity, not to a retained native
stack frame.

The MVP excludes user-visible `LockOSThread`, but the scheduler data model must
not make later affinity impossible.

### 11.7 Compatibility delta checklist

Moving logical continuations out of native stacks changes more than scheduling.
The following areas need an explicit experiment policy:

| Area | Stackless difference | Required treatment |
| --- | --- | --- |
| Stack growth | executor stack is not copied | bound, guard, diagnose unknown depth |
| Recursion | unbounded native recursion defeats the bound | transform, prove a bound, or diagnose |
| Preemption | no arbitrary suspended native activation | compiler safe points and push yield |
| GC roots | suspended locals are not on a G stack | typed frames, barriers, scheduler roots |
| Syscalls | blocking call holds executor/M | release permit, replace executor, exact return |
| Direct C | no cgo wrapper or `g0` switch | System ABI plus explicit foreign-state protocol |
| Callbacks | cannot resume a retained C/Go stack chain | root ingress publishes an operation ID |
| TLS and affinity | a logical G may migrate between episodes | explicit executor/M affinity |
| Signals | interrupted PC may be C | mark external PC; audit signals and traceback |
| Panic/defer | native unwind cannot cross suspension | frame cleanup states and typed outcome |
| Stack inspection | physical frames omit suspended callers | logical parent-chain traceback |
| Assembly | body may hide stack, calls, or blocking | effect and stack-bound declaration |
| Race/sanitizers | cgo wrappers add synchronization | direct-path hooks and mixed tests |
| Profiling/trace | samples see a shared executor | attribute episode to active logical G |
| Reflect/function values | plain or coroutine entry | typed descriptor; never infer from a PC |
| Linkname/plugins | hidden ABI or calls may differ | experiment ABI and fail-closed boundaries |
| Runtime metrics | G and stack counts differ | logical-G, frame, executor, blocked-M metrics |

## 12. Debugging and tooling

A physical stack no longer represents the complete logical call chain. The
runtime must eventually reconstruct logical stacks from frame headers and
parent links.

Required later work includes:

- source PC and inline metadata for every state;
- traceback traversal across frame parents;
- panic output and `runtime.Callers`;
- debugger presentation of frame locals;
- profiler samples that associate an executor PC with the active logical G;
- race detector task identity and happens-before edges;
- coverage counters that remain associated with the source function;
- execution trace events for suspend, wake, migration, and foreign blocking.

The MVP provides state names and source positions sufficient for failures in
its fixtures. It does not claim debugger compatibility.

## 13. Feature and toolchain identity

`GOEXPERIMENT=coro` remains the only user-visible switch during development.
Internal `-d=coro=` levels may select diagnostics or temporary MVP stages, but
must not become a second semantic configuration.

With the experiment disabled:

- no coroutine analysis changes optimization decisions;
- no frame types or adapters are emitted;
- the existing runtime scheduler and stack growth remain in use;
- current cgo behavior remains unchanged.

With the experiment enabled:

- package export data carries a versioned coroutine summary;
- build action identity includes the experiment;
- incompatible archives are rejected;
- the coroutine runtime and ABI version are checked at link time.

The `cpunion/go:main` branch should continue to merge Go development versions.
Coroutine changes should remain in reviewable packages and hooks so upstream
merges resolve policy differences locally.

## 14. MVP

### 14.1 Definition

The MVP is an architecture proof, not a production runtime. It is complete
only when all of the following run on Linux/amd64 and Darwin/arm64:

1. many logical goroutines suspend and resume without one native stack each;
2. a compiler-inferred caller suspends through a child call;
3. a nonblocking C function is called through the direct System ABI path;
4. while one direct C call blocks, another logical goroutine makes progress on
   a replacement native thread;
5. an asynchronous C submit call returns to the scheduler and its completion
   pushes the waiting logical goroutine;
6. a timer fires through the same operation protocol;
7. a regular-file read completes through a blocking worker or compensated
   execution domain;
8. a nonblocking TCP echo exchange parks on readiness and resumes;
9. the equivalent narrow paths are exercised through ordinary `time`,
   `os.File`, and `net` source APIs before MVP sign-off.

Items 1 through 5 form the early go/no-go gate. Timer and I/O are required for
the MVP because they test whether the design is an execution model rather than
only a transformed `yield` example.

### 14.2 Restricted source subset

The first automatic lowering supports:

- closed direct calls;
- scalar parameters and results;
- local variables whose addresses do not escape except into their typed
  coroutine frame;
- `if`, simple loops, ordinary return, and explicit compiler test operations;
- one logical root and spawned roots;
- normal completion.

It initially rejects:

- open interface or function-value calls;
- defer, recover, Goexit, and suspension across panic handling;
- reflection;
- closures with escaping environments;
- variadic Go or C calls;
- assembly with unknown stack or suspend effects;
- C callbacks into arbitrary Go;
- C++ exceptions and `longjmp`.

Every rejection should be a deterministic experiment diagnostic, not a
miscompile.

### 14.3 Foreign fixtures

The direct-call tests use a small separately compiled C object with functions
similar to:

```c
uint64_t coro_add_u64(uint64_t a, uint64_t b);
void coro_block_until(uint32_t *gate);
int coro_submit_u64(uint64_t op_id, uint64_t value);
```

The exact asynchronous callback representation may use two 32-bit words for
the operation identity on 32-bit-compatible boundaries. The C fixture cannot
retain a Go pointer and cannot call a Go continuation.

The C object is linked through the platform external linker. Using a C compiler
for the fixture does not invalidate the runtime-overhead proof. The generated
Go call path must not contain a cgo wrapper.

### 14.4 MVP runtime objects

The initial runtime needs only:

- a typed logical-G header;
- a typed per-function frame;
- a parent-owned completion record;
- one ready queue;
- a fixed executor pool;
- one operation source with generation-checked slots;
- a pointer-free producer mailbox;
- a coalescing doorbell;
- a bounded blocking worker or replacement-executor handoff;
- timer and poll adapters.

The first queue may use a simple lock. Lock-free queues, work stealing, and
multi-P scaling are not prerequisites for proving ownership.

### 14.5 Implementation sequence

Each step should be independently reviewable and keep the default toolchain
green.

#### M0: Freeze native plans

- Retain the existing effect-analysis experiment.
- Add explicit call classes and operation recipes to the program model.
- Add verifier tests and package-summary round trips.
- Demote the alternate-backend SSA hook to a verifier, or remove it when it
  has no native consumer.
- Make no code-generation change.

#### M1: Generate one stackless state machine

- Lower one restricted suspendable function before escape analysis.
- Generate a typed frame and resume function.
- Add the push scheduler trampoline and child completion record.
- Run many `yieldOnce` logical goroutines.

Go/no-go evidence:

- no recursive `resume` loop;
- no native activation survives a suspension;
- logical task memory scales with frame size;
- one executor stack services many logical goroutines.

#### M2: Add native executor stacks

- Associate one executor G with each participating OS thread stack.
- Give `g0` a separate bounded runtime scheduler stack.
- Audit M bootstrap, teardown, signal, traceback, and system-stack switching.
- Add stack-bound verification for the restricted call graph.
- Reject `morestack` on an executor stack.
- Preserve GC, preemption, and stack traceback safety for the supported path.

Go/no-go evidence:

- the executor SP is within the OS-reported thread stack bounds;
- guard-page overflow fails deterministically;
- generated resume hot paths contain no `morestack` call;
- GC stress while tasks are suspended preserves frame pointers.

#### M3: Add direct System ABI calls

- Add one scalar signature on Linux/amd64 and Darwin/arm64.
- Implement `DirectNoBlock` external-state bookkeeping.
- Implement `DirectMayBlock` permit release and replacement progress.
- Implement one `AsyncOperation` submit and completion ingress.

Go/no-go evidence:

- disassembly shows a direct C symbol call or a compiler-generated leaf ABI
  shim;
- the hot path has no `runtime.cgocall`, `runtime.asmcgocall`, `_cgo_Cfunc_*`,
  or cgo-generated C wrapper;
- a blocking C call does not stop another logical G at an execution quota of
  one;
- callback or worker completion carries only operation identity and scalar
  payload;
- automatic effect propagation colors a caller that awaits the async
  operation.

#### M4: Add timer and regular-file paths

- Connect one runtime timer operation to the owner source.
- Add a bounded blocking file worker or compensated direct syscall path.
- Cover success, timeout, cancellation, stale completion, EOF, and errno.
- Exercise a narrow `time.Sleep` and `os.File.Read` source path.

#### M5: Add nonblocking network path

- Connect a poll descriptor to a generation-checked operation.
- Retry nonblocking socket operations after readiness.
- Cover close, deadline, stale readiness, and simultaneous logical work.
- Exercise a loopback TCP echo through a narrow `net` source path.

#### M6: Harden and measure

- Run default and experiment compiler/runtime tests.
- Add race-oriented operation-state tests.
- Record memory, direct-call latency, switch latency, binary size, and thread
  counts.
- Remove temporary duplicate paths and unused experiment code.
- Document unsupported source with compiler diagnostics.

### 14.6 Tests

The minimum test layers are:

1. analysis unit tests for effect, execution class, summaries, and bad
   declarations;
2. lowering tests for frame fields, states, exact evaluation, and child calls;
3. runtime unit tests for queue ownership, operation generation, duplicate
   completion, cancellation, and replacement handoff;
4. compiler end-to-end tests that build and run experiment programs;
5. object inspection tests for forbidden cgo symbols and expected external
   calls;
6. GC stress tests with pointers live across suspension;
7. Linux/amd64 and Darwin/arm64 CI;
8. unchanged default `make.bash`, `go test cmd/compile/...`, and relevant
   runtime tests.

The experiment tests should use deterministic fake operations where possible.
Wall-clock and real socket tests are a separate, small integration layer.

### 14.7 Benchmarks and measurements

The direct-call benchmark compares the same non-inlined scalar C function
through:

- ordinary cgo;
- the new `DirectNoBlock` path;
- a pure Go call as a lower-bound reference.

Report at least:

- nanoseconds per call;
- allocations per call;
- generated text size;
- wrapper and runtime symbols in the call graph.

The scheduling benchmark reports:

- memory for 1, 1,000, and 100,000 suspended logical goroutines;
- bytes per logical G and per live frame;
- executor/native-thread count;
- yield, child-await, timer, file, and net wake latency.

The blocking test records that a second logical G completes while the first M
is in C. A result that only completes after the C call returns is a failure,
even if the program eventually exits.

### 14.8 MVP acceptance

The MVP is accepted only if:

- both target platforms pass;
- the default toolchain remains unchanged and green;
- all supported suspension paths are push-driven;
- no supported suspended logical G owns a native stack;
- direct nonblocking and async-submit C calls avoid the cgo transition path;
- blocking C compensation works at managed execution quota one;
- GC stress and operation-race tests pass;
- timer, file, and TCP probes use the common operation protocol;
- unsupported behavior is rejected rather than silently falling back;
- the implementation has no LLVM build or runtime dependency.

## 15. Work after the MVP

The likely order is:

1. complete panic, defer, recover, Goexit, and implicit fault lowering;
2. add channels, select, mutexes, semaphores, and runtime notes;
3. generalize System ABI type classification and errno handling;
4. let `cmd/cgo` emit typed fast-path metadata for safe declarations;
5. add dynamic function values, interfaces, closures, generics, and reflect;
6. add precise logical traceback, debugger, profiler, trace, race, and
   coverage integration;
7. add work stealing, affinity, dynamic executor sizing, and bounded blocking
   policies;
8. broaden `time`, `os`, `net`, `internal/poll`, and standard-library tests;
9. evaluate deeper integration with the Go scheduler after the nested
   executor model is measured;
10. add other targets only after their platform operation sources and stack
    contracts are explicit.

## 16. Decisions to confirm before implementation

The recommended MVP choices are:

1. Use a dedicated executor G on the OS thread stack, give `g0` a separate
   runtime stack, and do not run arbitrary Go code under the `g0` identity.
2. Keep the existing Go scheduler as the physical M/P/GC scheduler during the
   MVP and add a bounded stackless scheduler above it.
3. Use internal typed foreign metadata in the MVP; do not freeze a public
   directive.
4. Support only scalar and foreign-pointer C signatures without callbacks.
5. Classify supported unknown-duration C calls as `DirectMayBlock`; reject
   unsupported signatures instead of silently using cgo.
6. Permit a compiler-generated leaf ABI shim in the first direct-call slice,
   while making direct call-site System ABI lowering the long-term target.
7. Require timer, regular-file, and TCP paths before calling the work an MVP.

The most important technical risk is the executor stack. It must be a normal
GC-visible Go execution context on the native OS stack, fixed and C-compatible,
without allowing `morestack` or arbitrary unbounded call depth. Moving
runtime-scheduler work to a separate `g0` stack affects M bootstrap, signals,
traceback, syscall state, and teardown. This layout and its
compile-time/runtime bound should be proven before broad state-machine
lowering.

The second risk is foreign state. A direct call can remove wrapper and stack
switch overhead, but nonblocking, blocking, and async calls require different
scheduler and preemption protocols. Combining them into one generic
`uintptr`-based call would lose both safety and the intended performance.
