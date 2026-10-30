// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime

import "unsafe"

//go:noinline
func StacklessCoroDrainedCacheForTest(drained bool, delayGC int) (unsafe.Pointer, unsafe.Pointer, bool) {
	s := newStacklessCoroTestScheduler()
	s.wake = make(chan struct{}, stacklessCoroWarmExecutorCount)
	wake := s.wake
	lifetime := stacklessCoroLifetimeRootComplete | stacklessCoroLifetimeSpawned |
		stacklessCoroLifetimeOperationStarted
	if drained {
		s.drainCacheAge.complete()
		lifetime |= stacklessCoroLifetimeDrained
	}
	s.lifetime.Store(lifetime)
	task := &stacklessCoroTask{state: stacklessCoroTaskNew, frameSize: 8,
		flags: stacklessCoroTaskCacheFrame, resume: drainedCacheResumeForTest}
	task.context.frame = unsafe.Pointer(new(uint64))
	s.freeTasks = task
	s.freeFrameBytes, s.cachedFrameBytes, s.cachedFrameTasks = 8, 8, 1
	op := newStacklessCoroChannelOperation()
	s.freeOperations, s.freeOperationCount = op, 1
	for range delayGC {
		GC()
	}
	cacheStacklessCoroDrainedTasks(s)
	valid := s.wake == wake && s.lifetime.Load() == lifetime
	if !drained {
		valid = valid && s.freeTasks == task && s.freeOperations == op &&
			s.freeOperationCount == 1 && s.freeFrameBytes == 8 && s.cachedFrameTasks == 1
	} else {
		valid = valid && s.freeTasks == nil && s.freeOperations == op &&
			s.freeOperationCount == 1 && s.freePlainTaskCount == 0 &&
			s.freeFrameBytes == 0 && s.cachedFrameBytes == 0 && s.cachedFrameTasks == 0
		if delayGC < 2 {
			ctx := acquireStacklessCoroNativeContext()
			valid = valid && ctx.freeTasks == task && ctx.cachedFrameTasks == 1 &&
				ctx.freeFrameBytes == 8 && ctx.cachedFrameBytes == 8 &&
				task.context.frame != nil && task.resume != nil && task.next == nil
			// Observation must not reset the generation it is checking.
			releaseStacklessCoroNativeContextAt(ctx, s.drainCacheAge.cycle)
		}
	}
	// A producer that retained the old scheduler may still signal its original
	// wake. It must neither touch the detached cache nor see a reused scheduler.
	s.signal()
	valid = valid && len(wake) == 1
	return unsafe.Pointer(s), unsafe.Pointer(task), valid
}

func drainedCacheResumeForTest(unsafe.Pointer) uint8 { return stacklessCoroActionComplete }

func StacklessCoroDrainedCacheSignalForTest(token unsafe.Pointer) bool {
	s := (*stacklessCoroScheduler)(token)
	s.signal()
	return s.freeTasks == nil && s.freeOperationCount == 1 && s.freeOperations != nil && s.wake != nil
}

func CheckStacklessCoroDrainedOperationsForTest() bool {
	s := newStacklessCoroTestScheduler()
	s.wake = make(chan struct{}, stacklessCoroWarmExecutorCount)
	s.lifetime.Store(stacklessCoroLifetimeSpawned | stacklessCoroLifetimeOperationStarted)
	s.liveSpawns = 1
	for range stacklessCoroOperationCacheSize {
		op := newStacklessCoroChannelOperation()
		op.next = s.freeOperations
		s.freeOperations = op
		s.freeOperationCount++
	}
	lock(&s.lock)
	s.completeLogicalTaskLocked(&s.root)
	if s.drained() || s.freeOperationCount != stacklessCoroOperationCacheSize || s.freeOperations == nil {
		unlock(&s.lock)
		return false
	}
	s.completeLogicalTaskLocked(new(stacklessCoroTask))
	valid := s.drained() && s.freeOperations == nil && s.freeOperationCount == 0 && s.liveSpawns == 0
	unlock(&s.lock)
	// No delayed native cache return may reinsert operations after an idle GC.
	GC()
	before := stacklessCoroChannelCache.generations[stacklessCoroChannelCache.young].count
	cacheStacklessCoroDrainedTasks(s)
	valid = valid && before == stacklessCoroChannelCache.generations[stacklessCoroChannelCache.young].count
	s.signal()
	return valid && len(s.wake) == 1
}
