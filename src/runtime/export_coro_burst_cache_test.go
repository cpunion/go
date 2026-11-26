// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
	"unsafe"
)

type stacklessCoroBurstTestFrame32 struct {
	state uint64
	p1    *uint64
	p2    *uint64
	pad   uint64
}

type stacklessCoroBurstTestAlloc32 struct {
	task  stacklessCoroTask
	frame stacklessCoroBurstTestFrame32
}

type stacklessCoroBurstTestFrame48 struct {
	state uint64
	p1    *uint64
	pad   [4]uint64
}

type stacklessCoroBurstTestAlloc48 struct {
	task  stacklessCoroTask
	frame stacklessCoroBurstTestFrame48
}

func stacklessCoroBurstTestResumeA(unsafe.Pointer) uint8 { return stacklessCoroActionComplete }
func stacklessCoroBurstTestResumeB(unsafe.Pointer) uint8 { return stacklessCoroActionComplete }

func CheckStacklessCoroGoroutineBurstCacheForTest() string {
	resetStacklessCoroBurstCachesForTest()
	defer resetStacklessCoroBurstCachesForTest()

	const n = stacklessCoroTaskCacheSize + 400
	tasks := make([]stacklessCoroTask, n)
	recs := make([]*stacklessCoroGoroutine, n)
	for i := range n {
		recs[i] = &stacklessCoroGoroutine{root: &tasks[i], leaf: &tasks[i]}
	}
	lock(&stacklessCoroGoroutines.lock)
	for i := range n {
		stacklessCoroInsertGoroutine(recs[i])
		stacklessCoroGoroutines.unhosted.Add(1)
	}
	unlock(&stacklessCoroGoroutines.lock)
	for i := range n {
		stacklessCoroCompleteLogicalFrame(&tasks[i])
	}
	lock(&stacklessCoroGoroutines.lock)
	burstBefore := stacklessCoroGoroutines.taskBurstCount[0] + stacklessCoroGoroutines.taskBurstCount[1]
	unlock(&stacklessCoroGoroutines.lock)
	if burstBefore == 0 || burstBefore > stacklessCoroGoroutineBurstCacheCap {
		return "goroutine burst cache count out of expected range after N>256 drain"
	}

	// Two idle GC cycles must expire the burst cache back to zero.
	GC()
	GC()
	lock(&stacklessCoroGoroutines.lock)
	burstAfter := stacklessCoroGoroutines.taskBurstCount[0] + stacklessCoroGoroutines.taskBurstCount[1]
	unlock(&stacklessCoroGoroutines.lock)
	if burstAfter != 0 {
		return "goroutine burst cache did not expire to zero after two idle GCs"
	}
	return ""
}

func CheckStacklessCoroTypedBurstCacheForTest() string {
	if raceenabled || unsafe.Sizeof(uintptr(0)) != 8 {
		return ""
	}
	resetStacklessCoroBurstCachesForTest()
	defer resetStacklessCoroBurstCachesForTest()

	typ32 := abi.TypeFor[stacklessCoroBurstTestAlloc32]()
	size32 := unsafe.Sizeof(stacklessCoroBurstTestFrame32{})
	if takeStacklessCoroTypedBurst(stacklessCoroBurstTestResumeA, typ32, size32) != nil {
		return "cold typed burst cache returned non-nil task"
	}

	alloc := (*stacklessCoroBurstTestAlloc32)(newobject(typ32))
	sentinel := new(uint64)
	alloc.frame.p1 = sentinel
	alloc.frame.p2 = sentinel
	alloc.task.state = stacklessCoroTaskComplete

	cacheStacklessCoroTypedBurst(stacklessCoroBurstTestResumeA, &alloc.task)
	if alloc.task.state != stacklessCoroTaskComplete || alloc.task.next != nil || alloc.task.frameSize != 0 {
		return "cached typed burst task mutated idle completion invariants"
	}
	if alloc.frame.p1 != nil || alloc.frame.p2 != nil {
		return "cached typed burst task did not clear frame pointers"
	}
	if stacklessCoroTypedBurst.totalBytes.Load() != unsafe.Sizeof(stacklessCoroTask{})+size32 {
		return "typed burst totalBytes mismatch after cache admission"
	}

	got := takeStacklessCoroTypedBurst(stacklessCoroBurstTestResumeA, typ32, size32)
	if got != &alloc.task || got.state != stacklessCoroTaskNew {
		return "typed burst reuse did not return task in stacklessCoroTaskNew state"
	}
	if stacklessCoroTypedBurst.totalBytes.Load() != 0 {
		return "typed burst totalBytes non-zero after popping sole entry"
	}

	// Re-cache and verify two idle GC cycles expire it to zero.
	got.state = stacklessCoroTaskComplete
	cacheStacklessCoroTypedBurst(stacklessCoroBurstTestResumeA, got)
	GC()
	GC()
	if stacklessCoroTypedBurst.totalBytes.Load() != 0 {
		return "typed burst cache did not expire to zero after two idle GCs"
	}

	// Verify ambiguous resume function (two different types for one resume fn)
	// drops cached entries and disables caching for that resume function.
	typ48 := abi.TypeFor[stacklessCoroBurstTestAlloc48]()
	size48 := unsafe.Sizeof(stacklessCoroBurstTestFrame48{})
	_ = takeStacklessCoroTypedBurst(stacklessCoroBurstTestResumeB, typ32, size32)
	allocB := (*stacklessCoroBurstTestAlloc32)(newobject(typ32))
	allocB.task.state = stacklessCoroTaskComplete
	cacheStacklessCoroTypedBurst(stacklessCoroBurstTestResumeB, &allocB.task)
	if takeStacklessCoroTypedBurst(stacklessCoroBurstTestResumeB, typ48, size48) != nil {
		return "ambiguous resume lookup returned mismatched type from cache"
	}
	if stacklessCoroTypedBurst.totalBytes.Load() != 0 {
		return "ambiguous resume did not drop previously cached entries"
	}
	allocB2 := (*stacklessCoroBurstTestAlloc32)(newobject(typ32))
	allocB2.task.state = stacklessCoroTaskComplete
	cacheStacklessCoroTypedBurst(stacklessCoroBurstTestResumeB, &allocB2.task)
	if stacklessCoroTypedBurst.totalBytes.Load() != 0 {
		return "ambiguous resume still admitted new cache entries"
	}
	return ""
}
