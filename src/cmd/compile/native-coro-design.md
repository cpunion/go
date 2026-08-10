# Native Stackless Coroutine Design

Status: restricted MVP implemented behind `GOEXPERIMENT=coro` and
`-d=coro=4`; transparent scalar and bounded pointer-free aggregate cgo direct
calls including the two-result errno form, fixed and repeated
simple-loop defer cleanup for normal return and explicit panic, direct recover
and replacement panic in fixed or repeated local defer literals and
statically resolved local or imported named defer targets, typed structured
panic propagation, and implicit runtime panic capture across lowered and
ordinary synchronous calls; sticky Goexit cleanup and propagation including
fixed or repeated local defer literals and statically resolved local or
imported named defer targets; direct deferred Goexit; operation-progress
follow-ups; nested multi-result and conservatively normalized single-result
expressions; and restricted compiler-private cross-package factory and defer
entries; ordinary channel send, single-value receive, comma-ok receive, and
receive-channel range use the runtime's shared channel wait queues without an
operation goroutine; channel select uses one task-owned arbitration record and
shared wait-queue entries without a goroutine per case;
not production-ready

Last updated: 2026-07-30

Upstream mirror: `cpunion/go:main`, which remains aligned with the Go
development branch

Integration branch: `cpunion/go:coro/main`, which receives upstream updates
from `main` and is the base for coroutine pull requests

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

LLGo is used as a risk inventory, not as a contract or implementation source.
The Go experiment keeps only the minimal execution invariants that are needed
for correctness:

- a logical goroutine is a chain of stackless frames;
- the scheduler is the sole continuation executor;
- an operation producer owns a stable operation record, not a frame;
- child completion is stored in a parent-owned record;
- suspend, resume, result, cancellation, and destruction are exact
  transactions;
- blocking foreign calls may occupy native threads, while replacement native
  threads continue managed work;
- a completion source publishes a fact through one scheduler boundary.

Capabilities are added only when an executable example demonstrates the need.
They should use existing Go compiler and runtime mechanisms, or one small
typed internal interface. The experiment does not copy LLGo's annotation
vocabulary, per-call policy matrix, or parallel runtime abstraction.

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
- avoid the general cgo Go and C argument-frame wrappers;
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

A stackless coroutine executor changes the first premise and the cost structure
of the second. A resume episode can run on one fixed, nonmoving, C-compatible
executor stack. A potentially blocking call must still remain on its current M
while a replacement M runs managed work. Before entering C, however, it can
release its managed execution permit without saving, moving, or switching away
from a goroutine stack: the logical continuation is already in its typed heap
frame. It is therefore unnecessary to pay for a cgo wrapper and a `g0` stack
switch on every supported call.

The remaining responsibilities do not disappear. They become explicit,
smaller foreign-entry and foreign-exit protocols for each call class.

## 5. Core invariants

The compiler and runtime must enforce these invariants in every phase.

1. A suspended logical goroutine has no live native activation.
2. A frame has exactly one scheduler owner and at most one active resume
   executor.
3. A frame is resumed or destroyed only after an exact scheduler action is
   issued for it.
4. A producer carries only pointer-free operation identity and payload allowed
   by its source contract.
5. A producer cannot call `resume`, `destroy`, or a user callback.
6. A completion fact is sticky until the owner acknowledges it. Completion
   before an active resume returns cannot make the same task runnable early.
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

The MVP implements this runtime layout change. At M initialization, the
runtime starts on the OS stack as it does today. Before managed coroutine
execution, it provides a separate internal scheduler stack for `g0`,
associates the OS stack with the executor G, and switches between them at
runtime scheduling boundaries.

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
	coroActionPanic
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

The runtime records whether a resume call still owns the task. If an operation
finishes after changing the task to `waiting` but before that call returns its
`Wait` action, completion sets one sticky pending-ready bit. The task is not
queued until the scheduler consumes the returned action and releases the
resume owner. This closes the otherwise possible window in which two
executors could enter the same resume function concurrently.

Runtime mutexes are not synchronization events visible to the race detector.
Every completed resume episode therefore release-merges its task identity, a
completion producer release-merges its result publication, and the next
executor acquires that identity after claiming the runnable token. The root
owner also acquires the terminal task before returning to ordinary Go code.

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

This protocol carries return, panic, Goexit, abort, and shutdown as
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

The logical wait ticket is separate from the physical operation identity. A
logical wait may own one arbitration record and several physical source
registrations; a future mixed-source select may also need several operation
identities. The implemented channel select uses one identity shared by all of
its channel waiters. A physical source may remain active after another
candidate wins. The wait ticket stays in scheduler-owned logical-G state and
never enters the producer ABI.

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

The MVP adds a runtime-supported executor G whose fixed,
guard-paged, noncopying stack is the OS thread stack. Generated coroutine code
and its allowed plain callees run on this stack. Runtime scheduler work runs on
a separate internal `g0` stack.

The compiler must not emit a path that calls `morestack` for a frame already
running on an executor stack. The MVP can prove a static maximum for its small
call graph and reject a program that exceeds the configured executor-stack
budget.

The production problem is broader:

- direct and mutually recursive calls in a lowered Go call component use child
  factories instead of native recursion;
- indirect calls need a summary bound or a conservative capability;
- assembly needs an explicit stack bound;
- foreign calls need a documented C-stack bound or a sufficiently large
  guarded reservation;
- panic and signal paths need reserved emergency space.

Globally applying `NOSPLIT` to arbitrary Go code is not a solution. It would
replace controlled stack growth with unchecked overflow.

### 9.3 MVP stack gate

The MVP uses the OS thread stack and discovers its real bounds through the
existing per-platform runtime initialization. It allocates a separate bounded
runtime scheduler stack for `g0`.

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

The implementation reuses `reentersyscall` and `exitsyscall` for the scheduler
and GC state transition. The combined blocking-entry helper explicitly saves
its generated resume caller as the syscall PC, SP, and frame pointer. That
resume frame remains active until the combined exit helper calls
`exitsyscall`, including on its slow return path. The helpers also open-code
the no-callback foreign state transition, so each side crosses one runtime
boundary. The call does not route through `runtime.cgocall` or
`runtime.asmcgocall`.

`entersyscallblock` is not an unconditional improvement for this execution
model. Calling it inside a helper saves a helper frame that is invalid after
the helper returns. Emitting it directly in the resume frame is correct and
hands off the P immediately, but a fixed native-stack executor is locked to
its original M. If another M owns the P when C returns, that executor must take
the locked-M slow return path. A coroutine-specific handoff remains a later
optimization, but it must reduce the forward handoff and return costs together
while preserving GC, trace, stop-the-world, and scheduler behavior.

This path does not eliminate M replacement. Its intended advantage over
ordinary cgo is a cheaper handoff: the old M remains in C as before, but the
logical continuation has already been materialized and the call needs neither
a movable-G-stack save nor a switch to `m.g0`. The direct trampoline also
avoids the general cgo argument frame, callback bookkeeping, and
`runtime.cgocall` dispatch. This is a reduction in transition cost, not a
claim that a blocking C function stops occupying a native thread.

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

On System V AMD64, each shim must dynamically align SP to 16 bytes before the
C call and restore the Go SP afterward. The Go internal ABI only guarantees
8-byte stack alignment, so a fixed-size shim frame cannot satisfy both possible
caller alignments.

The MVP must not freeze a generic `uintptr` call API. A typed call description
is necessary for correctness and for eventual direct call-site lowering.

### 10.6 Declaration frontend

The first MVP used compiler-owned declarations for fixed test fixtures. The
transparent-call follow-up keeps `cmd/cgo` as the source and C declaration
frontend. For a supported declaration, `cmd/cgo` emits versioned,
compiler-private metadata containing:

- the generated Go wrapper and direct-entry identities;
- the external C symbol;
- the conservative call class;
- the scalar, pointer, and indirect aggregate bridge shape;
- whether the two-result form captures errno.

This metadata is generated only for `GOEXPERIMENT=coro`. It is consumed by the
compiler and is not passed to the linker as a cgo directive. It does not change
the ordinary cgo path until native lowering has selected the direct entry.

The generated metadata supports non-variadic integer, pointer, `float`, and
`double` signatures, plus pointer-free struct parameters and results up to 128
bytes with at most eight-byte alignment. Integer and floating-point argument
registers are classified independently, as required by both target System
ABIs. Linux/amd64 accepts up to six integer or pointer arguments and eight
floating-point arguments; Darwin/arm64 accepts up to eight of each. An
indirect aggregate consumes one integer argument register.
A declaration must use the existing `#cgo nocallback` contract. The existing
`#cgo noescape` directive remains an independent escape-analysis optimization.
Unsupported declarations retain the ordinary cgo path in compatibility mode.
Integration tests assert both the metadata and the selected symbols in
disassembly, so a fallback cannot silently become a direct-call performance
result.

The assembly entry passes each supported aggregate's stable Go ABI result or
argument slot address to the typed C bridge. The bridge dereferences aggregate
arguments into the target's by-value call and writes an aggregate result
through a hidden result pointer. The platform C compiler, rather than a
partial Go-side classifier, therefore implements the target function's SysV
AMD64 or AAPCS64 aggregate ABI. This internal pointer is not exposed to the
target function. Structs containing pointers, unions, classes, zero-sized,
over-aligned, and larger aggregate values retain ordinary cgo. If the bridge
shape exhausts integer registers it also retains ordinary cgo.

The cgo two-result form requires a split result protocol. The C bridge clears
errno, calls the target, and captures errno before doing any other work. For a
non-void target, the direct entry passes a hidden pointer to the Go result slot
and the bridge writes the C result through that pointer. The bridge returns raw
errno as `size_t` in the ordinary integer result register. This avoids a
platform-specific aggregate return. The hidden pointer participates in
argument-register classification; a declaration falls back to ordinary cgo
when no register remains. For aggregate results the bridge captures errno
before copying its local value into the Go result slot, so a compiler-generated
copy cannot clobber the reported error.

The generated Go direct declaration exposes the raw pair as
`(result, syscall.Errno)`. Lowering stores that pair while the M is still in
foreign-blocking state, leaves that state, and only then converts a nonzero
`syscall.Errno` to the source error interface. A zero value becomes nil.
Keeping interface conversion outside the foreign window is required because
it may allocate or otherwise enter the Go runtime.

`cmd/cgo` emits a typed external bridge in the same C translation unit as the
preamble. This makes `static` and inline declarations visible to the generated
assembly entry without using the general argument-frame wrapper. Scalar bridge
entries have the exact C signature. Aggregate entries use typed pointers only
at the generated bridge boundary and call the target with its exact by-value
signature. Object inspection verifies that the hot path still contains no
`runtime.cgocall` or `runtime.asmcgocall`.

The transparent path is currently emitted only for Darwin/arm64 and
Linux/amd64. Race, memory-sanitizer, and address-sanitizer builds deliberately
ignore the direct metadata and retain ordinary cgo, because the direct
executor path does not yet provide equivalent sanitizer hooks.

### 10.7 Pointer and callback safety

The direct path preserves, rather than weakens, Go's foreign-pointer rules.

The MVP accepts scalar values, foreign pointers, and bounded pointer-free
structs. Aggregate slot addresses are used only by the synchronous generated
bridge, which copies arguments and results by value; the target function
cannot retain those internal pointers. The direct path does not pass a Go
pointer to memory containing Go pointers.

For later typed calls:

- synchronous borrowed Go memory remains live through the call;
- asynchronously retained memory is copied to foreign storage or explicitly
  pinned and rooted until terminal acknowledgement;
- a C result pointer must have declared foreign or borrowed provenance;
- callbacks are separate root adapters with explicit reentry and affinity
  rules;
- C must not unwind or `longjmp` through a Go frame.

### 10.8 Annotation policy

The Go implementation does not adopt LLGo's distributed `//llgo:coro`
directives. Review of `cpunion/llgo:llvm-coro` found that scheduler, worker,
reentry, affinity, memory, result, and foreign-progress policy had spread into
source annotations and a multi-part contract vocabulary. Porting that
vocabulary would move its maintenance burden into the Go tree.

Instead, suspension and executor requirements propagate through the compiler
call graph from compiler-owned operations and versioned C-boundary metadata.
The frontend reuses only the existing `#cgo nocallback` and `#cgo noescape`
contracts. No Go source annotation is required for coloring, scheduler waits,
worker dispatch, result projection, or terminal propagation.
`runtime.Goexit` is recognized by the same centralized operation-recipe table
as `runtime.Gosched` and `time.Sleep`; it does not create another source
contract.

Most synchronous supported C calls are conservatively `DirectMayBlock`.
Neither a C signature nor `#cgo nocallback` proves bounded execution time. The
first transparent-call MVP therefore adds no public nonblocking directive.
`DirectNoBlock` is measured with compiler-owned fixtures. A later proposal may
add one optional C-boundary contract if the measured benefit justifies it.

Asynchronous completion semantics cannot be inferred from a C signature.
Those APIs use small typed adapters that create an operation record and become
suspension seeds. Their Go callers are still colored automatically. Worker
arity, result projection, and propagation are derived from types and operation
recipes, not maintained as source annotations.

### 10.9 Cross-package entry boundary

Ordinary callers continue to call the exported Go entry, which creates and
runs one coroutine root. A lowered caller instead uses a compiler-private
factory entry when the imported function summary advertises a compatible
capability. This avoids recursively entering an ordinary wrapper from a native
executor while preserving the normal Go ABI for all existing callers.

The current Unified IR summary format records one `FactoryABI` value. Version
1 derives all other information from the imported Go function type:

```text
func(P0, ..., Pn) (R0, ..., Rm)
    -> func(P0, ..., Pn, *R0, ..., *Rm) func(unsafe.Pointer) uint8

func(P0, ..., Pn)
    -> func(P0, ..., Pn) func(unsafe.Pointer) uint8

func (T) M(P0, ..., Pn) (R0, ..., Rm)
    -> func(T, P0, ..., Pn, *R0, ..., *Rm) func(unsafe.Pointer) uint8

func(P0, ..., Pn, values ...V) R
    -> func(P0, ..., Pn, values []V, *R) func(unsafe.Pointer) uint8
```

The emitted symbol is the ordinary package symbol plus the compiler-reserved
`.coro` suffix. The summary does not carry a symbol string, arity, call policy,
worker policy, or source annotation. The importer reconstructs the type and
symbol only after validating the capability.

Factory ABI 1 is intentionally narrow. It supports package functions and
concrete methods without generic shapes. A method receiver is the first
explicit factory parameter, followed by the ordinary parameters and one typed
pointer per result. Type checking has already normalized a variadic `...V`
parameter and every call site to an explicit `[]V`, which is the factory
parameter type. Interface calls, method values, closures, generic shapes,
missing capabilities, and the legacy summary format do not produce an
imported factory reference. The caller is removed from the lowering candidate
fixed point and retains its ordinary source body. The same fallback propagates
to its lowered callers, so an unsupported leaf cannot leave a partially
transformed call chain.

Factories for every accepted function in a local recursive call component are
created before any body is lowered. Direct recursion, mutual recursion, and
concrete recursive methods therefore await another factory-produced child
without recursively entering a public wrapper or consuming native stack per
logical frame.

The same summary has a separate `DeferABI` capability. Version 1 certifies
that a statically resolved named target may run during coroutine cleanup. A
recover-only plain primary uses its ordinary entry so Go's required direct
call relationship is preserved. A primary that may panic or call Goexit uses
Factory ABI 1 and the nested defer scheduler. Its entire reachable coroutine
component is proven non-suspending, free of execution constraints and
stackless `go` edges, and itself covered by Defer ABI 1. This stricter
fixed-point proof is independent of ordinary factory availability: a function
can be safe as a structured child but unsafe to enter while its parent is
unwinding.

The capability permits terminal effects to pass through local or imported
direct-call edges. It deliberately does not permit recover to pass through an
ordinary helper call: Go only allows recover when it is called directly by
the deferred function. Missing and unknown defer capabilities fail closed,
just like missing factory capabilities. The compiler-generated local wrapper
that snapshots defer arguments is not itself an ABI boundary; the importer
unwraps it to the certified named target.

The signature extensions above do not require another factory ABI value. An
older importer already rejects a version-1 factory when the Go signature has
multiple results, has a receiver, or is variadic, while older archives never
advertise a factory for these extensions. Mixed compiler versions therefore
retain the ordinary entry in either direction.

No-result calls and discarded results are supported. A discarded awaited
result uses a typed parent-frame slot. Direct assignments may receive multiple
results when every non-blank target is a variable. The Go frontend represents
a multi-result call used as another call's argument list with a typed
assignment in the surrounding expression's `Init` list. Lowering recognizes
that direct owner, suspends at the inner call, and resumes the enclosing
expression with the same result temporaries. This covers nested multi-result
calls in returns, call statements, and assignments to variables without
changing Factory ABI 1.

Single-result calls do not have a frontend-generated `Init` assignment.
Before escape analysis, the coroutine package conservatively normalizes a
nested awaited call into a typed temporary assignment on the enclosing
statement's `Init` list. Calls are considered in postorder, so sibling awaits
and nested coroutine call chains become an explicit sequence without invoking
the later `walk.order` pass. The ordinary escape and walk passes then see the
same typed calls and temporaries as the coroutine transform.

The normalization proceeds only when no observable operation precedes the
hidden call. A short-circuit right operand, a nested call following an
effectful operand, a `for` condition or post statement, and an assignment with
an effectful left operand retain the ordinary entry. Moving a hidden call
ahead of an index, dereference, or other effectful destination would change Go
evaluation order; those forms require explicit destination stabilization or
control-flow splitting before suspension. A spawned call with results is
rejected because the child may outlive the parent slot.

Summary version 2 carries effect and execution requirements, version 3 adds
the factory capability, version 4 adds `MayPanic`, `UsesRecover`, and
`MayGoexit`, and version 5 adds the defer-entry capability. All older versions
decode with absent newer capabilities. Unknown factory or defer ABI values
are treated as unavailable; an unknown summary format version is rejected as
incompatible. This lets explicit terminal effects propagate through imported
structured calls while stale objects fail closed without a compatibility
policy matrix.

The measured implementation must not hide this cost by batching only. The
benchmark therefore separates:

- `Steady`: many foreign calls inside one already-created root;
- `Entry`: one exported Go call and one new root per foreign call.

The factory removes nested root creation inside a successfully lowered call
chain. It does not remove the one root at an ordinary program boundary, so the
`Entry` benchmark remains a separate cost and compatibility measurement.

## 11. Language and runtime integration

### 11.1 Panic, defer, recover, and Goexit

These controls cannot rely on native stack unwinding across a suspension.
Every frame eventually needs explicit cleanup states and a typed terminal
outcome.

The post-MVP cleanup slices support fixed direct defer sites and repeated
defer sites in simple loops on normal completion and explicit panic.
Encountering a fixed defer evaluates its call operands immediately and arms
one frame-owned site. A function with any repeated site instead uses one
frame-owned, typed LIFO stack of compiler-proven parameterless wrappers for
all of its defer sites. Each registration snapshots the evaluated captures;
the cleanup loop loads, clears, and pops an entry before invoking it. Every
return or explicit panic enters the same cleanup state. Normal return then
copies results to the caller. Panic instead returns a typed terminal action
without copying results.

An awaited child's terminal outcome is transferred under scheduler ownership
to the waiting parent. The parent enters its own cleanup state rather than its
post-call source state. A panic value reaches the root only after native
executors stop, where it is re-raised on an ordinary caller Go stack so an
enclosing ordinary defer can recover it. A separate outcome kind preserves
`panic(nil)` instead of confusing it with normal completion.

`runtime.Goexit` is a compiler-owned terminal operation recipe. It sets a
sticky task-owned Goexit state, jumps directly to the current frame's cleanup,
and propagates through each structured parent. A detached stackless `go`
target completes only that logical goroutine. At an ordinary root boundary,
the runtime invokes the real `runtime.Goexit`.

A panic raised by a defer while Goexit is active overlays, but does not erase,
the sticky Goexit state. Recovering that panic resumes Goexit cleanup. If the
panic reaches the ordinary root boundary, the runtime reconstructs the same
pending-Goexit relationship on the native Go stack before raising it, so an
ordinary outer recover cannot accidentally cancel Goexit.

Conditional fixed defer sites and repeated sites in simple loops are
supported. The compiler still rejects an open function-value target and a
suspending, unknown-effect, or execution-constrained defer target. A fixed or
repeated local defer literal may directly call `recover`, panic, or call
`runtime.Goexit`. Its terminal operations are rewritten to use a stable opaque
task token during cleanup, so recover clears the pending structured panic and
a later panic starts or replaces it. A rewritten Goexit clears the active
panic, sets sticky task-owned Goexit, and immediately returns from the literal.
That return runs the literal's own native defers without executing source
statements after Goexit.

A statically resolved recover-only named defer target retains its ordinary Go
entry. Cleanup invokes it inside a short runtime scope that exposes the
task-owned panic to `gorecover`; this preserves Go's requirement that recover
be called directly by the deferred function. An active native panic takes
priority, so ordinary Go panic and recover inside that call retain their
semantics, and native unwinding restores the scope. Local targets are proven
from their bodies. Imported targets must advertise Defer ABI 1.

A named target that may panic or call Goexit is a coroutine primary. It keeps
its public Go entry and lowers the same body to its private coroutine factory.
The compiler-generated defer wrapper invokes that factory through a nested
run-to-completion scheduler instead of recursively entering a new ordinary
root. Calls without arguments or results receive a private wrapper; frontend
wrappers retain exact argument snapshots, method receivers, variadic slices,
and ignored-result temporaries. The target may reach panic or Goexit through
local or imported structured calls when every coroutine-primary dependency
has both Factory ABI 1 and Defer ABI 1. An indirect call to a recover-capable
helper is rejected because accepting it would change recover's directness
semantics. Suspension, execution constraints, unknown effects, and stackless
`go` edges also invalidate the defer capability.

`defer runtime.Goexit()` is handled without a factory. The registration site
is not a Goexit transition; lowering synthesizes a private parameterless
wrapper that calls the task-owned `coroDeferGoexit` operation with the active
cleanup token.

The nested run establishes the parent's logical defer scope while the target
executes, so a direct recover in the named target can consume the parent
panic. The target's own defer cleanup uses its child task as usual. Completion
then merges one typed outcome into the parent: a normal return leaves the
parent outcome unchanged, Goexit clears a parent panic and becomes sticky, and
a child panic replaces the parent panic while preserving any pending Goexit.
This also preserves the Go rule that recovering a panic raised during Goexit
resumes Goexit.

The scope reuses the experiment's existing one-pointer `g` extension and does
not enlarge either `g` or the logical task. Native execution stores its state
in the fixed context pool; managed and sanitizer fallback allocate a temporary
scope only while invoking the named defer target.

Cleanup queries the runtime-owned outcome after every defer; `panic(nil)`
follows the ordinary Go default and `GODEBUG=panicnil=1` semantics. Missing or
unknown defer capabilities, open function values, invalid indirect recover,
and targets with unsupported effects retain the ordinary Go path.

Repeated source defer literals preserve Go's by-reference and
per-declaration-execution capture identity with typed cells. For each captured
logical variable, the factory owns a current `*T` cell slot. Parameter and
named-result cells are allocated at factory entry; a local receives a new cell
when its source declaration executes, including each loop iteration. All
lowered uses of the logical variable dereference the current cell. Registering
the defer snapshots the current cell pointer, not its value, into the literal.
Consequently, registrations from different iterations retain different
storage, literals registered in the same iteration share storage, mutations
after registration remain visible, and taking the variable's address retains
ordinary identity. The cells and snapshots remain typed GC roots.

This first slice covers declarations already represented in the supported
structured body and initialization forms. Directly capturing a `for` induction
variable can make the frontend loop-variable pass introduce an unlabeled
`break`; that function retains its ordinary entry until branch lowering is
implemented.

The same result target is used for an awaited coroutine assignment. An
ordinary `os.File.Read` or supported network read worker captures the typed
cell pointer and writes through it before publishing completion. A source
literal may directly recover, panic, or call Goexit; the existing terminal
rewrite is applied after its captures have been redirected through the
snapshotted cells.

The LIFO stack therefore admits compiler-generated wrappers and statically
known source literals whose captures have this cell representation; it does
not enable open dynamic calls. A nested closure inside a repeated literal that
recaptures one of these variables remains rejected until the cell pointer can
be propagated through every nested environment. A stackless `go` edge inside
a terminal named target and a `go` target that may explicitly panic also
retain the ordinary Go path.

A native panic raised by an implicit fault or an ordinary synchronous callee
is recovered at the queue-driving boundary, not by a defer installed around
every resume call. The active resume unwinds completely, the runtime records
the original panic value on its logical task, and that task is made runnable
again. Every generated resume that may execute a faulting operation, call
ordinary Go code, run a defer, or observe an awaited child's terminal outcome
checks for a pending terminal outcome before dispatch and enters its
frame-owned cleanup state. A conservative IR analysis retains the check for
unknown operations; a proven yield-only resume does not pay for it. A
structured child then transfers the panic to its parent as before. An
independent stackless `go` target runs its logical cleanup and re-raises an
unhandled panic on its executor, preserving process-terminating goroutine
semantics. A native panic raised during cleanup replaces the active panic and
overlays, without erasing, a pending Goexit. The recovery boundary does not
consume the runtime's own Goexit unwind marker.

Repeated-literal capture ownership is implemented. Nested closure-environment
propagation remains a separate general-closure task.

### 11.2 Channels and synchronization

Channel and select operations become typed operation recipes. A channel
producer may publish readiness or a committed result, but it may not resume a
frame directly. Mutexes, semaphores, notes, and condition variables need the
same park/wake boundary.

The first channel slice lowers ordinary sends, discarded or single-value
receives, and comma-ok receives with simple variable targets. It preserves the
frontend's normalized temporary assignments, including blank results and
defined bool conversions. A send evaluates the channel and value before
suspending, then retains the typed value in factory-owned, GC-scanned storage.
A receive writes typed result temporaries before publishing readiness.
Send-on-closed-channel panic is transferred to the logical task and enters the
same cleanup path as another asynchronous terminal outcome.

Channel range evaluates the channel expression once into frame-owned storage.
Each iteration receives into hidden typed value and status slots, assigns the
source iteration variable only after a successful receive, and returns to the
same receive state after the body. A closed receive leaves an existing
iteration variable unchanged. The hidden value is cleared before the next
receive when its type contains pointers, matching the ordinary compiler's
lifetime rule. Nested channel ranges and suspension in a range body use the
same state graph. Per-iteration variables whose addresses escape receive a
fresh typed cell on every iteration; a closure that captures such a variable
retains the ordinary entry until general closure-frame lowering exists.

Channel select evaluates every channel operand and send value once in source
order, stores channels and typed elements in the coroutine frame, and builds a
compiler-private descriptor array matching the runtime's two-pointer `scase`
layout. Runtime case indices group sends before receives as `selectgo` does;
explicit state dispatch maps the selected index back to the source case.
Receive values and comma-ok status are assigned only for the selected case,
and pointer-bearing case storage is cleared before its body runs. Buffered,
unbuffered, nil, duplicate, closed, default-only, empty, nested, and timer
channel selects are supported. Receive destinations remain restricted to
simple variables or blanks.

The runtime registers a stackless operation directly in the existing
`hchan.sendq` or `hchan.recvq`. Its `sudog` has no parked `g`; an
experiment-only owner points to the registry operation instead. Ordinary and
stackless waiters consequently share one FIFO, buffer accounting, close path,
timer-channel bookkeeping, and race synchronization. Matching copies values
with the channel's element type, releases the channel lock, releases the
`sudog`, and only then publishes the logical task to its scheduler. A producer
never invokes the continuation.

This removes the per-operation goroutine and its stack without introducing a
parallel channel implementation. A parked goroutine releases its own `sudog`
after wakeup, but a logical waiter has no resumed `g` to perform that cleanup.
It therefore uses a dedicated heap `sudog` that becomes garbage after the
waker removes and clears it, instead of returning the waiter to an ordinary
per-P cache from the waker's execution context. The non-experiment `sudog`
extension has zero size and leaves the 64-bit structure at 104 bytes; the
experiment-only owner pointer makes it 112 bytes.

A blocked logical select owns one operation record, one atomic arbitration
bit, and one `sudog` per non-nil case. The waiters enter the existing channel
queues in channel-address lock order. The first producer that removes a waiter
wins the arbitration, completes the channel transaction, then reacquires the
ordered channel locks to remove every losing waiter and balance timer-channel
bookkeeping. Duplicate cases on one channel use the same arbitration path.
Only after all waiters and descriptor references are cleared does the producer
publish one selected index and ready the task. No case owns a goroutine and no
producer invokes the continuation.

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

The `cpunion/go:main` branch should remain aligned with the Go development
branch. `cpunion/go:coro/main` periodically merges it. Coroutine changes should
remain in reviewable packages and hooks so upstream merges resolve policy
differences locally.

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
- ordinary channel send, discarded or single-value receive, and comma-ok
  receive with simple variable targets;
- receive-channel range with zero or one iteration variable, including nested
  range, suspension in the body, assignment conversion, and per-iteration
  address identity;
- one logical root and spawned roots;
- normal completion;
- conditional fixed direct defer sites and repeated defer sites in simple
  loops;
- explicit panic propagation through structured calls and fixed cleanup;
- implicit nil, bounds, divide, and synchronous-callee panics through
  structured calls;
- direct recover and replacement panic in fixed local defer literals and
  repeated source defer literals, and statically resolved local or imported
  named defer targets;
- direct Goexit cleanup through structured calls and isolated spawned
  logical goroutines;
- Goexit in fixed local defer literals, direct deferred Goexit, and statically
  resolved local or imported named defer targets, including repeated source
  literals, frontend argument and method wrappers, ignored results, repeated
  named registrations, target-owned cleanup, parent recover, replacement
  panic, and indirect terminal calls through certified structured
  dependencies.

It initially rejects:

- open interface or function-value calls;
- nested closures that recapture a repeated source literal's cell, indirect
  recover-capable helpers, missing or unknown defer-entry capabilities, a
  stackless `go` edge inside a terminal named defer target, and a spawned
  target that may explicitly panic;
- direct capture of a `for` induction variable when frontend loop-variable
  rewriting introduces unsupported branch control;
- closure capture of a per-iteration channel-range variable;
- reflection;
- closures with escaping environments;
- variadic C calls;
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
  or general cgo argument-frame wrapper;
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
- transparent `DirectMayBlock`, which is the conservative default;
- compiler-owned `DirectNoBlock`, as the upper bound for a future explicit
  bounded-call contract.

Each path is measured both in steady state inside one root and, where
applicable, with root entry on every exported call. Mixing the two would make
the result depend more on benchmark shape than on the foreign boundary.

Report at least:

- nanoseconds per call;
- allocations per call;
- generated text size;
- wrapper and runtime symbols in the call graph.

Measure bounded direct calls and potentially blocking calls separately. The
blocking comparison reports three distinct values:

- direct foreign-boundary cost for a nonblocking control call;
- time from the blocking C body publishing work until another G first makes
  progress (`ns/progress`);
- complete call-and-return time (`ns/op`).

It additionally reports replacement-M creation or reuse. The first value
isolates the transition that the direct path is designed to reduce, the second
checks prompt scheduler handoff, and the third exposes return-side P
reacquisition. It compares identical C bodies through the direct and ordinary
cgo paths rather than attributing the `DirectNoBlock` result to the blocking
path.

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
- unsupported coroutine semantics are rejected rather than miscompiled;
  unsupported direct-cgo declarations retain the ordinary compatibility path
  and direct-path tests assert symbol selection;
- the implementation has no LLVM build or runtime dependency.

## 15. MVP implementation report

The restricted MVP implements M0 through M6 without an LLVM build or runtime
dependency. It is an architecture proof, not a claim that arbitrary Go
programs can use stackless goroutines.

### 15.1 Implemented path

The compiler:

- computes suspend effects and System ABI execution requirements before
  lowering;
- lowers only when both `GOEXPERIMENT=coro` and internal level `-d=coro=4`
  are selected;
- generates typed heap frames and `NOSPLIT` resume functions before escape
  analysis;
- supports closed structured calls, `go` spawn, parameters and results,
  exact expression evaluation, `if`, simple `for`, and nested normal returns;
- splits frontend-generated expression `Init` assignments at an awaited
  multi-result call and resumes the enclosing expression from typed result
  temporaries;
- splits frontend-normalized channel receive assignments at the operation,
  writes their typed value and status temporaries asynchronously, and resumes
  the original projection so blank results and defined bool conversions retain
  Go assignment semantics;
- evaluates a channel send's channel and value in source order and keeps the
  value in factory-owned typed storage until the operation completes;
- lowers receive-channel range to a cyclic receive, status branch, body, and
  pointer-clearing state sequence while evaluating the channel expression
  exactly once;
- lowers channel select to source-ordered operand evaluation, frame-owned
  typed case storage, one runtime operation, and explicit selected-case
  dispatch; this includes empty and default-only selects, nil and duplicate
  channels, closed send and receive, nested case control, and timer channels;
- normalizes single-result awaits in returns, call arguments, and simple
  assignments into pre-escape typed temporary assignments when doing so
  preserves every observable prefix operation; an existing statement `Init`
  prefix remains before the generated await assignments;
- exports a versioned, compiler-private factory capability for the restricted
  cross-package signature subset and reconstructs its deterministic typed
  entry without changing the ordinary Go entry;
- creates all factories in a local recursive call component before lowering
  their bodies, so direct and mutual recursive edges remain stackless;
- exports terminal-control flags alongside factory capabilities and propagates
  them through local and imported structured calls, but not across `go`
  statements;
- exports a separate defer-entry capability, proves it to a fixed point over
  local and imported coroutine dependencies, and distinguishes ordinary
  factory availability from the stricter cleanup-entry contract;
- lowers explicit panic, including panic nested in supported structured
  control flow, to a typed scheduler action, transfers an awaited child's
  panic to its parent, runs each frame's fixed or repeated defers in one
  shared cleanup state, and re-raises the root outcome on an ordinary Go
  stack;
- represents every logical variable captured by a repeated source defer
  literal with a factory-owned typed current-cell slot, allocates local cells
  at declaration execution, snapshots the current pointer at registration,
  and rewrites literal references through that pointer; parameters, named
  results, address identity, post-registration mutation, LIFO sharing, and GC
  scanning therefore retain Go semantics;
- lowers direct recover and replacement panic in a fixed or repeated local
  defer literal through a stable task token, uses the ordinary entry for
  certified local or imported recover-only named targets, and uses a private
  nested factory for certified named targets that may panic, with ordinary
  `panic(nil)` compatibility;
- lowers `runtime.Goexit` from a compiler-owned operation recipe, propagates
  its sticky outcome through structured calls, and isolates it at a detached
  stackless `go` boundary;
- rewrites Goexit in a fixed or repeated local defer literal or direct
  `defer runtime.Goexit()` to an immediate task-owned terminal return, and
  invokes a statically resolved local or imported named target through its
  private factory and a nested run-to-completion scheduler, preserving
  argument evaluation, target-owned defers, recover scope, normal return,
  replacement panic, and sticky Goexit across indirect structured calls;
- gives each state-machine or run-to-completion resume that may execute
  faulting or ordinary Go code, run a defer, or await a child a terminal
  entry, so a native panic recovered by the scheduler enters logical cleanup
  without replaying the faulting state or a direct C call; unknown IR remains
  terminal-aware, while a proven yield-only resume omits the hot-path query;
- stores the terminal kind and Goexit state in existing task-header padding
  and allocates the panic-value map lazily, so normal logical tasks do not
  grow for these capabilities;
- explicitly declares source locals moved into generated factories, ensuring
  that locals captured across a child suspension receive typed heap storage
  even when the source declaration was implicit;
- carries captured-cell result targets through structured awaits and through
  supported ordinary file or network read workers, so asynchronous result
  publication writes the declaration instance selected before suspension;
- rejects unsupported control flow deterministically and removes callers whose
  stackless dependencies cannot be lowered;
- recognizes the narrow `time.Sleep`, `os.File.Read`, and
  `net.(*TCPConn).Read` source paths used by the end-to-end probes.

The runtime:

- uses a push-driven ready queue and generation-checked operation registry;
- tracks one active resume owner per task, defers an early operation completion
  until that owner returns `Wait`, and gives the race detector explicit
  release-merge/acquire edges when a logical task changes executors;
- installs one native-panic recovery boundary per queue-driving episode,
  records the active task's original panic value after its native activation
  unwinds, requeues that task for compiler-generated logical cleanup, and
  applies ordinary replacement semantics when cleanup itself panics;
- re-raises a detached task's unhandled panic only after its logical cleanup,
  outside the resume recovery scope;
- runs a proven non-suspending named defer factory in a nested scheduler and
  merges its normal, panic, or Goexit outcome into the active parent cleanup
  without growing the logical task or the one-pointer `g` extension;
- runs resume episodes on a fixed pool of four executor Gs, each using its
  native OS thread stack, while `m.g0` uses a separate scheduler stack;
- prevents executor stack copy, growth, and shrink, and handles signal stack
  discovery, asynchronous preemption, GC scanning, traceback, and trace events
  on that stack;
- changes `m.g0` identity and resumes the target G in one architecture
  assembly sequence, so GC cannot observe a half-completed switch;
- reloads the scheduler pointer from the heap-owned native context after that
  switch and returns it to the root owner for replacement-executor shutdown;
  the low-level switch helper does not own scheduler-lifecycle policy or reuse
  a caller local saved on the pre-switch stack;
- lets a root borrow the bounded logical-task free list retained by its native
  context, and returns that list before releasing the context only when the
  root never started replacement executors; replacement executors and race
  builds do not move the cache;
- uses common operation ownership for timer, regular-file worker, nonblocking
  socket, and asynchronous C completion;
- registers ordinary channel send and receive directly in the runtime channel
  wait queues, shares FIFO and buffering with parked goroutines, publishes
  completion only after releasing the channel lock, and converts
  send-on-closed panic into a task-owned terminal outcome;
- registers every non-nil case of a logical select in those same queues,
  arbitrates one winner with task-owned atomic state, removes losing waiters
  under the ordinary select lock order, and balances timer-channel blocking
  without allocating a goroutine per case;
- makes timer completion and cancellation race for the same operation record,
  so exactly one path readies the waiter;
- keeps the stackless scheduler runnable while a timer, file worker, or socket
  poll operation is pending; blocking file work releases its P and socket work
  parks through netpoll;
- issues supported scalar C calls through System ABI assembly shims and typed
  visibility bridges without `runtime.cgocall`, `runtime.asmcgocall`, or the
  general cgo argument-frame wrappers;
- reserves the ABI scratch area below SP in arm64 direct trampolines, keeping a
  non-leaf C function from overwriting the Go frame pointer saved at `SP-8`;
- releases scheduler capacity around a blocking C call, allowing another
  stackless logical goroutine to run at `GOMAXPROCS=1`;
- explicitly saves the generated resume frame in its combined blocking entry,
  exits syscall state before that frame unwinds, and reuses both admitted and
  waiting replacement executors across repeated blocking calls.

The transparent cgo frontend:

- emits versioned direct-call metadata and assembly only under the coroutine
  experiment on Darwin/arm64 and Linux/amd64;
- requires the existing `#cgo nocallback` declaration and conservatively
  classifies it as `DirectMayBlock`;
- classifies integer, pointer, `float`, and `double` parameters into the
  target ABI's independent general-purpose and floating-point register
  classes, and supports scalar results;
- passes bounded pointer-free structs through typed bridge pointers, leaving
  target aggregate argument and result classification to the platform C
  compiler;
- supports the cgo two-result errno form by returning raw errno separately and
  materializing nil or `syscall.Errno` only after blocking-call exit;
- preserves ordinary cgo for unsupported declarations and sanitizer builds;
- emits a typed bridge in the preamble translation unit, so `static`
  declarations work without exposing a new user annotation.

Race, memory-sanitizer, and address-sanitizer builds still analyze and export
coroutine summaries, but deliberately skip native lowering and direct cgo
metadata. They retain the managed-stack and general-cgo paths because their
instrumentation hooks do not yet cover arbitrary Go code on the fixed native
executor stack.

### 15.2 Validation

The following gates pass locally on Darwin/arm64 and Linux/amd64:

- compiler analysis, summary, lowering, archive identity, and end-to-end tests;
- all `TestStacklessCoro` runtime tests;
- operation tests under `-race`;
- deterministic early-completion publication and repeated cross-executor
  handoff tests under `-race`;
- GC with typed pointers live across suspension;
- native stack bounds, fixed four-executor pool, `SIGURG` asynchronous
  preemption, traceback, trace output, and deterministic fixed-stack overflow;
- 10,000 native-context reuse cycles concurrent with forced GC, repeated 20
  times on each target;
- 50,000 blocking-boundary root entries concurrent with 100 forced GCs,
  repeated 10 times on Darwin/arm64; this covers stale preemption state when a
  synthetic executor becomes dead and is reused;
- repeated blocking direct C calls beyond the fixed executor-pool size,
  including a framed non-leaf C callee, while another G makes progress at
  `GOMAXPROCS=1`;
- yield, structured child await, 100,000 spawned logical goroutines, timer
  fire/cancel races, regular-file success/error/EOF, socket
  success/EOF/deadline/close, and asynchronous C success/error paths;
- buffered and unbuffered channel send/receive, closed-channel comma-ok,
  blank results, defined bool status, discarded receive, send-on-closed panic,
  direct ordinary/logical waiter interoperability, shared FIFO order, and 64
  simultaneously blocked logical sends without 64 operation goroutines at
  `GOMAXPROCS=1`;
- buffered and unbuffered select send/receive, source-order evaluation,
  default, nil, duplicate, closed, heterogeneous, nested, empty, and timer
  cases; blocked winner publication removes all losing waiters, and stress
  runs exercise both ordinary and race builds;
- a real timer channel repeatedly pairs `Stop` and `Reset` with a directly
  queued logical receive, covering the `blockTimerChan` and
  `unblockTimerChan` lifecycle;
- at `GOMAXPROCS=1`, a sibling stackless task must run before it can cancel a
  pending timer or release the writer that completes a blocked file or socket
  read; a five-second rescue makes a scheduling regression fail instead of
  hanging the runtime test;
- end-to-end programs apply the same progress gate to ordinary `time.Sleep`,
  `os.File.Read`, and `net.(*TCPConn).Read` calls and verify that all three
  waiter functions contain generated coroutine resume symbols;
- direct C symbol inspection proving the absence of the general cgo
  transition symbols in the supported hot path;
- two-result direct C calls with zero and nonzero errno for both scalar and
  void C results, through both run-to-completion and state-machine lowering;
- pointer-free struct arguments and results through both ordinary and
  two-result errno forms; a repeated three-clause loop retains and updates a
  struct result through the direct entry, and disassembly proves it does not
  call the ordinary cgo wrapper;
- a run-to-completion direct C call followed by an implicit nil fault recovers
  without replaying the C call;
- the transparent C fixture evaluates two panic-capable argument helpers in
  source order, lowers their conditional panic without a native `gopanic`
  edge, and retains ordinary cgo with no native lowering under `-race`;
- a real three-package call chain covering package functions, concrete pointer
  and value methods, packed and explicit-slice variadic calls, single and
  multiple results, blank and discarded results, a no-result call, and reuse
  of one imported factory; disassembly verifies private factory calls and the
  absence of ordinary wrappers or `runtime.coroRun` inside the lowered
  resumes;
- direct recursion, mutual recursion, and a concrete recursive method through
  4,096 logical frames; disassembly verifies that every recursive edge calls a
  private factory rather than a public wrapper;
- nested multi-result expressions in a return, call statement, variable
  assignment, and variable-list assignment through two package boundaries;
  disassembly verifies private factories at both edges and no recursive
  `runtime.coroRun`;
- nested single-result expressions in a return, call statement, simple
  assignment, two-await expression, and nested coroutine call chain;
  disassembly verifies that both inner and outer edges use private factories
  and never re-enter a public wrapper;
- explicit string and nil panic values through three lowered frames, with
  fixed defers running in leaf-to-root order before an ordinary outer defer
  recovers the root outcome; object inspection verifies typed panic helpers
  and the absence of native `gopanic` and defer machinery in resume functions;
- implicit nil dereference, integer divide, bounds, and ordinary
  synchronous-callee panics through three lowered frames, with logical defers
  running leaf-to-root before recover; an implicit divide fault in cleanup
  replaces an earlier nil fault; a detached implicit panic runs cleanup and
  then terminates its process as an unhandled goroutine panic;
- repeated defer sites across scheduler yields, normal return, and explicit
  panic; the test verifies exact argument evaluation and LIFO order, scans
  4,096 independently captured heap pointers during 50 concurrent forced GCs,
  and covers the greater-than-128-byte capture path that escape analysis
  retains through a per-registration heap cell;
- repeated source defer literals with parameter and named-result sharing,
  zero-valued and initialized locals, per-iteration declaration identity,
  multiple literals sharing one cell, mutation after registration, address
  equality, single- and multi-result structured awaits, and LIFO cleanup;
  a source-literal pointer-retention probe runs alongside the same 50 forced
  GCs, and an ordinary file-read probe verifies that asynchronous `n` and
  `error` results are written through the selected iteration's cells;
- fixed local defer literals and local named targets that recover an original
  panic, replace it with another panic, recover the replacement in an earlier
  defer, panic during normal-return cleanup, call recover without a panic, and
  exercise both `panic(nil)` modes; named coverage includes direct,
  parameter-wrapper, and repeated registrations, while object inspection
  verifies the coroutine defer helpers, ordinary `gorecover` entries for
  recover-only targets, and a nested factory for a target that both recovers
  and replaces a panic;
- explicit panic propagation through a three-package factory chain, including
  versioned terminal summaries and private factory symbols at both imported
  edges;
- direct and structured Goexit cleanup, a recovered panic raised while Goexit
  is active, detached stackless spawn isolation, and immediate Goexit without
  another suspension; object inspection verifies the typed Goexit helpers and
  absence of `runtime.Goexit` in generated resume functions;
- Goexit propagation through a three-package factory chain, including
  versioned terminal summaries and private factory symbols at both imported
  edges;
- Goexit from a fixed defer literal and from local named targets with no
  arguments, ignored results, parameter wrappers, pointer methods, repeated
  loop registrations, target-owned defers, direct parent recover, a logical
  replacement panic, and an implicit nil fault during Goexit cleanup; the
  normal branch of a conditional Goexit target also returns normally, and
  object inspection verifies `coroDeferGoexit`, `coroDeferRun`, private
  factories, and the absence of `runtime.Goexit` in target resume functions;
- named terminal defers through three packages, including indirect panic and
  Goexit calls, exported recover-only entries, frontend parameter/result
  wrappers, target-owned cleanup, and direct `defer runtime.Goexit()`; symbol
  and object inspection verifies imported private factories and task-owned
  Goexit rewriting;
- deterministic ordinary-path fallback for indirect recover helpers, missing
  or unknown defer capabilities, nested closures that recapture a repeated
  source literal's cell, a stackless `go` edge inside a named defer target,
  and a stackless `go` target that may explicitly panic;
- ordinary callers, missing capabilities, and complex multi-result targets
  retain the public Go entry and execute correctly; an effectful destination
  probe verifies left-before-right evaluation order, and decoder tests verify
  that the legacy summary format carries no factory capability.

The Linux/amd64 local run used an amd64 OrbStack guest translated on Apple
Silicon. Its correctness and allocation results are useful, but its latency
must not be compared directly with a native amd64 host. Native Linux and
Darwin jobs remain CI acceptance gates. On the final topic head, the focused
stackless runtime set passed three times, fixed-defer lowering passed ten
times, transparent C calls passed three times, and the stackless runtime set
passed under the race detector. A full translated-runtime invocation is not a
reliable additional gate: Rosetta can terminate the test process while
reserving its fixed address space. In particular, `TestCheckFDs` reports that
mapping failure, and the deliberately racy `TestPanicRace` is sensitive to the
translated scheduler timing even though disassembly confirms that its
`main.main` and `PanicRace` paths contain no `runtime.coroRun` call. The
workflow therefore runs the unfiltered runtime suite on native Ubuntu and
macOS.

The implicit-panic follow-up's compiler, runtime, race, and transparent-cgo
probes pass on Darwin/arm64 and translated Linux/amd64. Native Linux CI remains
the acceptance gate for that target.

The defer-Goexit topic head passes the unfiltered default and coroutine
compiler/runtime CI package sets on Darwin/arm64, plus the focused stackless
runtime race test and both transparent-cgo probes. The same source passes its
new compiler/runtime cases, the complete coroutine compiler package, the
focused stackless runtime race test, and both transparent-cgo probes on
translated Linux/amd64. The translated full runtime suite retains the
fixed-address reservation limitation described above; native Ubuntu CI is the
final Linux runtime gate.

The focused profiles cover 632 of 697 instrumentable changed production lines,
or 90.7%, across `cmd/cgo`, the compiler, runtime, and `cmd/go` plumbing.
Excluding the small `cmd/go` execution hooks that are exercised in a
subprocess by the end-to-end script, core coverage is 624 of 679 lines, or
91.9%. The new generated-call helpers and benchmark wrappers are 99.5% and
100% covered; run-to-completion lowering is 88.4% covered and its eligibility
predicate is 100% covered. The final native-context return and root-owned
cleanup success paths are covered on Darwin. Remaining lines are defensive
invariant failures or subprocess-only build plumbing.

For the terminal-outcome follow-up specifically, focused statement profiles
cover 135 of 138 changed statements in `cmd/compile/internal/coro` (97.8%) and
52 of 68 in `runtime/coro_stackless.go` (76.5%). Together they cover 187 of
206 changed core statements (90.8%); the uncovered runtime statements are
defensive invalid-state throws.

For the repeated-defer follow-up, the complete coroutine compiler package is
89.7% covered, and the focused profile covers all 62 instrumentable changed
production statements in `lower.go` (100%). The real object, panic, GC, and
large-capture checks run as end-to-end subprocess tests in addition to that
instrumented unit coverage.

For the repeated-source-literal follow-up, the complete coroutine compiler
package is 92.5% covered. The complete profile covers 164 of 167 changed
production statements in `lower.go` (98.2%). The three uncovered statements
reject malformed untyped dynamic-wrapper captures, a non-name fixed-wrapper
capture, and an impossible ordinary-read result expression. End-to-end tests
add real single- and multi-result awaits, file-read worker publication,
terminal cleanup, address identity, and concurrent-GC coverage that is not
credited to the in-process profile.

For the direct-recover follow-up, the complete coroutine compiler package is
90.0% covered. Focused profiles cover 69 of 72 changed compiler statements
(95.8%) and 31 of 38 changed runtime statements (81.6%), or 100 of 110 changed
core statements (90.9%) together. The uncovered runtime statements are
defensive invalid-token and invalid-state failures.

For the Goexit follow-up, focused profiles cover all 45 changed compiler
statements (100%) and 80 of 91 changed runtime statements (87.9%), or 125 of
136 changed core statements (91.9%) together. The uncovered runtime
statements are defensive invalid-context, invalid-state, and invalid-parent
throws.

For the defer-Goexit follow-up, the complete compiler profile covers 82 of 83
changed production statements in `lower.go` (98.8%). The union of the normal
and race runtime profiles covers 71 of 77 changed statements in
`coro_stackless.go` (92.2%). Together they cover 153 of 160 changed core
statements (95.6%). The seven uncovered statements are the compiler's
propagated helper-error return and runtime defensive invalid-token,
invalid-parent, and invalid-terminal throws. End-to-end subprocess tests add
real compilation, execution, implicit-fault, and object-inspection coverage
that is not credited to these in-process statement profiles.

For the implicit-panic follow-up, the complete coroutine compiler package is
89.8% covered. The complete compiler profile covers all 46 changed production
statements in `lower.go`; the focused runtime profile covers 32 of 34 changed
statements in `coro_stackless.go`. Together they cover 78 of 80 changed core
statements (97.5%). The two uncovered statements are the defensive invalid
panic-requeue throw.

For the ordinary-channel follow-up, the complete coroutine compiler package
is 92.3% covered. The complete profile covers 144 of 158 changed production
statements in `lower.go` (91.1%), and the focused normal and race runtime
profiles cover 42 of 46 changed statements in `coro_stackless.go` (91.3%).
Together they cover 186 of 204 changed core statements (91.2%). End-to-end
tests additionally compile and run direction-restricted channels, blank and
defined-bool comma-ok results, pointer values across a forced GC, closed-send
panic recovery, and object inspection; subprocess coverage is not credited to
the in-process profile.

For the direct channel wait-queue follow-up, the union of the feature-off
complete runtime profile, feature-on complete runtime profile, and focused
feature-on race profile covers 191 of 209 changed production statements
(91.39%). The experiment-only `chan_coro.go` implementation is 91 of 96
statements (94.79%). The uncovered core-channel statements are defensive
mixed-owner and invalid-completion throws; test-only compiler subprocesses are
again not credited to these profiles.

For the channel-range follow-up, the complete coroutine compiler package is
92.4% covered. The profile covers 143 of 147 changed production statements in
`lower.go` (97.28%). The four uncovered statements are defensive propagation
and invalid-state paths. End-to-end subprocess tests additionally compile and
run buffered, unbuffered, closed, empty, converted, nested, body-yield, and
per-iteration-address cases, and verify that an iteration-variable closure
falls back without changing Go behavior; subprocess coverage is not credited
to the in-process profile.

For the channel-select follow-up, the complete coroutine compiler package is
92.4% covered. The compiler profile covers 182 of 195 changed production
statements in `lower.go` (93.33%). The union of feature-off and feature-on
complete runtime profiles with the focused feature-on race profile covers 281
of 299 changed runtime statements (93.98%). Together they cover 463 of 494
changed core statements (93.72%). The uncovered runtime statements are
sanitizer-only hooks, synctest and invalid-state failures, one heap-sort branch,
and the non-experiment defensive stub. End-to-end subprocess tests additionally
compile and run source-order, buffered, unbuffered, default, nil, duplicate,
closed, heterogeneous, nested, empty, and timer cases and inspect generated
resume symbols; subprocess coverage is not credited to the in-process
profiles.

### 15.3 Measurements

The runtime benchmarks use a 100 ms sample. Values are representative local
runs, not stable performance promises.

| Operation | Darwin/arm64 | Linux/amd64 translated | Allocation |
| --- | ---: | ---: | ---: |
| yield transition | 15.97 ns | 23.22 ns | 0 B |
| spawn and complete | 50.28 ns | 70.29 ns | 32 B |
| child await | 49.83 ns | 70.49 ns | 32 B |
| zero-duration timer wake | 9.14 us | 27.34 us | 248 B |
| one-byte regular-file read | 10.47 us | 25.28 us | 144 B |
| one-byte socket read | 10.78 us | 25.35 us | 144 B |

The direct channel benchmark performs one unbuffered stackless send/receive
handoff. Ten 1-second samples on Darwin/arm64 Apple M4 Max and translated
Linux/amd64 VirtualApple compare the operation-goroutine implementation at the
preceding channel commit with the shared wait-queue implementation:

| Channel implementation | Darwin median | Linux median | Darwin B/op | Linux B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: | ---: |
| operation goroutine | 10.06 us | 38.186 us | 384 | 385 | 4 |
| direct runtime wait queue | 194.85 ns | 572.9 ns | 464 | 464 | 3 |

The direct path is approximately 51.6 times faster on Darwin and 66.7 times
faster on translated Linux in this microbenchmark, and removes one allocation.
Its dedicated 112-byte logical `sudog` increases bytes per handoff by about 80
relative to the worker baseline; a coroutine-specific waiter pool is a
separate optimization after the ownership model is stable.

A compiler-generated yield loop was also compared with the integration branch
using 20 interleaved 500 ms samples on Darwin/arm64. The baseline and
implicit-panic builds had medians of 18.85 and 18.86 ns per yield,
respectively; their means were 19.72 and 19.57 ns. The generated yield-resume
instruction sequence is identical between the two binaries. Terminal-aware
resumes retain the additional entry query, but a proven yield-only resume has
no measurable regression.

A current Darwin/arm64 burst of 100,000 logical tasks allocates approximately
4.80 MB and 100,012 objects. The task-size regression test fixes the task
header at six pointer widths, or 48 bytes on the two MVP targets. Terminal
payload storage is absent until a panic occurs; representative 100,000-task
runs before and after this slice remained approximately 4.80 MB. The native
executor count remains four.

A repeated-defer microbenchmark runs one scheduler yield and then returns from
a function with 0, 1, 16, or 256 registered defer calls. The same no-inline
source was measured five times for 300 ms on the Darwin/arm64 Apple M4 Max.
The ordinary build uses the standard Go defer path; the coroutine build lowers
the complete benchmark call chain.

| Registered defers | Ordinary Go | Stackless coroutine |
| ---: | ---: | ---: |
| 0 | 85.16 ns, 0 B, 0 allocs | 94.37 ns, 140 B, 5 allocs |
| 1 | 108.8 ns, 16 B, 1 alloc | 112.4 ns, 164 B, 7 allocs |
| 16 | 425.8 ns, 256 B, 16 allocs | 311.6 ns, 644 B, 26 allocs |
| 256 | 5.465 us, 4,104 B, 256 allocs | 3.037 us, 8,708 B, 270 allocs |

At 256 registrations, subtracting the zero-defer fixed cost gives
approximately 21.0 ns and 16.0 bytes per ordinary defer, versus 11.5 ns and
33.5 bytes per stackless defer. The frame-owned function-value stack reduces
the incremental time in this fixture but uses more memory. This is not yet a
memory optimization; fixed non-repeated defer sites retain their existing
armed-bit path and do not pay this cost.

A direct-recover microbenchmark calls a no-inline function that yields once
and then runs one fixed defer. It compares a defer without recover, recover
without a panic, and recover of an explicit panic. Each number is the median
of five 300 ms samples on the same Darwin/arm64 Apple M4 Max and includes one
ordinary public root entry.

| Cleanup | Ordinary Go | Stackless coroutine |
| --- | ---: | ---: |
| fixed defer | 76.20 ns, 0 B, 0 allocs | 1.032 us, 496 B, 7 allocs |
| recover, no panic | 79.33 ns, 0 B, 0 allocs | 1.052 us, 512 B, 8 allocs |
| recover explicit panic | 278.4 ns, 0 B, 0 allocs | 1.152 us, 768 B, 10 allocs |

Within the stackless path, adding a direct recover to the fixed-defer fixture
costs about 20 ns, 16 bytes, and one allocation. Recording and recovering the
explicit panic adds about 100 ns, 256 bytes, and two allocations. The much
larger common cost is the current per-call root scheduler entry, not the
recover helper itself; nested factory calls avoid that entry.

The direct-call microbenchmark performs the same non-inlined scalar addition
through the stackless System ABI path, ordinary cgo, and pure Go. The compiler
reported two lowered functions and no skipped functions.

| Call path | Darwin/arm64 | Linux/amd64 translated | Allocation |
| --- | ---: | ---: | ---: |
| direct System ABI | 3.84-3.96 ns | 8.49-8.89 ns | 0 B/op |
| ordinary cgo | 16.53-16.62 ns | 26.81-26.97 ns | 0 B/op |
| pure Go reference | 0.767-0.772 ns | 1.04-1.06 ns | 0 B/op |

The transparent-cgo follow-up adds a separate benchmark in
`internal/runtime/cgobench`. The Darwin/arm64 sample ran on an Apple M4 Max.
The Linux/amd64 sample ran in the translated VirtualApple guest described
above. Both used five 300 ms runs with allocation reporting:

| Shape | Path | Darwin/arm64 median | Linux/amd64 translated median | Allocation |
| --- | --- | ---: | ---: | ---: |
| steady | ordinary cgo | 13.27 ns | 22.53 ns | 0 B/op, 0 allocs/op |
| steady | transparent `DirectMayBlock` | 14.26 ns | 20.98 ns | 0 B/op, 0 allocs/op |
| steady | compiler-owned `DirectNoBlock` | 4.331 ns | 5.176 ns | 0 B/op, 0 allocs/op |
| entry | ordinary cgo | 13.78 ns | 23.01 ns | 0 B/op, 0 allocs/op |
| entry | transparent `DirectMayBlock` | 61.70 ns | 98.02 ns | 48 B/op, 1 alloc/op |
| entry | compiler-owned `DirectNoBlock` | 1.042 us | 956.3 ns | 520 B/op, 8 allocs/op |

The conservative transparent path is therefore near ordinary cgo in steady
state on both runs, not a demonstrated speedup. `DirectNoBlock` shows that
avoiding syscall scheduler accounting can materially reduce the boundary
cost, but a public bounded-call contract has not been selected. Current
run-to-completion lowering makes the no-result `DirectMayBlock` entry much
smaller on both runs, while the result-returning `DirectNoBlock` entry still
exposes the larger root-creation cost. The entry measurements are evidence for
the separate cross-package entry design question in section 10.9, not
justification for adding source annotations.

A follow-up benchmark uses the same non-leaf C body for ordinary cgo and
transparent `DirectMayBlock`. The body writes one byte to a pipe and waits on
an atomic gate set by a Go worker. It reports both the interval from the C
write until the worker releases the gate and the complete call-and-return
time. Each value below is the median of ten 500 ms samples:

| Platform | Path | Time to Go progress | Complete round trip |
| --- | --- | ---: | ---: |
| Darwin/arm64 | ordinary cgo | 1.575 us | 1.765 us |
| Darwin/arm64 | transparent `DirectMayBlock` | 1.581 us | 1.798 us |
| Linux/amd64 translated | ordinary cgo | 2.951 ms | 2.967 ms |
| Linux/amd64 translated | transparent `DirectMayBlock` | 2.915 ms | 2.950 ms |

The Darwin samples ran on an Apple M4 Max. The translated Linux samples ran on
the QEMU TCG guest and show substantially more scheduler noise. Both paths
reported zero allocations per call; the translated direct batch amortized its
one root setup to 2 B per call.

The platform directions differ and the Darwin distributions overlap, so this
does not yet demonstrate a lower blocking handoff cost. It does show prompt
forward progress and establishes separate regression metrics for the handoff
and return sides. In the same Darwin run, the steady nonblocking direct
boundary was 4.011 ns versus 13.35 ns for ordinary cgo. That remains the
measured evidence that avoiding the general cgo transition can reduce cost.
A coroutine-specific blocking handoff must demonstrate the same advantage in
`ns/progress` without moving a large penalty into P reacquisition on return.

Combining blocking preparation, caller-frame capture, syscall entry, and
foreign bookkeeping into one runtime entry reduces the conservative direct
boundary itself. Ten later 500 ms Darwin/arm64 samples measured a median of
12.61 ns for transparent `DirectMayBlock` versus 13.28 ns for ordinary cgo.
The same run measured 1.614 us versus 1.610 us until Go progress and 1.837 us
versus 1.804 us for the complete round trip. The boundary is approximately 5%
smaller, while the thread handoff and return remain effectively scheduler
costs.

For comparison, a direct-resume experiment with `entersyscallblock` measured
approximately 6.5 us until Go progress and 11.2 us for the round trip. It was
correct only when emitted in the resume frame and was slower on both sides, so
the implementation does not use it.

An atomic-only follow-up isolates the case where the blocked call must let an
already-runnable sibling progress. Each iteration creates one sibling before C
publishes an epoch and spins on its atomic release. The ordinary path remains
a plain function and creates an ordinary Go goroutine. The direct parent and
its no-argument worker both lower, so the direct path enqueues a logical task
in the same stackless scheduler.

An earlier fixture used `go worker(arguments)` and yielded before entering C.
That source shape creates a generated wrapper that is not currently a
top-level lowering target. Final analysis therefore skipped both parent
functions, and the resulting 24--40 ms measurements did not exercise the
intended stackless path. The corrected fixture verifies final lowering and
uses a one-shot worker so it cannot retain the P after releasing C.

At `GOMAXPROCS=1`, ten 500 ms Darwin/arm64 samples produced these medians:

| Path | Time from C publication to sibling | Benchmark round trip | Allocation |
| --- | ---: | ---: | ---: |
| ordinary cgo | 66.347 us | 67.438 us | 0 B/op, 0 allocs/op |
| transparent `DirectMayBlock` | 53.739 us | 70.331 us | 88 B/op, 3 allocs/op |

The direct C-to-sibling interval is about 19% smaller, but logical-task
creation leaves the complete benchmark round trip about 4.3% larger. The
round-trip column includes creation of one sibling per iteration; the
`ns/progress` metric starts at C publication. A coroutine-aware blocking entry
must reduce the forward handoff without regressing the no-sibling boundary,
and any return optimization must not move the cost into locked-M
reacquisition. Logical-task allocation remains a separate follow-up.

A conditional blocking entry now takes the immediate handoff only when the
scheduler has both replacement capacity and queued logical work. The C call
remains on its original M, as it does for an ordinary blocking cgo call. The
runtime releases that M's P with the standard `entersyscallblock` transition,
and a replacement M runs the queued continuation. The coroutine-specific
advantage is therefore a smaller decision and wakeup path, not removal of the
M/P handoff required by a blocking C call.

The scheduler counts runnable tasks while holding its queue lock. Blocking
entry snapshots that count before notifying replacement executors, avoiding a
race in which an executor could dequeue the task before entry selected the
handoff. With no queued task, entry retains the existing `entersyscall` path.

Ten 500 ms Darwin/arm64 samples after this change produced these medians:

| Path | Time from C publication to sibling | Benchmark round trip | Allocation |
| --- | ---: | ---: | ---: |
| ordinary cgo | 65.821 us | 66.605 us | 0 B/op, 0 allocs/op |
| conditional direct handoff | 6.928 us | 23.556 us | 88 B/op, 3 allocs/op |

The direct interval is about 89.5% smaller than ordinary cgo and the complete
round trip is about 64.6% smaller. In separate paired 15-sample runs without a
runnable sibling, the direct steady boundary changed from 12.63 ns to
12.38 ns and root entry from 66.02 ns to 65.46 ns. Those differences are
within run-to-run noise and show no no-sibling regression.

The scalar ABI follow-up adds `float` and `double` classification. An
end-to-end mixed signature uses floating-point and integer argument registers
in alternating source order and returns a `double`. The same test calls
`sin` from libm, checks the result, and verifies that final lowering selects
the direct symbols. Race builds continue to select the ordinary wrappers.

The corresponding steady benchmark calls a non-inlined C function with the
signature `(double, double *) -> void`; the C body accumulates an actual libm
`sin` result. The void result keeps the repeated call in the currently
supported loop lowering, while the separate end-to-end test covers
floating-point return registers. Disassembly verifies that the direct
benchmark calls its `_Cdirect_` entry and contains neither `runtime.cgocall`
nor `runtime.asmcgocall`. Each value below is the median of ten 500 ms runs:

| Shape | Path | Darwin/arm64 | Linux/amd64 translated | Allocation |
| --- | --- | ---: | ---: | ---: |
| empty C leaf | ordinary cgo | 13.14 ns | 21.82 ns | 0 B/op, 0 allocs/op |
| empty C leaf | transparent `DirectMayBlock` | 12.77 ns | 21.63 ns | 0 B/op, 0 allocs/op |
| libm `sin` accumulation | ordinary cgo | 17.02 ns | 32.24 ns | 0 B/op, 0 allocs/op |
| libm `sin` accumulation | transparent `DirectMayBlock` | 17.94 ns | 30.47 ns | 0 B/op, 0 allocs/op |

The direct libm shape is about 5.4% slower on Darwin and 5.5% faster in the
translated Linux run. The empty boundary is slightly smaller on both. These
mixed platform results do not establish a universal short-call speedup for
the conservative `DirectMayBlock` class. They do establish real library ABI
compatibility and retain a fixed benchmark for future transition and
`DirectNoBlock` policy work.

The two-result errno follow-up adds another fixed steady benchmark. Both paths
call a non-inlined C leaf that clears errno and returns one scalar. The direct
bridge also writes that scalar through its hidden result pointer and returns
raw errno in the integer result register. Disassembly verifies
`_Cdirect2_coro_direct_errno` and the absence of `runtime.cgocall` and
`runtime.asmcgocall`. Each value is the median of ten 500 ms runs:

| Path | Darwin/arm64 | Linux/amd64 translated | Allocation |
| --- | ---: | ---: | ---: |
| ordinary cgo errno | 18.30 ns | 26.59 ns | 0 B/op, 0 allocs/op |
| direct cgo errno | 16.94 ns | 23.95 ns | 0 B/op, 0 allocs/op |

The direct boundary is about 7.4% smaller on Darwin and 9.9% smaller in the
translated Linux run. These short calls do not require a replacement M to run,
so this benchmark isolates the ABI and enter/exit transition rather than the
blocking handoff itself. A genuinely blocking call still leaves the C frame on
its original M and hands the P to a replacement M when runnable work exists.
The coroutine advantage is the lower transition and conditional handoff cost,
not removal of that scheduler obligation.

The bounded-aggregate follow-up fixes a pointer-free
`struct { uint64_t; double; }` benchmark. Both paths repeatedly call the same
non-inlined C function with two by-value struct arguments and one by-value
struct result. The direct path passes stable Go frame-slot addresses to its
typed bridge; the platform C compiler performs the actual target ABI
classification. End-to-end disassembly checks the direct symbol and excludes
`runtime.cgocall`. Each value is the median of ten paired 500 ms runs:

| Path | Darwin/arm64 | Linux/amd64 translated | Allocation |
| --- | ---: | ---: | ---: |
| ordinary cgo aggregate | 22.245 ns | 24.945 ns | 0 B/op, 0 allocs/op |
| direct cgo aggregate | 21.58 ns | 20.89 ns | 0 B/op, 0 allocs/op |

The direct boundary is about 3.0% smaller on Darwin and 16.3% smaller in the
translated Linux run. The Darwin distributions overlap and the platforms
differ materially, so this is evidence that the typed bridge preserves a
competitive boundary, not a universal aggregate-call speedup.

For a `println` program whose `work` function calls `runtime.Gosched`, the
Darwin executable is 1,833,458 bytes with the coroutine experiment and
1,729,378 bytes without it, a 104,080-byte (6.0%) increase. Stripped
executables are 1,195,746 and 1,128,066 bytes, a 67,680-byte (6.0%) increase.
Summed text symbols are 474,448 and 444,176 bytes, a 30,272-byte (6.8%)
increase. The compiler reported two lowered functions and no skips. Most of
this fixed delta is the experimental runtime; per-logical-task memory is
reported separately above.

### 15.4 Remaining restrictions

The MVP does not yet support general non-channel range loops, labeled control
flow, dynamic calls, interfaces, unlabeled branch control, closure capture of
a per-iteration channel-range variable, general escaping closures, nested
closures that recapture a repeated-literal cell, indirect recover-capable
helpers, stackless `go` edges in terminal named defer targets, explicit panic
from a non-structured spawned task, labeled select or complex select receive
destinations, multiple channel operations in one expression, mutex parking,
reflection, callbacks, variadic C calls, or general C ABI type classification.
Ordinary channel send, discarded or single-value receive, comma-ok receive,
receive-channel range, and select with simple receive targets are supported
through the runtime's shared channel wait queues.
Explicit and implicit panic and Goexit
through structured calls, implicit unhandled panic and isolated Goexit from a
spawned logical goroutine, fixed or repeated-simple-loop defer cleanup,
terminal control and typed capture ownership in repeated source literals,
direct deferred Goexit, and statically resolved local or imported named
terminal targets are supported. The terminal target may reach panic or Goexit
through certified indirect structured calls. Direct foreign declarations now
have a restricted transparent cgo path, including bounded pointer-free
structs through a typed indirect bridge. Pointer-bearing, union, class,
oversized, over-aligned, variadic, callback-capable, and non-target ABIs retain
ordinary cgo.
Cross-package factory ABI 1 excludes interface and method-value calls,
closures, and generic shapes. Frontend-normalized nested multi-result calls and
conservatively normalized single-result calls are supported. Short-circuit
right operands, loop condition and post expressions, effectful expression
prefixes, and complex result targets remain on the ordinary entry until
general expression, control-flow, and assignment lowering exists. Logical
traceback, debugger, profiler, race instrumentation on native executor stacks,
dynamic executor sizing, cancellation of in-flight file work, and broad
standard-library compatibility remain future work.

### 15.5 Explicit frame migration

The architecture in sections 1 and 7 specifies a typed frame pointer plus a
static resume symbol. The initial implementation instead lets ordinary Go
closure lowering provide a provisional typed frame: the generated factory
returns a closure, and variables assigned by the resume function occupy
separate captured cells. This is GC-correct and kept the first lowering small,
but it does not yet realize the intended physical handle or allocation cost.

After the root-entry reductions, a public `yieldEntry` uses 360 B and five
allocations. Its runtime scheduler and wake channel account for 304 B and two
allocations. Disassembly attributes the remaining compiler-side 56 B and
three allocations to an 8-byte result cell in the wrapper, a 16-byte
pointer-free state cell in the factory, and a 32-byte scanned closure. A
depth-4096 recursive yield uses 475,520 B and 16,390 allocations, so reducing
per-frame objects has substantially more leverage than another scheduler
micro-optimization.

Revision `b996f63c22` adds the runtime foundation without changing the active
factory ABI. The task-owned resume packet remains two pointer widths: its
former task backlink becomes an explicit frame pointer, the scheduler pointer
continues to identify the active owner, and the task is recovered from the
packet's embedded address. This keeps the logical-task header at 48 bytes and
does not allocate a packet for each transition. Frame-aware root, await,
spawn, and deferred-run entries coexist with the closure entries, which pass
a nil frame until compiler lowering migrates.

The compiler migration should remain incremental:

1. generate one typed struct containing the state and values live across
   suspension for the already-supported closed-function subset;
2. generate a static resume function that loads the typed pointer from the
   first resume-packet word, instead of capturing mutable factory locals;
3. add a versioned factory ABI that returns the opaque frame pointer and
   static resume symbol, while retaining factory ABI 1 as a fallback for
   unsupported closures and generic shapes;
4. route root, structured await, spawn, and eligible defer sites through the
   matching frame-aware runtime entries;
5. remove the closure fallback only after cross-package summaries, terminal
   cleanup, race behavior, and every existing lowering test agree.

Factory ABI 2 now implements the first three steps and the root, await, and
spawn portion of the fourth for a deliberately closed subset. Its factory
allocates one anonymous, GC-typed frame and returns that frame as an opaque
pointer together with a zero-capture resume function. The resume function
loads the typed frame from the first resume-packet word. Root,
structured-await, and spawn call sites select the frame-aware runtime entry
from the callee's exported factory summary, including across package
boundaries.

The initial ABI 2 subset contained only yield and structured-await state
machines. The first measured extension also admits channel transition sites
when the function has no range-variable capture. Run-to-completion functions,
source closures, channel ranges, defer or terminal behavior, and timer, file,
poll, spawn, or foreign transition sites retain factory ABI 1. An ABI 1 caller
can still await or spawn an ABI 2 child. These restrictions keep closure and
cleanup ownership unchanged while the typed-frame layout is validated; they
are fallback rules, not source annotations.

Ten alternating-order 200 ms samples on Darwin/arm64 measured the channel
extension against the initial subset. Parking and waking 100 channel-blocked
tasks fell from 337 to 237 allocations (-29.7%) and from 20,225 to 18,625
bytes (-7.9%). Time changed from 100.19 us to 93.18 us, which was not
statistically significant (`p=0.579`). Channel round-trip and ready-select
allocation counts remained two and three respectively, with no significant
time change. Disassembly attributes the parked-task delta to replacing a
separate state object and scanned closure with one small typed frame. Spawn
and timer eligibility produced no stable object reduction in their dedicated
probes, so they remain ABI 1 rather than broadening the new ABI without a
measured benefit.

The same alternating measurement on Linux/amd64 reproduced the exact object
counts and byte reduction: the 100 parked tasks fell from 337 to 237
allocations and from 19.75 KiB to 18.19 KiB. Channel round-trip and
ready-select again remained at two and three allocations. None of the three
timing changes was statistically significant. Linux timing was collected in
an amd64 virtual machine on an arm64 host and is therefore directional, while
the allocation result is architecture independent and agrees with the
Darwin disassembly.

The first Darwin/arm64 performance gate used `GOMAXPROCS=1`, disabled
inlining, enabled real lowering with `-d=coro=4`, and reports the median of
five 300 ms samples:

| Probe | Closure frame | Explicit frame | Change |
| --- | ---: | ---: | ---: |
| public yield entry | 2,011 ns, 360 B, 5 allocs | 1,768 ns, 336 B, 4 allocs | -12.1%, -6.7%, -20.0% |
| recursive yield, depth 64 | 15,375 ns, 7,808 B, 262 allocs | 11,871 ns, 6,504 B, 132 allocs | -22.8%, -16.7%, -49.6% |
| recursive yield, depth 4,096 | 858,059 ns, 475,520 B, 16,390 allocs | 667,799 ns, 393,576 B, 8,196 allocs | -22.2%, -17.2%, -50.0% |

The same five-sample gate on Linux/amd64 under OrbStack translation confirmed
the direction and exact allocation counts. Absolute translated timings are
not compared with native Darwin:

| Probe | Closure frame | Explicit frame | Change |
| --- | ---: | ---: | ---: |
| public yield entry | 1,123 ns, 360 B, 5 allocs | 933 ns, 336 B, 4 allocs | -16.9%, -6.7%, -20.0% |
| recursive yield, depth 64 | 16,107 ns, 7,808 B, 262 allocs | 13,425 ns, 6,504 B, 132 allocs | -16.7%, -16.7%, -49.6% |
| recursive yield, depth 4,096 | 959,615 ns, 475,520 B, 16,390 allocs | 794,517 ns, 393,576 B, 8,196 allocs | -17.2%, -17.2%, -50.0% |

The exact upstream merge-base, revision `5d29d80b6c`, still defines a large
lower bound. With the same sources and flags except coroutine lowering, the
ordinary compiler allocated no heap objects in these probes:

| Probe | Upstream Darwin | Explicit frame Darwin | Upstream Linux | Explicit frame Linux |
| --- | ---: | ---: | ---: | ---: |
| public yield entry | 51.46 ns | 1,768 ns (34x) | 90.64 ns | 933 ns (10x) |
| recursive yield, depth 64 | 410.6 ns | 11,871 ns (29x) | 338.2 ns | 13,425 ns (40x) |
| recursive yield, depth 4,096 | 19,048 ns | 667,799 ns (35x) | 18,259 ns | 794,517 ns (44x) |

The explicit frame removes one object from a public entry and almost half the
objects in recursive lowering, but it does not eliminate the remaining frame
allocation or scheduler transition cost. Those are separate optimization
targets; the translated Linux ratios show direction, not native timing.

A native-root follow-up retains the scheduler's bounded logical-task free list
in its pooled native context between single-executor roots. Each cache remains
limited to 256 six-word task headers, or 12 KiB on the MVP targets. Only the
four-entry warm pool retains caches, bounding long-lived task retention at
48 KiB; contexts returned to the overflow list discard their cache. A root
that starts replacement executors leaves its cache with the root scheduler,
preserving the existing native-context release and locked-M handoff order.
Operation records are not retained across roots: their existing per-root cache
already covers the steady timer, file, and network loops, and the cross-root
retention cost had no measured benefit.

Twenty alternating one-second Darwin/arm64 samples isolated depth-64
recursion. Reusing the logical tasks changed the median from 7.660 us to
6.799 us (-11.24%), from 6,504 to 3,432 bytes (-47.23%), and from 132 to 68
allocations (-48.48%). A broader twelve-sample 500 ms gate reproduced the
allocation counts but had noisier timing: public entry, task sequence,
100-task burst, and 100 parked tasks had no significant time or allocation
change. Depth-4,096 recursion fell from 8,196 to 7,940 allocations (-3.12%);
the 256-entry bound deliberately leaves the remaining simultaneously live
tasks uncached. A separate timer, file, TCP, blocking-I/O, and blocking-C gate
reported identical bytes and allocations and no significant time changes.

The same ten-sample gate against exact upstream revision `5d29d80b6c` leaves
a material lower bound after task reuse:

| Probe | Upstream Darwin | Native task cache | Ratio |
| --- | ---: | ---: | ---: |
| public yield entry | 49.24 ns, 0 allocs | 1,106.5 ns, 4 allocs | 22.5x |
| recursive yield, depth 64 | 400.0 ns, 0 allocs | 6,740 ns, 68 allocs | 16.8x |
| recursive yield, depth 4,096 | 20.11 us, 0 allocs | 402.3 us, 7,940 allocs | 20.0x |

The remaining recursive objects are typed frames and tasks beyond the bounded
cache. Public entry also retains its result cell, typed frame, root scheduler,
and wake channel. Frame/result fusion and root-entry cost therefore remain
separate optimization targets.

Object inspection shows one typed heap allocation in each eligible factory
and a direct frame load at resume entry. The task header remains 48 bytes. The
GC, checkptr, race, cross-package, and recursive probes continue to use the
same source programs as factory ABI 1.

No source annotation or per-function policy switch is introduced. The first
performance gate is a simple public entry plus depth-64 and depth-4096
recursion. It must reduce compiler-owned objects, preserve the 48-byte task
header and live-stack advantage, and leave timer, file, network, channel,
defer, panic, Goexit, and direct-C probes green on both MVP platforms.

Factory ABI 3 implements bounded reuse for the typed frames introduced by
ABI 2. It prepends the current resume context to the compiler-private factory
entry. A generated factory asks the owning scheduler for a frame with the
same static resume identity and declared size, and reserves the corresponding
logical task until the immediately following frame-aware await or spawn
consumes it. The ordinary exported wrapper passes a nil context and keeps its
existing root allocation. ABI 1 and ABI 2 remain valid imported capabilities.

Each active scheduler owns at most 256 cached-frame tasks and 32 KiB of
declared frame payload. The task limit is shared with the existing bounded
free-task list; the task header remains 48 bytes. Single-executor roots move
the cache through the four-entry warm native-context pool. Thus the new
long-lived payload bound is 128 KiB across warm contexts, in addition to the
existing 48 KiB task-header bound and allocator size-class overhead. Overflow
contexts and roots that started replacement executors do not retain a cache.
Race builds do not reuse task or frame identities.

Normal completion clears every pointer-containing field of a cache-owned
typed frame after copying results to their destinations. Panic, Goexit, and a
panic between factory reservation and scheduling discard the frame instead.
This keeps a reused opaque frame from retaining completed arguments or result
pointers. A real-lowering finalizer probe covers both first use and reuse; the
runtime tests also cover resume identity, non-head reservations, cancellation,
cache bounds, deep unwinding, race behavior, and reuse across public roots.

Ten alternating 300 ms Darwin/arm64 samples used `GOMAXPROCS=1`, disabled
inlining in the target package, and enabled real lowering with `-d=coro=4`:

| Probe | Native task cache | Typed-frame cache | Change |
| --- | ---: | ---: | ---: |
| public yield entry | 1.045 us, 336 B, 4 allocs | 1.055 us, 336 B, 4 allocs | +0.96% time; objects unchanged |
| recursive yield, depth 64 | 6.376 us, 3,432 B, 68 allocs | 5.420 us, 360 B, 4 allocs | -14.99% time, -89.51% bytes, -94.12% allocs |
| recursive yield, depth 4,096 | 390.7 us, 381,288 B, 7,940 allocs | 445.5 us, 369,000 B, 7,684 allocs | +14.03% time, -3.22% bytes and allocs |

All three time changes were statistically significant. Their geomean changed
by -0.72%; the byte and allocation geomeans changed by -53.35% and -61.53%.
The allocation deltas match the measured 48-byte recursive frame size
exactly: depth 64 removes 64 frames and 3,072 bytes, while depth 4,096 removes
the bounded 256 frames and 12,288 bytes. The remaining deep count is
`4 + 2 * (4096 - 256) = 7684`: four public-root objects followed by one frame
and one task for every depth beyond the cache. The unchanged public 336 B
also confirms that the packed cache counters did not grow the root scheduler
allocation class.

Three 200 ms Linux/amd64 samples under QEMU reproduced the exact allocation
results: public entry used 336 B and four allocations, depth 64 used 360 B
and four allocations, and depth 4,096 used 369,000 B and 7,684 allocations.
The translated timings are not compared with native Darwin.

The exact upstream merge-base at revision `5d29d80b6c` still allocates no
objects in the same probes:

| Probe | Upstream Darwin | Typed-frame cache | Ratio |
| --- | ---: | ---: | ---: |
| public yield entry | 46.34 ns, 0 allocs | 1.055 us, 4 allocs | 22.8x |
| recursive yield, depth 64 | 391.9 ns, 0 allocs | 5.420 us, 4 allocs | 13.8x |
| recursive yield, depth 4,096 | 18.95 us, 0 allocs | 445.5 us, 7,684 allocs | 23.5x |

The shallow-recursion result validates frame reuse, but the saturated
depth-4,096 path exposes the next runtime target. A factory currently enters
the scheduler to attempt or reserve a frame, then the generated caller enters
again to schedule it. Calls beyond the cache bound retain this extra
transition without receiving a cached frame. A follow-up should bypass or
fuse the saturated reservation path without adding task-header or scheduler
size and without weakening concurrent spawn correctness. Public-root frame,
scheduler, and wake allocations and the remaining transition overhead stay
separate targets after that fast path.

## 16. Work after the MVP

The likely order is:

1. add mutexes, semaphores, and runtime notes through the same park/wake
   boundary;
2. broaden aggregate System ABI support beyond the bounded typed bridge and
   add platform-error conventions beyond POSIX errno;
3. extend expression, assignment, and branch normalization to short-circuit
   control, loop conditions, frontend loop-variable rewrites, and effectful
   destinations, then extend the compiler-private factory ABI only with
   matching closure and generic lowering; do not add source annotations or
   per-call policy metadata;
4. propagate repeated-literal cell pointers through nested closure
   environments as part of general closure lowering;
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

## 17. Confirmed MVP decisions

The implemented MVP choices are:

1. Use a dedicated executor G on the OS thread stack, give `g0` a separate
   runtime stack, and do not run arbitrary Go code under the `g0` identity.
2. Keep the existing Go scheduler as the physical M/P/GC scheduler during the
   MVP and add a bounded stackless scheduler above it.
3. Use versioned compiler-private typed foreign metadata; do not port the
   distributed LLGo annotation vocabulary or freeze a public directive.
4. Support only scalar and foreign-pointer C signatures without callbacks.
5. Classify supported unknown-duration C calls as `DirectMayBlock`; retain
   ordinary cgo for unsupported signatures and verify direct-path selection in
   tests that depend on it.
6. Permit a compiler-generated leaf ABI shim and typed C visibility bridge in
   the first direct-call slice, while excluding the general cgo argument-frame
   wrappers and keeping direct call-site System ABI lowering as the long-term
   target.
7. Require timer, regular-file, and TCP paths before calling the work an MVP.
8. Use one versioned, compiler-private factory capability for lowered
   cross-package calls; derive its symbol and type from the Go function and
   fail closed to the ordinary entry for unsupported signatures.

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
