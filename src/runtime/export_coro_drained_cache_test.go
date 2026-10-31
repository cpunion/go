// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime

import (
	"internal/abi"
	"unsafe"
)

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

type drainedBudgetAllocationForTest struct {
	task  stacklessCoroTask
	frame [4]uintptr
}

func drainedBudgetOldResumeForTest(unsafe.Pointer) uint8 { return stacklessCoroActionComplete }
func drainedBudgetNewResumeForTest(unsafe.Pointer) uint8 { return stacklessCoroActionComplete }

func CheckStacklessCoroFrameBudgetReplacementForTest(typed, active bool) bool {
	s := newStacklessCoroTestScheduler()
	s.root.state = stacklessCoroTaskRunning
	s.root.resuming = true
	s.root.context.scheduler = s
	for range stacklessCoroTaskCacheSize {
		task := &stacklessCoroTask{state: stacklessCoroTaskNew, frameSize: 16,
			flags: stacklessCoroTaskCacheFrame, resume: drainedBudgetOldResumeForTest}
		task.context.frame = unsafe.Pointer(new([2]uintptr))
		if !active {
			task.next = s.freeTasks
			s.freeTasks = task
			s.freeFrameBytes += 16
		}
		s.cachedFrameTasks++
		s.cachedFrameBytes += 16
	}
	size := uintptr(24)
	if typed {
		size = 32
	}
	take := func() unsafe.Pointer {
		if typed {
			return coroTakeTypedTaskFrame(unsafe.Pointer(&s.root.context), drainedBudgetNewResumeForTest,
				size, abi.TypeFor[drainedBudgetAllocationForTest]())
		}
		return coroTakeFrame(unsafe.Pointer(&s.root.context), drainedBudgetNewResumeForTest, size)
	}
	first := take()
	if active {
		return s.cachedFrameTasks == stacklessCoroTaskCacheSize && s.cachedFrameBytes == stacklessCoroFrameCacheSize &&
			s.freeFrameBytes == 0 && s.freeTasks == nil && s.reservedTasks != nil &&
			!s.reservedTasks.hasFlag(stacklessCoroTaskCacheFrame)
	}
	if first != nil || s.reservedTasks == nil || !s.reservedTasks.hasFlag(stacklessCoroTaskCacheFrame) ||
		s.cachedFrameBytes > stacklessCoroFrameCacheSize || s.cachedFrameTasks+s.freePlainTaskCount > stacklessCoroTaskCacheSize {
		return false
	}
	var frame unsafe.Pointer
	if typed {
		frame = unsafe.Pointer(new([4]uintptr))
	} else {
		frame = unsafe.Pointer(new([3]uintptr))
	}
	*(*uintptr)(frame) = 0x789abc
	lock(&s.lock)
	task := s.takeReservedFrameTaskLocked(&s.root, frame, drainedBudgetNewResumeForTest)
	if task == nil {
		unlock(&s.lock)
		return false
	}
	task.parent = nil
	task.state = stacklessCoroTaskComplete
	s.recycleTaskLocked(task)
	unlock(&s.lock)
	second := take()
	return second == frame && *(*uintptr)(second) == 0x789abc &&
		s.cachedFrameBytes <= stacklessCoroFrameCacheSize && s.cachedFrameTasks+s.freePlainTaskCount <= stacklessCoroTaskCacheSize
}
