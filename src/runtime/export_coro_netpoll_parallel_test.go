// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

func CheckStacklessCoroNetpollPopOneForTest() string {
	if stacklessCoroNetpoll.started.Load() != 0 ||
		stacklessCoroNetpoll.head.Load() != 0 ||
		stacklessCoroNetpoll.drained.Load() != 0 {
		return "netpoll parallel-pop probe is not isolated"
	}
	const n = 4
	ops := make([]*stacklessCoroOperation, n)
	for i := range ops {
		op := newStacklessCoroIOOperation()
		op.id = uint64(i + 1)
		ops[i] = op
		stacklessCoroNetpollReady(op)
	}
	seen := make([]bool, n)
	for range n {
		op := stacklessCoroNetpollPopOne(true)
		if op == nil {
			return "stacklessCoroNetpollPopOne returned nil early"
		}
		if op.resourceState().packet[stacklessCoroPollNextWord] != 0 {
			return "popped op retained non-zero next link"
		}
		idx := int(op.id - 1)
		if idx < 0 || idx >= n || seen[idx] {
			return "popped unexpected or duplicate op"
		}
		seen[idx] = true
	}
	if stacklessCoroNetpollPopOne(true) != nil || stacklessCoroNetpollPopOne(false) != nil {
		return "netpoll queue not empty after popping all ops"
	}
	// Verify empty Dekker re-arm restores wake from pdReady back to loopG without waking.
	fakeLoop := Escape(new(g))
	stacklessCoroNetpoll.loopG.Store(fakeLoop)
	stacklessCoroNetpoll.wake.Store(pdReady)
	stacklessCoroNetpollReleaseLoop()
	if got := stacklessCoroNetpoll.wake.Load(); got != uintptr(unsafe.Pointer(fakeLoop)) {
		stacklessCoroNetpoll.loopG.Store(nil)
		stacklessCoroNetpoll.wake.Store(pdNil)
		return "empty releaseLoop did not re-arm wake to loopG"
	}
	stacklessCoroNetpoll.loopG.Store(nil)
	stacklessCoroNetpoll.wake.Store(pdNil)
	KeepAlive(fakeLoop)
	KeepAlive(ops)
	return ""
}

func CheckStacklessCoroNetpollFanoutWakeForTest() string {
	if stacklessCoroNetpoll.started.Load() != 0 ||
		stacklessCoroNetpoll.head.Load() != 0 ||
		stacklessCoroNetpoll.drained.Load() != 0 {
		return "netpoll fanout probe is not isolated"
	}
	if gomaxprocs < 2 {
		return "netpoll fanout probe requires GOMAXPROCS >= 2"
	}
	s := newStacklessCoroTestScheduler()
	s.wake = make(chan struct{}, stacklessCoroWarmExecutorCount)
	s.executorState.Store(stacklessCoroExecutorStateRunning | uint32(2<<stacklessCoroExecutorWaitingShift))
	const n = 4
	ops := make([]*stacklessCoroOperation, n)
	for i := range ops {
		op := newStacklessCoroIOOperation()
		op.id = uint64(i + 1)
		task := new(stacklessCoroTask)
		task.context.scheduler = s
		op.task = task
		ops[i] = op
		stacklessCoroNetpollReady(op)
	}
	// Single-scheduler batch must stay on the zero-overhead fast path (no
	// same-scheduler wake storm) and clear any stale multiSched hint.
	stacklessCoroNetpoll.multiSched.Store(1)
	first := stacklessCoroNetpollPopOne(true)
	if first == nil {
		return "first netpoll pop returned nil"
	}
	if got := len(s.wake); got != 0 {
		return "single-scheduler netpoll pop unexpectedly fanned out wakes"
	}
	if stacklessCoroNetpoll.multiSched.Load() != 0 {
		return "single-scheduler refill did not reset multiSched hint"
	}
	// Drain remaining ops.
	for range n - 1 {
		if stacklessCoroNetpollPopOne(true) == nil {
			return "remaining netpoll pop returned nil"
		}
	}
	if got := len(s.wake); got != 0 {
		return "single-scheduler drain sent unexpected wakes"
	}
	stacklessCoroNetpoll.wake.Store(pdNil)
	stacklessCoroNetpoll.lastSched.Store(0)
	stacklessCoroNetpoll.multiSched.Store(0)
	KeepAlive(ops)
	return ""
}

func CheckStacklessCoroNetpollDualSchedulerFanoutForTest() string {
	if stacklessCoroNetpoll.started.Load() != 0 ||
		stacklessCoroNetpoll.head.Load() != 0 ||
		stacklessCoroNetpoll.drained.Load() != 0 {
		return "dual-scheduler netpoll fanout probe is not isolated"
	}
	if gomaxprocs < 4 {
		return "dual-scheduler netpoll fanout probe requires GOMAXPROCS >= 4"
	}
	makeOp := func(id uint64, s *stacklessCoroScheduler) *stacklessCoroOperation {
		op := newStacklessCoroIOOperation()
		op.id = id
		task := new(stacklessCoroTask)
		task.context.scheduler = s
		op.task = task
		return op
	}
	// Case 1: first popped op belongs to s1 (0 waiting executors), followed by
	// another s1 op and two s2 ops (s2 has 2 waiting executors).
	// Note stacklessCoroNetpollReady prepends LIFO, so push in reverse order.
	s1 := newStacklessCoroTestScheduler()
	s1.wake = make(chan struct{}, stacklessCoroWarmExecutorCount)
	s1.executorState.Store(stacklessCoroExecutorStateRunning)

	s2 := newStacklessCoroTestScheduler()
	s2.wake = make(chan struct{}, stacklessCoroWarmExecutorCount)
	s2.executorState.Store(stacklessCoroExecutorStateRunning | uint32(2<<stacklessCoroExecutorWaitingShift))

	ops1 := []*stacklessCoroOperation{
		makeOp(1, s2),
		makeOp(2, s2),
		makeOp(3, s1),
		makeOp(4, s1), // popped first (LIFO head)
	}
	for _, op := range ops1 {
		stacklessCoroNetpollReady(op)
	}
	first := stacklessCoroNetpollPopOne(true)
	if first != ops1[3] {
		return "unexpected first op in dual-scheduler fanout case 1"
	}
	if len(s1.wake) != 0 {
		return "signaled s1 with zero waiting executors in case 1"
	}
	if len(s2.wake) != 2 {
		return "did not wake s2 waiting executors behind s1 op in case 1"
	}
	// Drain remaining ops with waiting=0.
	s2.executorState.Store(stacklessCoroExecutorStateRunning)
	for range len(ops1) - 1 {
		if stacklessCoroNetpollPopOne(true) == nil {
			return "short drain in dual-scheduler fanout case 1"
		}
	}

	// Case 2: reverse fallback — first popped op belongs to s1 (2 waiting),
	// followed by two s2 ops (0 waiting). Idle s1 executors should still be
	// woken to help drain the shared global netpoll queue in parallel.
	s1.wake = make(chan struct{}, stacklessCoroWarmExecutorCount)
	s1.executorState.Store(stacklessCoroExecutorStateRunning | uint32(2<<stacklessCoroExecutorWaitingShift))
	s2.wake = make(chan struct{}, stacklessCoroWarmExecutorCount)
	s2.executorState.Store(stacklessCoroExecutorStateRunning)

	ops2 := []*stacklessCoroOperation{
		makeOp(11, s2),
		makeOp(12, s2),
		makeOp(13, s1), // popped first
	}
	for _, op := range ops2 {
		stacklessCoroNetpollReady(op)
	}
	first2 := stacklessCoroNetpollPopOne(true)
	if first2 != ops2[2] {
		return "unexpected first op in dual-scheduler fanout case 2"
	}
	if len(s1.wake) != 2 || len(s2.wake) != 0 {
		return "fallback wake did not signal s1 waiting executors in case 2"
	}
	s1.executorState.Store(stacklessCoroExecutorStateRunning)
	for range len(ops2) - 1 {
		if stacklessCoroNetpollPopOne(true) == nil {
			return "short drain in dual-scheduler fanout case 2"
		}
	}
	stacklessCoroNetpoll.wake.Store(pdNil)
	stacklessCoroNetpoll.lastSched.Store(0)
	stacklessCoroNetpoll.multiSched.Store(0)
	KeepAlive(ops1)
	KeepAlive(ops2)
	return ""
}

func CheckStacklessCoroOperationRegistryShardsForTest(workers, perWorker int) string {
	done := make(chan string, workers)
	for range workers {
		go func() {
			ids := make([]uint64, perWorker)
			ops := make([]*stacklessCoroOperation, perWorker)
			for i := range perWorker {
				op := new(stacklessCoroOperation)
				ops[i] = op
				id := registerStacklessCoroOperation(op)
				if id == 0 {
					done <- "registered zero operation id"
					return
				}
				ids[i] = id
			}
			for i := range perWorker {
				if findStacklessCoroOperation(ids[i]) != ops[i] {
					done <- "missing registered operation"
					return
				}
				if takeStacklessCoroOperation(ids[i]) != ops[i] {
					done <- "mismatched taken operation"
					return
				}
				if takeStacklessCoroOperation(ids[i]) != nil {
					done <- "duplicate operation removal succeeded"
					return
				}
			}
			done <- ""
		}()
	}
	for range workers {
		if msg := <-done; msg != "" {
			return msg
		}
	}
	return ""
}
