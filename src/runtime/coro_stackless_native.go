// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/sys"
	"unsafe"
)

const (
	stacklessCoroSchedulerSize = 64 << 10

	// coroNativeStart runs in a frame entered by mcall on the original g0
	// stack. Leave that frame and its caller above the executor's synthetic
	// stack root.
	stacklessCoroBootstrapReserve = 4 << 10
)

type stacklessCoroNativeContext struct {
	gState     stacklessCoroGState
	scheduler  *stacklessCoroScheduler
	schedulerG *g
	executor   *g
	caller     *g
	nativeG0   *g
	lockedG    guintptr
	lockedInt  uint32
	g0Accurate bool
	sigmask    sigset
	poolNext   *stacklessCoroNativeContext
}

var stacklessCoroNativePool struct {
	lock  mutex
	count int
	// available preserves the common lock-free reuse path. Contexts beyond
	// the warm capacity remain reusable through overflow.
	available chan *stacklessCoroNativeContext
	overflow  *stacklessCoroNativeContext
}

// coroNativeGogo installs newG0 and resumes buf in one assembly sequence. No
// Go safe point may observe the old current G with the new m.g0.
func coroNativeGogo(buf *gobuf, newG0 *g)

func init() {
	lockInit(&stacklessCoroNativePool.lock, lockRankLeafRank)
	stacklessCoroNativePool.available = make(chan *stacklessCoroNativeContext, stacklessCoroWarmExecutorCount)
}

// coroRunOnNativeStack runs s on a fixed portion of the current operating
// system thread stack. It returns s reloaded from the heap context after a
// successful stack switch; callers must not reuse their pre-switch local.
// The race runtime has its own stack and goroutine bookkeeping, so race builds
// retain the managed-stack driver for now.
func coroRunOnNativeStack(s *stacklessCoroScheduler) *stacklessCoroScheduler {
	if raceenabled {
		return nil
	}

	ctx := acquireStacklessCoroNativeContext()
	gp := getg()
	if gp != gp.m.curg || gp == gp.m.g0 || gp == gp.m.gsignal {
		releaseStacklessCoroNativeContext(ctx)
		return nil
	}
	if gp.param != nil {
		releaseStacklessCoroNativeContext(ctx)
		throw("runtime: stackless coroutine caller has pending parameter")
	}

	ctx.scheduler = s
	ctx.caller = gp
	gp.param = unsafe.Pointer(ctx)
	mcall(coroNativeStart)
	msigrestore(ctx.sigmask)

	if gp.param != unsafe.Pointer(ctx) {
		throw("runtime: lost stackless coroutine native context")
	}
	gp.param = nil
	scheduler := ctx.scheduler
	ctx.scheduler = nil
	ctx.caller = nil
	releaseStacklessCoroNativeContext(ctx)
	return scheduler
}

func acquireStacklessCoroNativeContext() *stacklessCoroNativeContext {
	select {
	case ctx := <-stacklessCoroNativePool.available:
		return ctx
	default:
	}
	lock(&stacklessCoroNativePool.lock)
	if ctx := stacklessCoroNativePool.overflow; ctx != nil {
		stacklessCoroNativePool.overflow = ctx.poolNext
		ctx.poolNext = nil
		unlock(&stacklessCoroNativePool.lock)
		return ctx
	}
	stacklessCoroNativePool.count++
	unlock(&stacklessCoroNativePool.lock)
	return newStacklessCoroNativeContext()
}

func releaseStacklessCoroNativeContext(ctx *stacklessCoroNativeContext) {
	select {
	case stacklessCoroNativePool.available <- ctx:
		return
	default:
	}
	lock(&stacklessCoroNativePool.lock)
	ctx.poolNext = stacklessCoroNativePool.overflow
	stacklessCoroNativePool.overflow = ctx
	unlock(&stacklessCoroNativePool.lock)
}

func newStacklessCoroNativeContext() *stacklessCoroNativeContext {
	ctx := new(stacklessCoroNativeContext)
	ctx.gState.native = unsafe.Pointer(ctx)

	schedulerG := malg(stacklessCoroSchedulerSize)
	schedulerG.stackguard1 = schedulerG.stackguard0
	schedulerG.sched.sp = schedulerG.stack.hi - 4*goarch.PtrSize
	schedulerG.sched.g = guintptr(unsafe.Pointer(schedulerG))
	ctx.schedulerG = schedulerG

	executor := malg(-1)
	casgstatus(executor, _Gidle, _Gdead)
	executor.startpc = abi.FuncPCABIInternal(coroNativeMain)
	executor.goid = sched.goidgen.Add(1)
	executor.sched.g = guintptr(unsafe.Pointer(executor))
	executor.trace.reset()
	allgadd(executor)
	sched.ngsys.Add(1)
	ctx.executor = executor
	return ctx
}

// coroNativeStart is the mcall continuation that replaces the native g0
// identity with an executor G. It never returns.
func coroNativeStart(caller *g) {
	ctx := (*stacklessCoroNativeContext)(caller.param)
	if ctx == nil || ctx.caller != caller {
		throw("runtime: invalid stackless coroutine native start")
	}

	nativeG0 := getg()
	mp := nativeG0.m
	if nativeG0 != mp.g0 || mp.curg != caller {
		throw("runtime: invalid stackless coroutine native bootstrap")
	}

	// sys.GetCallerSP identifies the mcall frame above this function. Build a
	// fresh synthetic goroutine root below both bootstrap frames so precise
	// stack scanning never encounters mcall in the executor's call chain.
	hi := alignDown(sys.GetCallerSP()-stacklessCoroBootstrapReserve, sys.StackAlign)
	if hi <= nativeG0.stack.lo+stackGuard+stackMin {
		throw("runtime: native stack is too small for stackless coroutine executor")
	}

	executor := ctx.executor
	schedulerG := ctx.schedulerG
	ctx.nativeG0 = nativeG0
	ctx.lockedG = mp.lockedg
	ctx.lockedInt = mp.lockedInt
	ctx.g0Accurate = mp.g0StackAccurate

	executor.stack = stack{lo: nativeG0.stack.lo, hi: hi}
	executor.stackguard0 = executor.stack.lo + stackGuard
	executor.stackguard1 = ^uintptr(0)
	executor.m = mp
	executor.stacklessCoro = &ctx.gState

	totalSize := uintptr(4*goarch.PtrSize + sys.MinFrameSize)
	totalSize = alignUp(totalSize, sys.StackAlign)
	sp := hi - totalSize
	if usesLR {
		*(*uintptr)(unsafe.Pointer(sp)) = 0
		prepGoExitFrame(sp)
	}
	if GOARCH == "arm64" {
		*(*uintptr)(unsafe.Pointer(sp - goarch.PtrSize)) = 0
	}
	memclrNoHeapPointers(unsafe.Pointer(&executor.sched), unsafe.Sizeof(executor.sched))
	executor.sched.sp = sp
	executor.stktopsp = sp
	executor.sched.pc = abi.FuncPCABI0(goexit) + sys.PCQuantum
	executor.sched.g = guintptr(unsafe.Pointer(executor))
	gostartcall(&executor.sched, unsafe.Pointer(abi.FuncPCABIInternal(coroNativeMain)), nil)

	schedulerG.m = mp
	memclrNoHeapPointers(unsafe.Pointer(&schedulerG.sched), unsafe.Sizeof(schedulerG.sched))
	schedulerG.sched.sp = schedulerG.stack.hi - 4*goarch.PtrSize
	schedulerG.sched.g = guintptr(unsafe.Pointer(schedulerG))

	executor.trace.reset()
	trace := traceAcquire()
	gcController.addScannableStack(mp.p.ptr(), int64(executor.stack.hi-executor.stack.lo))
	casgstatus(executor, _Gdead, _Grunnable)
	if trace.ok() {
		trace.GoCreate(executor, executor.startpc, false)
		trace.GoPark(traceBlockGeneric, 0)
	}
	caller.waitreason = waitReasonCoroutine
	casgstatus(caller, _Grunning, _Gwaiting)
	caller.m = nil

	mp.curg = executor
	mp.lockedInt++
	if mp.lockedInt == 0 {
		throw("runtime: stackless coroutine locked thread count overflow")
	}
	mp.lockedg.set(executor)
	executor.lockedm.set(mp)
	casgstatus(executor, _Grunnable, _Grunning)
	if trace.ok() {
		trace.GoStart()
		traceRelease(trace)
	}

	// Keep nativeG0 installed as m.g0 until every runtime call above has
	// completed. Between changing m.g0 and gogo, the current G would otherwise
	// be neither m.g0 nor m.curg, so a concurrent GC transition could fail in
	// systemstack.
	sigsave(&ctx.sigmask)
	sigblock(false)
	mp.g0StackAccurate = true
	coroNativeGogo(&executor.sched, schedulerG)
}

func coroNativeMain() {
	gp := getg()
	ctx := stacklessCoroNativeContextFor(gp)
	if ctx == nil || ctx.executor != gp {
		throw("runtime: invalid stackless coroutine executor context")
	}
	msigrestore(ctx.sigmask)
	ctx.scheduler.run(true)
	mcall(coroNativeFinish)
	throw("runtime: stackless coroutine native finish returned")
}

// coroNativeFinish restores the original g0 and caller. It runs on the
// separate scheduler stack and never returns.
func coroNativeFinish(executor *g) {
	ctx := stacklessCoroNativeContextFor(executor)
	if ctx == nil || ctx.executor != executor {
		throw("runtime: invalid stackless coroutine native finish")
	}
	if ctx.gState.deferTask != nil || ctx.gState.blockingScheduler != nil {
		throw("runtime: active stackless coroutine state during native finish")
	}
	schedulerG := getg()
	mp := schedulerG.m
	caller := ctx.caller
	if schedulerG != ctx.schedulerG || mp.g0 != schedulerG || mp.curg != executor {
		throw("runtime: invalid stackless coroutine native teardown")
	}

	trace := traceAcquire()
	if trace.ok() {
		trace.GoEnd()
	}
	casgstatus(executor, _Grunning, _Gdead)
	gcController.addScannableStack(mp.p.ptr(), -int64(executor.stack.hi-executor.stack.lo))
	resetStacklessCoroExecutor(executor)

	executor.m = nil
	executor.lockedm = 0
	executor.stacklessCoro = nil
	executor.stack = stack{}
	executor.stackguard0 = 0
	executor.stackguard1 = 0
	executor.stktopsp = 0
	memclrNoHeapPointers(unsafe.Pointer(&executor.sched), unsafe.Sizeof(executor.sched))

	mp.curg = caller
	mp.lockedg = ctx.lockedG
	mp.lockedInt = ctx.lockedInt
	caller.m = mp

	casgstatus(caller, _Gwaiting, _Grunnable)
	if trace.ok() {
		trace.GoUnpark(caller, 0)
	}
	casgstatus(caller, _Grunnable, _Grunning)
	if trace.ok() {
		trace.GoStart()
		traceRelease(trace)
	}

	nativeG0 := ctx.nativeG0
	g0Accurate := ctx.g0Accurate
	ctx.nativeG0 = nil
	ctx.lockedG = 0
	ctx.lockedInt = 0
	ctx.g0Accurate = false

	// Keep schedulerG installed as m.g0 while teardown calls runtime helpers.
	// Change m.g0 only for the final non-returning switch to caller.
	sigsave(&ctx.sigmask)
	sigblock(false)
	mp.g0StackAccurate = g0Accurate
	schedulerG.m = nil
	coroNativeGogo(&caller.sched, nativeG0)
}

//go:nosplit
func stacklessCoroNativeContextFor(gp *g) *stacklessCoroNativeContext {
	state := gp.stacklessCoro
	if state == nil {
		return nil
	}
	return (*stacklessCoroNativeContext)(state.native)
}

//go:nosplit
func stacklessCoroNativeSchedulerFor(gp *g) *stacklessCoroScheduler {
	ctx := stacklessCoroNativeContextFor(gp)
	if ctx == nil {
		return nil
	}
	if ctx.scheduler == nil {
		throw("runtime: stackless coroutine blocking call has no scheduler")
	}
	return ctx.scheduler
}

// resetStacklessCoroExecutor clears state that execute and gdestroy normally
// reset for a reused G. In particular, a concurrent stack scan may request a
// synchronous preemption immediately before the executor becomes dead. That
// request has no owner after suspendG observes the dead G and must not survive
// into the next use of this synthetic executor.
func resetStacklessCoroExecutor(executor *g) {
	executor.preempt = false
	executor.preemptStop = false
	executor.preemptShrink = false
	executor.syncSafePoint = false
	executor.asyncSafePoint = false
	executor.throwsplit = false
	executor.waitreason = waitReasonZero
	executor.waitsince = 0
}
