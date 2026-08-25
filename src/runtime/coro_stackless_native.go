// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/atomic"
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
	gState             stacklessCoroGState
	scheduler          *stacklessCoroScheduler
	freeTasks          *stacklessCoroTask
	freePlainTaskCount uint16
	freeFrameBytes     uint16
	cachedFrameTasks   uint16
	cachedFrameBytes   uint16
	schedulerG         *g
	executor           *g
	caller             *g
	nativeG0           *g
	lockedG            guintptr
	lockedInt          uint32
	g0Accurate         bool
	initialRoot        bool
	poolNext           *stacklessCoroNativeContext
}

// stacklessCoroNativeDriver owns one native context across the fixed-stack
// episodes of a public root. Replacement executors use a driver for one
// episode and park on their ordinary G between activations.
type stacklessCoroNativeDriver struct {
	context *stacklessCoroNativeContext
	root    bool
}

var stacklessCoroNativePool struct {
	lock  mutex
	count int
	// slots preserve the common lock-free reuse path. Contexts beyond the
	// warm capacity remain reusable through overflow.
	slots    [stacklessCoroWarmExecutorCount]atomic.Pointer[stacklessCoroNativeContext]
	overflow *stacklessCoroNativeContext
}

func init() {
	lockInit(&stacklessCoroNativePool.lock, lockRankLeafRank)
}

// run drives one episode on a fixed portion of the current operating system
// thread stack. It returns s reloaded from the heap context after a successful
// stack switch; callers must not reuse their pre-switch local. The race
// runtime has its own stack and goroutine bookkeeping, so race builds retain
// the managed-stack driver for now.
func (d *stacklessCoroNativeDriver) run(s *stacklessCoroScheduler,
	initial bool) *stacklessCoroScheduler {
	if raceenabled {
		return nil
	}
	if d.context == nil {
		// A newly created root is the only scheduler that is both off and has
		// one executor. Replacement executors are prepared before they start.
		d.root = s.executorState.Load() == stacklessCoroExecutorStateOff &&
			s.executorCount.Load() == 1
		d.context = acquireStacklessCoroNativeContext()
		if d.root && d.context.freeTasks != nil {
			s.freeTasks = d.context.freeTasks
			s.freePlainTaskCount = d.context.freePlainTaskCount
			s.freeFrameBytes = d.context.freeFrameBytes
			s.cachedFrameTasks = d.context.cachedFrameTasks
			s.cachedFrameBytes = d.context.cachedFrameBytes
			d.context.freeTasks = nil
			d.context.freePlainTaskCount = 0
			d.context.freeFrameBytes = 0
			d.context.cachedFrameTasks = 0
			d.context.cachedFrameBytes = 0
		}
	}

	ctx := d.context
	gp := getg()
	if gp != gp.m.curg || gp == gp.m.g0 || gp == gp.m.gsignal {
		return nil
	}
	if gp.param != nil {
		throw("runtime: stackless coroutine caller has pending parameter")
	}

	ctx.scheduler = s
	ctx.caller = gp
	ctx.initialRoot = initial
	gp.param = unsafe.Pointer(ctx)
	mcall(coroNativeStart)
	mp := gp.m
	if mp == nil || mp.curg != gp || mp.g0 != ctx.schedulerG || ctx.schedulerG.m != mp || ctx.nativeG0 == nil {
		throw("runtime: invalid stackless coroutine native return")
	}
	mp.g0 = ctx.nativeG0
	mp.g0StackAccurate = ctx.g0Accurate
	ctx.schedulerG.m = nil
	ctx.nativeG0 = nil
	ctx.lockedG = 0
	ctx.lockedInt = 0
	ctx.g0Accurate = false

	if gp.param != unsafe.Pointer(ctx) {
		throw("runtime: lost stackless coroutine native context")
	}
	gp.param = nil
	scheduler := ctx.scheduler
	ctx.scheduler = nil
	ctx.caller = nil
	return scheduler
}

// close releases the native context. A completed public root returns its
// bounded task cache to that context only after its final episode.
func (d *stacklessCoroNativeDriver) close(s *stacklessCoroScheduler, rootComplete bool) {
	ctx := d.context
	if ctx == nil {
		return
	}
	if d.root && rootComplete &&
		s.executorState.Load() == stacklessCoroExecutorStateOff &&
		s.freeTasks != nil {
		s.discardFreeOverflowTasks()
		ctx.freeTasks = s.freeTasks
		ctx.freePlainTaskCount = s.freePlainTaskCount
		ctx.freeFrameBytes = s.freeFrameBytes
		ctx.cachedFrameTasks = s.cachedFrameTasks
		ctx.cachedFrameBytes = s.cachedFrameBytes
		s.freeTasks = nil
		s.freePlainTaskCount = 0
		s.freeFrameBytes = 0
		s.cachedFrameTasks = 0
		s.cachedFrameBytes = 0
	}
	releaseStacklessCoroNativeContext(ctx)
	d.context = nil
	d.root = false
}

// coroRunOnNativeStack drives one replacement episode with a short-lived
// driver. Public roots keep their driver across idle episodes instead.
func coroRunOnNativeStack(s *stacklessCoroScheduler) *stacklessCoroScheduler {
	var native stacklessCoroNativeDriver
	scheduler := native.run(s, false)
	if scheduler != nil {
		native.close(scheduler, false)
	} else {
		native.close(s, false)
	}
	return scheduler
}

func acquireStacklessCoroNativeContext() *stacklessCoroNativeContext {
	for i := range stacklessCoroNativePool.slots {
		slot := &stacklessCoroNativePool.slots[i]
		ctx := slot.Load()
		if ctx != nil && slot.CompareAndSwap(ctx, nil) {
			return ctx
		}
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
	for i := range stacklessCoroNativePool.slots {
		if stacklessCoroNativePool.slots[i].CompareAndSwap(nil, ctx) {
			return
		}
	}
	// Only the bounded warm pool retains task caches. The overflow list may
	// grow with peak root concurrency and must not retain one cache per peak.
	ctx.freeTasks = nil
	ctx.freePlainTaskCount = 0
	ctx.freeFrameBytes = 0
	ctx.cachedFrameTasks = 0
	ctx.cachedFrameBytes = 0
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
	// completed. gogo performs the signal-safe G and stack transition; the
	// executor replaces m.g0 only after it is running on its target stack.
	gogo(&executor.sched)
}

func coroNativeMain() {
	gp := getg()
	ctx := stacklessCoroNativeContextFor(gp)
	if ctx == nil || ctx.executor != gp || gp.m.g0 != ctx.nativeG0 || ctx.schedulerG.m != gp.m {
		throw("runtime: invalid stackless coroutine executor context")
	}
	gp.m.g0 = ctx.schedulerG
	gp.m.g0StackAccurate = true
	initial := ctx.initialRoot
	ctx.initialRoot = false
	ctx.scheduler.runTasks(true, false, initial)
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

	// Keep schedulerG installed as m.g0 through gogo's signal-safe G and stack
	// transition. The caller restores nativeG0 after it is running again.
	gogo(&caller.sched)
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
