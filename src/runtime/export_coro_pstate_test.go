// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

type stacklessCoroPStateTestFrameA struct {
	header stacklessCoroFusedResumeFrameHeader
	data   [4]uintptr
}

type stacklessCoroPStateTestFrameB struct {
	header stacklessCoroFusedResumeFrameHeader
	data   [12]uintptr
}

func stacklessCoroPStateTestResumeA(unsafe.Pointer) uint8 { return stacklessCoroActionComplete }
func stacklessCoroPStateTestResumeB(unsafe.Pointer) uint8 { return stacklessCoroActionComplete }
func stacklessCoroPStateShrinkResume(ctx unsafe.Pointer) uint8 {
	context := (*stacklessCoroContext)(ctx)
	(*atomic.Int32)(context.frame).Add(1)
	return stacklessCoroActionComplete
}

func CheckStacklessCoroTaskLifecycleAtomicsForTest() string {
	// 1. Normal park before completion: parkWaitAtomic succeeds, then completeWaitAtomic returns early=false.
	task := new(stacklessCoroTask)
	task.setLifecycle(taskWordWaitingResuming)
	if !task.parkWaitAtomic() {
		return "parkWaitAtomic failed without early ready"
	}
	if task.state != stacklessCoroTaskWaiting || task.resuming || task.readyPending {
		return "unexpected state after parkWaitAtomic"
	}
	if task.completeWaitAtomic(0) {
		return "completeWaitAtomic reported early on idle waiting task"
	}
	if task.state != stacklessCoroTaskRunnable || task.resuming || task.readyPending {
		return "unexpected state after idle completeWaitAtomic"
	}

	// 2. Early completion before park: completeWaitAtomic sets readyPending, parkWaitAtomic sees it.
	task.setLifecycle(taskWordWaitingResuming)
	if !task.completeWaitAtomic(0) {
		return "completeWaitAtomic did not report early on resuming task"
	}
	if task.state != stacklessCoroTaskWaiting || !task.resuming || !task.readyPending {
		return "unexpected state after early completeWaitAtomic"
	}
	if task.parkWaitAtomic() {
		return "parkWaitAtomic unexpectedly succeeded despite readyPending"
	}

	// 3. Panic set (+1) and deferred Goexit clear (-1) update terminal atomically.
	task.setLifecycle(taskWordWaitingResuming)
	if !task.completeWaitAtomic(1) || task.terminal != stacklessCoroTerminalPanic || !task.readyPending {
		return "completeWaitAtomic(+1) did not set panic terminal atomically"
	}
	task.setLifecycle(packTaskLifecycle(stacklessCoroTaskWaiting, false, false, stacklessCoroTerminalPanic))
	if task.completeWaitAtomic(-1) || task.state != stacklessCoroTaskRunnable || task.terminal != stacklessCoroTerminalNone {
		return "completeWaitAtomic(-1) did not clear terminal atomically"
	}

	// 4. takeIdleWaitingAtomic claims suspended waiting task and rejects resuming/readyPending.
	task.setLifecycle(packTaskLifecycle(stacklessCoroTaskWaiting, false, false, stacklessCoroTerminalNone))
	if !task.takeIdleWaitingAtomic() {
		return "takeIdleWaitingAtomic failed on idle waiting task"
	}
	if task.state != stacklessCoroTaskRunning || !task.resuming {
		return "unexpected state after takeIdleWaitingAtomic"
	}
	if task.takeIdleWaitingAtomic() {
		return "takeIdleWaitingAtomic unexpectedly succeeded on running task"
	}
	return ""
}

func CheckStacklessCoroPStateCachesAndGCForTest() string {
	if raceenabled {
		return ""
	}
	// Per-P channel and IO op caches.
	chOp := newStacklessCoroChannelOperation()
	ioOp := newStacklessCoroIOOperation()
	if !coroPOpCachePut(chOp) || coroPOpCacheGet() != chOp {
		return "channel per-P op cache round-trip failed"
	}
	if !coroPIOOpCachePut(ioOp) || coroPIOOpCacheGet() != ioOp {
		return "IO per-P op cache round-trip failed"
	}

	// Per-P fused frame cache matching by resume + size class, and ring eviction.
	idA := stacklessCoroResumeIdentity(stacklessCoroPStateTestResumeA)
	idB := stacklessCoroResumeIdentity(stacklessCoroPStateTestResumeB)
	fA := unsafe.Pointer(new(stacklessCoroPStateTestFrameA))
	fB := unsafe.Pointer(new(stacklessCoroPStateTestFrameB))
	coroPFusedFramePut(idA, fA)
	coroPFusedFramePut(idB, fB)
	if coroPFusedFrameGet(idA, unsafe.Sizeof(stacklessCoroPStateTestFrameB{})) != nil {
		return "fused frame cache returned mismatched size class"
	}
	if coroPFusedFrameGet(idA, unsafe.Sizeof(stacklessCoroPStateTestFrameA{})) != fA {
		return "fused frame cache miss for frame A"
	}
	if coroPFusedFrameGet(idB, unsafe.Sizeof(stacklessCoroPStateTestFrameB{})) != fB {
		return "fused frame cache miss for frame B"
	}

	// Fill beyond ring capacity and confirm STW GC clears all idle P states.
	for range stacklessCoroPFusedCacheSize + 4 {
		coroPFusedFramePut(idA, unsafe.Pointer(new(stacklessCoroPStateTestFrameA)))
	}
	coroPOpCachePut(newStacklessCoroChannelOperation())
	coroPIOOpCachePut(newStacklessCoroIOOperation())
	GC()
	if coroPOpCacheGet() != nil || coroPIOOpCacheGet() != nil ||
		coroPFusedFrameGet(idA, unsafe.Sizeof(stacklessCoroPStateTestFrameA{})) != nil {
		return "STW GC did not clear idle per-P coroutine caches"
	}
	return ""
}

func CheckStacklessCoroPRunqShrinkDrainForTest() string {
	if raceenabled {
		return ""
	}
	// Run with 4 Ps, deterministically enqueue executable runnable tasks into
	// the local coroP.runq of Ps that will be destroyed (id >= 1), assert they
	// are present in those destroyed-P rings BEFORE shrinking GOMAXPROCS(4->1),
	// then verify drainStacklessCoroPState transfers them onto s.head and every
	// task executes exactly once.
	prev := GOMAXPROCS(4)
	defer GOMAXPROCS(prev)

	s := &stacklessCoroScheduler{
		wake: make(chan struct{}, stacklessCoroWarmExecutorCount),
	}
	s.executorState.Store(stacklessCoroExecutorStateRunning)

	const total = 32
	const wantDestroyed = 16
	tasks := make([]*stacklessCoroTask, total)
	executed := make([]atomic.Int32, total)
	for i := range total {
		task := new(stacklessCoroTask)
		task.resume = stacklessCoroPStateShrinkResume
		task.context.frame = unsafe.Pointer(&executed[i])
		task.context.scheduler = s
		rec := &stacklessCoroGoroutine{
			scheduler:  s,
			inRegistry: true,
		}
		task.setSource(rec)
		task.setLifecycle(taskWordRunnable)
		tasks[i] = task
	}

	// Keep 4 concurrent workers active so multiple Ps (including id >= 1)
	// execute simultaneously. The first wantDestroyed tasks must land on a P
	// with id >= 1 that GOMAXPROCS(1) will destroy.
	nextDestroyed := new(atomic.Int32)
	activeWorkers := new(atomic.Int32)
	const workers = 4
	ready := make(chan struct{}, workers)
	goDone := make(chan struct{}, workers)
	for range workers {
		go func() {
			defer func() { goDone <- struct{}{} }()
			activeWorkers.Add(1)
			ready <- struct{}{}
			for activeWorkers.Load() < workers {
				Gosched()
			}
			for {
				idx := int(nextDestroyed.Load())
				if idx >= wantDestroyed {
					return
				}
				mp := acquirem()
				pp := mp.p.ptr()
				onDestroyed := pp != nil && pp.id >= 1
				releasem(mp)
				if !onDestroyed {
					// Keep P0 briefly occupied while P1..P3 run their workers.
					procyield(30)
					Gosched()
					continue
				}
				claimed := int(nextDestroyed.Add(1)) - 1
				if claimed >= wantDestroyed {
					return
				}
				// Re-check P ownership right as we push; if preempted back to P0,
				// push anyway and the explicit allp[1:] assertion below still
				// verifies real residency on Ps to be destroyed.
				if _, ok := coroRunqPut(s, tasks[claimed]); !ok {
					lock(&s.lock)
					s.enqueueRunnableLocked(tasks[claimed])
					unlock(&s.lock)
				}
			}
		}()
	}
	for range workers {
		<-ready
	}
	for range workers {
		<-goDone
	}

	// Place remaining tasks across Ps (including P0).
	for i := wantDestroyed; i < total; i++ {
		if _, ok := coroRunqPut(s, tasks[i]); !ok {
			lock(&s.lock)
			s.enqueueRunnableLocked(tasks[i])
			unlock(&s.lock)
		}
	}

	// Explicitly assert that tasks are actually queued in the local coroP.runq
	// of Ps with id >= 1 BEFORE calling GOMAXPROCS(1).
	destroyedBefore := 0
	mp := acquirem()
	for idx := 1; idx < len(allp); idx++ {
		pp := allp[idx]
		if pp == nil {
			continue
		}
		if ps := pp.coroP.Load(); ps != nil && ps.runqSched.Load() == s {
			h := atomic.LoadAcq(&ps.runqHead)
			t := atomic.LoadAcq(&ps.runqTail)
			if t > h {
				destroyedBefore += int(t - h)
			}
		}
	}
	releasem(mp)
	if destroyedBefore == 0 {
		return "no tasks were queued in coroP.runq of destroyed Ps (id >= 1) before GOMAXPROCS shrink"
	}

	// Shrink GOMAXPROCS 4 -> 1, invoking p.destroy() -> drainStacklessCoroPState.
	GOMAXPROCS(1)

	// Verify the drained tasks landed on s.head before we start taking.
	lock(&s.lock)
	headCount := 0
	for cur := s.head; cur != nil; cur = cur.next {
		headCount++
	}
	unlock(&s.lock)
	if headCount < destroyedBefore {
		return "drainStacklessCoroPState did not move destroyed-P ring tasks onto scheduler queue"
	}

	seen := make([]bool, total)
	got := 0
	for {
		task := s.take()
		if task == nil {
			break
		}
		found := -1
		for i, cand := range tasks {
			if cand == task {
				found = i
				break
			}
		}
		if found < 0 || seen[found] {
			return "unexpected or duplicate task drained after GOMAXPROCS shrink"
		}
		seen[found] = true
		if action := task.resume(unsafe.Pointer(&task.context)); action != stacklessCoroActionComplete {
			return "drained task resume returned unexpected action"
		}
		got++
	}
	if got != total {
		return "lost runnable task from per-P coroRunq across GOMAXPROCS shrink"
	}
	for i := range total {
		if executed[i].Load() != 1 {
			return "drained task did not execute exactly once"
		}
	}
	if s.runnableState.Load() != 0 {
		return "non-zero runnableState after draining all tasks"
	}
	return ""
}
