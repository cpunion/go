// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "unsafe"

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
	// Run with 4 Ps, enqueue eligible runnable tasks on Ps, then shrink GOMAXPROCS(1)
	// and verify drainStacklessCoroPState moves every queued task onto s.head so none are lost.
	prev := GOMAXPROCS(4)
	defer GOMAXPROCS(prev)

	s := newStacklessCoroTestScheduler()
	s.wake = make(chan struct{}, stacklessCoroWarmExecutorCount)
	s.executorState.Store(stacklessCoroExecutorStateRunning)

	const total = 32
	tasks := make([]*stacklessCoroTask, total)
	seen := make([]bool, total)
	for i := range total {
		task := new(stacklessCoroTask)
		rec := &stacklessCoroGoroutine{
			scheduler:  s,
			inRegistry: true,
		}
		task.setSource(rec)
		task.setLifecycle(taskWordRunnable)
		tasks[i] = task
	}
	// Push tasks across multiple goroutines so they spread across Ps.
	done := make(chan struct{}, total)
	for i := range total {
		task := tasks[i]
		go func() {
			if _, ok := coroRunqPut(s, task); !ok {
				lock(&s.lock)
				s.enqueueRunnableLocked(task)
				unlock(&s.lock)
			}
			done <- struct{}{}
		}()
	}
	for range total {
		<-done
	}

	// Shrink GOMAXPROCS to 1, invoking p.destroy() -> drainStacklessCoroPState.
	GOMAXPROCS(1)

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
		got++
	}
	if got != total {
		return "lost runnable task from per-P coroRunq across GOMAXPROCS shrink"
	}
	if s.runnableState.Load() != 0 {
		return "non-zero runnableState after draining all tasks"
	}
	return ""
}
