// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

func StacklessCoroSinglePExecutorsForTest(ctx unsafe.Pointer) bool {
	return (*stacklessCoroContext)(ctx).scheduler.executorState.Load() ==
		stacklessCoroExecutorStateSingleP
}

// Exercise a real manager/replacement join after promotion, preserving the
// already-published root notification. The blocking-call integration tests
// additionally exercise promotion from the native foreign-call boundary.
func StacklessCoroSinglePPromotionForTest(mode int) bool {
	s := newStacklessCoroTestScheduler()
	s.root.state = stacklessCoroTaskRunning
	s.root.resuming = true
	s.head, s.tail = nil, nil
	s.runnableState.Store(0)
	s.executorCount.Store(1)
	s.wake = make(chan struct{}, stacklessCoroWarmExecutorCount)
	s.root.context.scheduler = s
	s.lifetime.Store(stacklessCoroLifetimeSpawned)
	s.prepareReplacementExecutors()
	done := s.rootDone
	lazy := !raceenabled && !stacklessCoroIsPullComparison(s)
	valid := done != nil && ((s.executorState.Load() ==
		stacklessCoroExecutorStateSingleP) == lazy)
	if lazy {
		valid = valid && s.executorCount.Load() == 1 &&
			s.executorWake == nil && s.executorDone == nil &&
			s.executorGrow == nil && s.executorStop == nil
	}
	switch mode {
	case 1:
		s.promoteSinglePExecutors()
	case 2:
		previous := GOMAXPROCS(4)
		defer GOMAXPROCS(previous)
		s.prepareReplacementExecutors()
		s.wakeReplacementExecutor()
	}
	if !lazy || mode != 0 {
		valid = valid && s.executorState.Load() ==
			stacklessCoroExecutorStateRunning &&
			s.executorCount.Load() == stacklessCoroWarmExecutorCount &&
			s.executorWake != nil && s.executorDone != nil &&
			s.executorGrow != nil && s.executorStop != nil
	}
	valid = valid && s.rootDone == done
	s.complete(&s.root)
	s.stopReplacementExecutors()
	select {
	case <-done:
	default:
		valid = false
	}
	return valid && s.drained() && s.executorState.Load() !=
		stacklessCoroExecutorStatePreparing
}
