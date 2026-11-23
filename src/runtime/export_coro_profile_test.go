// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && (amd64 || (arm64 && corolinuxarm64native))))

package runtime

import (
	"internal/profilerecord"
	"internal/runtime/atomic"
	"unsafe"
)

func StacklessCoroPendingRetirementForTest() bool {
	s := new(stacklessCoroScheduler)
	task := &stacklessCoroTask{state: stacklessCoroTaskRunning, resuming: true}
	record := &stacklessCoroGoroutine{root: task, leaf: task, scheduler: s}
	task.setSource(record)
	pending := new(atomic.Pointer[stacklessCoroTask])
	first := new(stacklessCoroTask)
	pending.Store(first)

	// Keep the synthetic source private to this test. A nonempty pending
	// list models a drainer that has yet to obtain the registry lock.
	lock(&stacklessCoroGoroutines.lock)
	bucket := stacklessCoroGoroutineBucket(task)
	record.next, *bucket = *bucket, record
	stacklessCoroGoroutines.records.Add(1)
	stacklessCoroGoroutines.unhosted.Add(1)
	s.completeIndependent(task, pending)
	retired := record.retired.Load() && pending.Load() == task && task.next == first
	removed, unhosted := stacklessCoroCompleteLogicalFrameLocked(task)
	stacklessCoroFinishLogicalRetirementLocked(removed, unhosted)
	unlock(&stacklessCoroGoroutines.lock)
	return retired
}

type StacklessCoroNativeProfileTransitionResult struct {
	Native, Reused bool
	State          uint32
	Records        int64
	PCs            int
}

type stacklessCoroProfileTestState struct {
	caller, executor *g
	enter, warm      bool
	result           StacklessCoroNativeProfileTransitionResult
	records          []profilerecord.StackRecord
}

func stacklessCoroProfileTestActivate(state *stacklessCoroProfileTestState, target *g) {
	stw := stopTheWorld(stwGoroutineProfile)
	// Other Gs have already contributed. Leave exactly the direct-handoff
	// target pending, including a dead executor that is about to be reused.
	forEachGRace(func(gp *g) { gp.goroutineProfiled.Store(goroutineProfileSatisfied) })
	target.goroutineProfiled.Store(goroutineProfileAbsent)
	goroutineProfile.offset.Store(0)
	goroutineProfile.records = state.records
	goroutineProfile.labels = nil
	goroutineProfile.active = true
	startTheWorld(stw)
}

func stacklessCoroProfileTestResume(ctx unsafe.Pointer) uint8 {
	state := (*stacklessCoroProfileTestState)((*stacklessCoroContext)(ctx).frame)
	native := stacklessCoroNativeContextFor(getg()) != nil
	if state.warm {
		state.executor = getg()
		state.result.Native = native
	} else {
		state.result.Native = state.result.Native && native
		state.result.Reused = getg() == state.executor
		if state.enter {
			state.result.State = uint32(getg().goroutineProfiled.Load())
		} else {
			stacklessCoroProfileTestActivate(state, state.caller)
		}
	}
	return stacklessCoroActionComplete
}

// Exercise the profiler's pending-record protocol at the direct native stack
// handoff. No collecting goroutine races to satisfy the target first: the
// transition itself must finish its record before executing Go code again.
func StacklessCoroNativeProfileTransitionForTest(enter bool) StacklessCoroNativeProfileTransitionResult {
	semacquire(&goroutineProfile.sema)
	state := new(stacklessCoroProfileTestState)
	state.caller, state.enter, state.warm = getg(), enter, true
	state.records = make([]profilerecord.StackRecord, 1)
	coroRunFrame(unsafe.Pointer(state), stacklessCoroProfileTestResume, 0)
	state.warm = false
	if enter {
		stacklessCoroProfileTestActivate(state, state.executor)
	}
	coroRunFrame(unsafe.Pointer(state), stacklessCoroProfileTestResume, 0)
	if !enter {
		state.result.State = uint32(state.caller.goroutineProfiled.Load())
	}
	stw := stopTheWorld(stwGoroutineProfileCleanup)
	state.result.Records = goroutineProfile.offset.Swap(0)
	goroutineProfile.active = false
	goroutineProfile.records = nil
	goroutineProfile.labels = nil
	forEachGRace(func(gp *g) { gp.goroutineProfiled.Store(goroutineProfileAbsent) })
	startTheWorld(stw)
	semrelease(&goroutineProfile.sema)
	state.result.PCs = len(state.records[0].Stack)
	return state.result
}
