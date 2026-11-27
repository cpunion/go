// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

func s6NetpollDummyResume(unsafe.Pointer) uint8 {
	return stacklessCoroActionComplete
}

// CheckStacklessCoroDirectIOAndShardsForTest verifies:
//  1. Per-P opID batching produces strictly unique non-zero IDs across batch boundaries.
//  2. Sharded ioCount / ioMask tracks registrations across all 16 registry buckets.
//  3. completeStacklessCoroOperationDirect claims an idle waiting task directly for
//     wantSched and returns nil (falling back to multi-P completion) for resuming tasks.
func CheckStacklessCoroDirectIOAndShardsForTest() string {
	seen := make(map[uint64]struct{}, 2500)
	for range 2500 {
		id := nextStacklessCoroOperationIDMultiP()
		if id == 0 {
			return "nextStacklessCoroOperationIDMultiP returned 0"
		}
		if _, dup := seen[id]; dup {
			return "duplicate operation ID across batch boundary"
		}
		seen[id] = struct{}{}
	}

	before := stacklessCoroRegisteredIOCount()
	ops := make([]*stacklessCoroOperation, 32)
	ids := make([]uint64, 32)
	for i := range ops {
		op := newStacklessCoroIOOperation()
		op.ensureIOState()
		ops[i] = op
		ids[i] = registerStacklessCoroOperation(op)
	}
	if !stacklessCoroHasRegisteredIO() {
		return "stacklessCoroHasRegisteredIO false while 32 IO ops registered"
	}
	if got := stacklessCoroRegisteredIOCount(); got != before+32 {
		return "stacklessCoroRegisteredIOCount mismatch during registration"
	}
	for i := range ops {
		if takeStacklessCoroOperation(ids[i]) != ops[i] {
			return "takeStacklessCoroOperation did not return registered op"
		}
	}
	if got := stacklessCoroRegisteredIOCount(); got != before {
		return "stacklessCoroRegisteredIOCount did not return to baseline"
	}
	if before == 0 && stacklessCoroHasRegisteredIO() {
		return "stacklessCoroHasRegisteredIO remained true after unregistering all IO ops"
	}

	if gomaxprocs > 1 && !raceenabled {
		s := new(stacklessCoroScheduler)
		if !stacklessCoroIsPullComparison(s) {
			s.wake = make(chan struct{}, stacklessCoroWarmExecutorCount)
			lockInit(&s.lock, lockRankStacklessCoro)
			s.executorState.Store(stacklessCoroExecutorStateRunning)
			s.executorCount.Store(2)
			task := new(stacklessCoroTask)
			task.resume = s6NetpollDummyResume
			task.context.scheduler = s
			rec := new(stacklessCoroGoroutine)
			rec.scheduler = s
			rec.inRegistry = true
			task.setSource(rec)
			task.setLifecycle(packTaskLifecycle(stacklessCoroTaskWaiting, false, false, stacklessCoroTerminalNone))
			op := newStacklessCoroIOOperation()
			op.ensureIOState()
			op.id = 1
			op.registeredIO = true
			op.task = task

			claimed := completeStacklessCoroOperationDirect(op, s)
			if claimed != task {
				return "completeStacklessCoroOperationDirect did not claim idle waiting task"
			}
			if task.state != stacklessCoroTaskRunning || !task.resuming {
				return "direct claimed task is not in Running+resuming state"
			}
			task.setLifecycle(taskWordWaitingResuming)
			op2 := newStacklessCoroIOOperation()
			op2.ensureIOState()
			op2.id = 2
			op2.registeredIO = true
			op2.task = task
			if claimed2 := completeStacklessCoroOperationDirect(op2, s); claimed2 != nil {
				return "completeStacklessCoroOperationDirect claimed still-resuming task"
			}
			if !task.readyPending {
				return "resuming task was not marked readyPending after fallback completion"
			}
		}
	}
	return ""
}

// CheckStacklessCoroNetpollThresholdFanoutForTest pushes 12 ready ops for one
// scheduler via stacklessCoroNetpollReady and verifies that refilling + popping
// the first op wakes rem/4 = 11/4 = 2 waiting executors via single-sched fanout.
func CheckStacklessCoroNetpollThresholdFanoutForTest() string {
	if stacklessCoroNetpoll.started.Load() != 0 ||
		stacklessCoroNetpoll.head.Load() != 0 ||
		stacklessCoroNetpoll.drained.Load() != 0 {
		return "netpoll threshold fanout probe is not isolated"
	}
	if gomaxprocs < 2 {
		return "netpoll threshold fanout probe requires GOMAXPROCS >= 2"
	}
	s := newStacklessCoroTestScheduler()
	s.wake = make(chan struct{}, stacklessCoroWarmExecutorCount)
	s.executorState.Store(stacklessCoroExecutorStateRunning | uint32(3<<stacklessCoroExecutorWaitingShift))
	s.executorCount.Store(4)

	const total = 12
	ops := make([]*stacklessCoroOperation, total)
	for i := range ops {
		op := newStacklessCoroIOOperation()
		op.id = uint64(i + 1)
		task := new(stacklessCoroTask)
		task.context.scheduler = s
		op.task = task
		ops[i] = op
		stacklessCoroNetpollReady(op)
	}

	first := stacklessCoroNetpollPopOne(true)
	if first == nil {
		return "first netpoll pop returned nil"
	}
	wakesAfterFirst := len(s.wake)
	for range total - 1 {
		if stacklessCoroNetpollPopOne(true) == nil {
			return "remaining netpoll pop returned nil"
		}
	}
	stacklessCoroNetpoll.wake.Store(pdNil)
	stacklessCoroNetpoll.lastSched.Store(0)
	stacklessCoroNetpoll.multiSched.Store(0)
	KeepAlive(ops)

	// With rem=11 (>=4), rem/4 = 2 wakeups (capped by waiting=3) should have
	// been delivered into s.wake when multiPFastPath() is enabled.
	if s.multiPFastPath() {
		if wakesAfterFirst < 2 {
			return "single-scheduler threshold fanout did not wake expected waiting executors"
		}
	} else if wakesAfterFirst != 0 {
		return "single-scheduler threshold fanout unexpectedly woke executors outside multiPFastPath"
	}
	return ""
}
