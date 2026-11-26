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
	// First pop sees 3 extra queued ops and 2 waiting executors -> should send
	// min(extra, waiting, gomaxprocs-1) signals to s.wake.
	first := stacklessCoroNetpollPopOne(true)
	if first == nil {
		return "first netpoll pop returned nil"
	}
	wantSignals := min(min(n-1, int(gomaxprocs)-1), 2)
	if got := len(s.wake); got != wantSignals {
		return "unexpected netpoll fanout signal count"
	}
	// Drain remaining ops with waiting=0 -> must not add extra signals.
	s.executorState.Store(stacklessCoroExecutorStateRunning)
	for range n - 1 {
		if stacklessCoroNetpollPopOne(true) == nil {
			return "remaining netpoll pop returned nil"
		}
	}
	if got := len(s.wake); got != wantSignals {
		return "netpoll fanout signaled with zero waiting executors"
	}
	stacklessCoroNetpoll.wake.Store(pdNil)
	KeepAlive(ops)
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
