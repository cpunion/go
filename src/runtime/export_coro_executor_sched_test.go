// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

// StacklessCoroExecutorCountsForTest returns the packed executor lifecycle
// state, parked waiter count, and admitted warm replacement count for ctx's
// scheduler.
func StacklessCoroExecutorCountsForTest(ctx unsafe.Pointer) (state, waiting, admitted uint32) {
	s := (*stacklessCoroContext)(ctx).scheduler
	v := s.executorState.Load()
	return v & stacklessCoroExecutorStateMask,
		(v & stacklessCoroExecutorWaitingMask) >> stacklessCoroExecutorWaitingShift,
		v >> stacklessCoroExecutorAdmittedShift
}

// StacklessCoroSignalIfWaitingForTest exercises signalIfWaiting when no
// executor is parked vs when one is parked in waitForWork.
func StacklessCoroSignalIfWaitingForTest() bool {
	s := newStacklessCoroTestScheduler()
	s.wake = make(chan struct{}, stacklessCoroWarmExecutorCount)
	s.executorState.Store(stacklessCoroExecutorStateRunning)
	s.signalIfWaiting()
	if !raceenabled && !stacklessCoroIsPullComparison(s) && len(s.wake) != 0 {
		return false
	}
	drainStacklessCoroWake(s.wake)
	done := make(chan bool, 1)
	go func() {
		done <- s.waitForWork()
	}()
	for s.waitingExecutors() == 0 && !stacklessCoroIsPullComparison(s) {
		Gosched()
	}
	s.signalIfWaiting()
	if !<-done {
		return false
	}
	return s.waitingExecutors() == 0
}
