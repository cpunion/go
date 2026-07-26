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
	stacklessCoroExecutorCount = 4
	stacklessCoroSchedulerSize = 64 << 10

	// coroNativeStart runs in a frame entered by mcall on the original g0
	// stack. Leave that frame and its caller above the executor's synthetic
	// stack root.
	stacklessCoroBootstrapReserve = 4 << 10
)

type stacklessCoroNativeContext struct {
	scheduler  *stacklessCoroScheduler
	schedulerG *g
	executor   *g
	caller     *g
	nativeG0   *g
	lockedG    guintptr
	lockedInt  uint32
	g0Accurate bool
}

var stacklessCoroNativePool struct {
	lock      mutex
	count     int
	available chan *stacklessCoroNativeContext
}

func init() {
	lockInit(&stacklessCoroNativePool.lock, lockRankLeafRank)
	stacklessCoroNativePool.available = make(chan *stacklessCoroNativeContext, stacklessCoroExecutorCount)
}

// coroRunOnNativeStack runs s on a fixed portion of the current operating
// system thread stack. The race runtime has its own stack and goroutine
// bookkeeping, so race builds retain the managed-stack driver for now.
func coroRunOnNativeStack(s *stacklessCoroScheduler) bool {
	if raceenabled {
		return false
	}

	ctx := acquireStacklessCoroNativeContext()
	gp := getg()
	if gp != gp.m.curg || gp == gp.m.g0 || gp == gp.m.gsignal {
		releaseStacklessCoroNativeContext(ctx)
		return false
	}
	if gp.param != nil {
		releaseStacklessCoroNativeContext(ctx)
		throw("runtime: stackless coroutine caller has pending parameter")
	}

	ctx.scheduler = s
	ctx.caller = gp
	gp.param = unsafe.Pointer(ctx)
	mcall(coroNativeStart)

	if gp.param != unsafe.Pointer(ctx) {
		throw("runtime: lost stackless coroutine native context")
	}
	gp.param = nil
	ctx.scheduler = nil
	ctx.caller = nil
	releaseStacklessCoroNativeContext(ctx)
	return true
}

func acquireStacklessCoroNativeContext() *stacklessCoroNativeContext {
	select {
	case ctx := <-stacklessCoroNativePool.available:
		return ctx
	default:
	}
	lock(&stacklessCoroNativePool.lock)
	if stacklessCoroNativePool.count < stacklessCoroExecutorCount {
		stacklessCoroNativePool.count++
		unlock(&stacklessCoroNativePool.lock)
		return newStacklessCoroNativeContext()
	}
	available := stacklessCoroNativePool.available
	unlock(&stacklessCoroNativePool.lock)
	return <-available
}

func releaseStacklessCoroNativeContext(ctx *stacklessCoroNativeContext) {
	stacklessCoroNativePool.available <- ctx
}

func newStacklessCoroNativeContext() *stacklessCoroNativeContext {
	ctx := new(stacklessCoroNativeContext)

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
	executor.stackFixed = true
	executor.m = mp
	executor.stacklessCoro = unsafe.Pointer(ctx)

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

	mp.g0 = schedulerG
	mp.g0StackAccurate = true
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
	gogo(&executor.sched)
}

func coroNativeMain() {
	gp := getg()
	ctx := (*stacklessCoroNativeContext)(gp.stacklessCoro)
	if ctx == nil || ctx.executor != gp {
		throw("runtime: invalid stackless coroutine executor context")
	}
	ctx.scheduler.run(true)
	mcall(coroNativeFinish)
	throw("runtime: stackless coroutine native finish returned")
}

// coroNativeFinish restores the original g0 and caller. It runs on the
// separate scheduler stack and never returns.
func coroNativeFinish(executor *g) {
	ctx := (*stacklessCoroNativeContext)(executor.stacklessCoro)
	if ctx == nil || ctx.executor != executor {
		throw("runtime: invalid stackless coroutine native finish")
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

	executor.m = nil
	executor.lockedm = 0
	executor.stacklessCoro = nil
	executor.stack = stack{}
	executor.stackguard0 = 0
	executor.stackguard1 = 0
	executor.stktopsp = 0
	executor.stackFixed = false
	memclrNoHeapPointers(unsafe.Pointer(&executor.sched), unsafe.Sizeof(executor.sched))

	mp.g0 = ctx.nativeG0
	mp.g0StackAccurate = ctx.g0Accurate
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

	ctx.nativeG0 = nil
	ctx.lockedG = 0
	ctx.lockedInt = 0
	ctx.g0Accurate = false
	schedulerG.m = nil
	gogo(&caller.sched)
}
