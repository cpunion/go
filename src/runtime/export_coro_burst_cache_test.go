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

type stacklessCoroBurstTestFrame56 struct {
	state uint64
	p1    *uint64
	pad   [5]uint64
}

type stacklessCoroBurstTestAlloc56 struct {
	task  stacklessCoroTask
	frame stacklessCoroBurstTestFrame56
}

type stacklessCoroBurstTestFrame200 struct {
	state uint64
	p1    *uint64
	pad   [23]uint64
}

type stacklessCoroBurstTestAlloc200 struct {
	task  stacklessCoroTask
	frame stacklessCoroBurstTestFrame200
}

type stacklessCoroBurstTestFrame248 struct {
	state uint64
	p1    *uint64
	pad   [29]uint64
}

type stacklessCoroBurstTestAlloc248 struct {
	task  stacklessCoroTask
	frame stacklessCoroBurstTestFrame248
}

func stacklessCoroBurstTestResumeA(unsafe.Pointer) uint8  { return stacklessCoroActionComplete }
func stacklessCoroBurstTestResumeB(unsafe.Pointer) uint8  { return stacklessCoroActionComplete }
func stacklessCoroBurstTestResumeC1(unsafe.Pointer) uint8 { return stacklessCoroActionComplete }
func stacklessCoroBurstTestResumeC2(unsafe.Pointer) uint8 { return stacklessCoroActionComplete }
func stacklessCoroBurstTestResumeC3(unsafe.Pointer) uint8 { return stacklessCoroActionComplete }
func stacklessCoroBurstTestResumeC4(unsafe.Pointer) uint8 { return stacklessCoroActionComplete }

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
	if stacklessCoroTypedBurst.totalBytes.Load() != stacklessCoroTypedBurstCharge(size32) {
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

	// Verify 8B-aligned 56B frame + 48B task (104B raw -> 112B size class) is
	// charged by its actual allocator size class, and multi-slot admissions
	// never exceed stacklessCoroTypedBurstBudget in either accounted or actual
	// span element bytes.
	resetStacklessCoroBurstCachesForTest()
	typ56 := abi.TypeFor[stacklessCoroBurstTestAlloc56]()
	size56 := unsafe.Sizeof(stacklessCoroBurstTestFrame56{})
	typ200 := abi.TypeFor[stacklessCoroBurstTestAlloc200]()
	size200 := unsafe.Sizeof(stacklessCoroBurstTestFrame200{})
	typ248 := abi.TypeFor[stacklessCoroBurstTestAlloc248]()
	size248 := unsafe.Sizeof(stacklessCoroBurstTestFrame248{})

	if takeStacklessCoroTypedBurst(stacklessCoroBurstTestResumeC1, typ56, size56) != nil {
		return "cold 56B typed burst slot returned non-nil"
	}
	alloc56 := (*stacklessCoroBurstTestAlloc56)(newobject(typ56))
	elem56 := spanOf(uintptr(unsafe.Pointer(alloc56))).elemsize
	if elem56 != 112 || stacklessCoroTypedBurstCharge(size56) != elem56 {
		return "56B task+frame typed burst charge does not match 112B size class"
	}
	alloc56.task.state = stacklessCoroTaskComplete
	cacheStacklessCoroTypedBurst(stacklessCoroBurstTestResumeC1, &alloc56.task)
	if stacklessCoroTypedBurst.totalBytes.Load() != elem56 {
		return "56B task+frame totalBytes did not charge actual size class"
	}

	// Register the remaining 3 slots with larger size classes so 4 slots can
	// push aggregate retained bytes right up to the 512 KiB budget ceiling.
	_ = takeStacklessCoroTypedBurst(stacklessCoroBurstTestResumeC2, typ200, size200)
	_ = takeStacklessCoroTypedBurst(stacklessCoroBurstTestResumeC3, typ248, size248)
	_ = takeStacklessCoroTypedBurst(stacklessCoroBurstTestResumeC4, typ248, size248)

	type budgetSpec struct {
		resume stacklessCoroResume
		typ    *_type
		size   uintptr
	}
	specs := []budgetSpec{
		{stacklessCoroBurstTestResumeC1, typ56, size56},
		{stacklessCoroBurstTestResumeC2, typ200, size200},
		{stacklessCoroBurstTestResumeC3, typ248, size248},
		{stacklessCoroBurstTestResumeC4, typ248, size248},
	}
	maxCharge := stacklessCoroTypedBurstCharge(size248)
	// Pre-allocate combined task+frame objects before filling the cache so a
	// background GC (e.g. GOGC=1) cannot age out entries mid-fill.
	const rounds = 600
	prealloc := make([][4]*stacklessCoroTask, rounds)
	for round := range rounds {
		for si, spec := range specs {
			task := (*stacklessCoroTask)(newobject(spec.typ))
			task.state = stacklessCoroTaskComplete
			prealloc[round][si] = task
		}
	}
	rejected := false
	for round := range rounds {
		for si, spec := range specs {
			before := stacklessCoroTypedBurst.totalBytes.Load()
			cacheStacklessCoroTypedBurst(spec.resume, prealloc[round][si])
			after := stacklessCoroTypedBurst.totalBytes.Load()
			if after > stacklessCoroTypedBurstBudget {
				return "typed burst totalBytes exceeded budget"
			}
			if after == before {
				rejected = true
			}
		}
		if rejected && stacklessCoroTypedBurstBudget-stacklessCoroTypedBurst.totalBytes.Load() < maxCharge {
			break
		}
	}
	if !rejected {
		return "multi-slot typed burst admissions never hit budget cap"
	}
	if stacklessCoroTypedBurstBudget-stacklessCoroTypedBurst.totalBytes.Load() >= maxCharge {
		return "multi-slot typed burst cache stopped well below budget ceiling"
	}
	world := stopTheWorld(stwForTestResetDebugLog)
	var actualSpanBytes uintptr
	var activeSlots int
	var mismatch string
	for i := uint8(0); i < stacklessCoroTypedBurst.used; i++ {
		slot := &stacklessCoroTypedBurst.slots[i]
		if slot.count > 0 {
			activeSlots++
		}
		for j := uint16(0); j < slot.count; j++ {
			sp := spanOf(uintptr(unsafe.Pointer(slot.entries[j])))
			if sp == nil || sp.elemsize != slot.allocSize {
				mismatch = "cached typed burst entry span elemsize mismatch"
				break
			}
			actualSpanBytes += sp.elemsize
		}
		if mismatch != "" {
			break
		}
	}
	if mismatch == "" {
		for _, pp := range allp {
			if pp == nil {
				continue
			}
			ps := pp.coroP.Load()
			if ps == nil || ps.typedBurstCount == 0 {
				continue
			}
			for j := uint16(0); j < ps.typedBurstCount; j++ {
				sp := spanOf(uintptr(unsafe.Pointer(ps.typedBurst[j])))
				if sp == nil || sp.elemsize != ps.typedBurstAllocSize {
					mismatch = "per-P cached typed burst entry span elemsize mismatch"
					break
				}
				actualSpanBytes += sp.elemsize
			}
			if mismatch != "" {
				break
			}
		}
	}
	totalAccounted := stacklessCoroTypedBurst.totalBytes.Load()
	startTheWorld(world)
	if mismatch != "" {
		return mismatch
	}
	if activeSlots != stacklessCoroTypedBurstSlots {
		return "multi-slot budget test did not populate all typed burst slots"
	}
	if actualSpanBytes != totalAccounted || actualSpanBytes > stacklessCoroTypedBurstBudget {
		return "actual retained span bytes mismatch or exceed typed burst budget"
	}
	return ""
}
