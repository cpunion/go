// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/runtime/atomic"
	"internal/runtime/gc"
	"unsafe"
)

const (
	stacklessCoroTypedBurstSlots  = 4
	stacklessCoroTypedBurstCap    = 1024
	stacklessCoroTypedBurstMax    = 256
	stacklessCoroTypedBurstBudget = 512 << 10
)

// Each slot retains completed, fully cleared combined task+frame allocations
// for one coroutine resume function with a single consistent allocation type.
// Storage is non-intrusive so completed tasks keep task.state == Complete,
// task.next == nil, and task.frameSize == 0 while idle in the cache.
// entries[0:oldEnd] holds the aging generation; entries[oldEnd:count] holds
// the young generation.
type stacklessCoroTypedBurstSlot struct {
	resume    *funcval
	typ       *_type
	frameSize uintptr
	allocSize uintptr
	ambiguous bool
	count     uint16
	oldEnd    uint16
	entries   [stacklessCoroTypedBurstCap]*stacklessCoroTask
}

// Protected by stacklessCoroSharedOperations.lock. totalBytes mirrors the
// aggregate cached payload so cache admissions beyond the budget skip the lock.
var stacklessCoroTypedBurst struct {
	slots      [stacklessCoroTypedBurstSlots]stacklessCoroTypedBurstSlot
	used       uint8
	totalBytes atomic.Uintptr
}

func takeStacklessCoroTypedBurst(child stacklessCoroResume, typ *_type, frameSize uintptr) *stacklessCoroTask {
	if raceenabled || unsafe.Sizeof(uintptr(0)) != 8 ||
		frameSize == 0 || frameSize > stacklessCoroTypedBurstMax {
		return nil
	}
	fn := stacklessCoroResumeIdentity(child)
	if fn == nil {
		return nil
	}
	lock(&stacklessCoroSharedOperations.lock)
	slot := findOrInitStacklessCoroTypedBurstSlotLocked(fn, typ, frameSize)
	if slot == nil || slot.ambiguous || slot.count == 0 {
		unlock(&stacklessCoroSharedOperations.lock)
		return nil
	}
	slot.count--
	task := slot.entries[slot.count]
	slot.entries[slot.count] = nil
	if slot.count < slot.oldEnd {
		slot.oldEnd = slot.count
	}
	stacklessCoroTypedBurst.totalBytes.Add(-slot.allocSize)
	unlock(&stacklessCoroSharedOperations.lock)
	task.state = stacklessCoroTaskNew
	return task
}

func stacklessCoroTypedBurstCharge(frameSize uintptr) uintptr {
	size := unsafe.Sizeof(stacklessCoroTask{}) + frameSize
	charge := roundupsize(size, false)
	if size > gc.MinSizeForMallocHeader && size <= maxSmallSize-gc.MallocHeaderSize {
		charge += gc.MallocHeaderSize
	}
	return charge
}

func findOrInitStacklessCoroTypedBurstSlotLocked(fn *funcval, typ *_type, frameSize uintptr) *stacklessCoroTypedBurstSlot {
	for i := uint8(0); i < stacklessCoroTypedBurst.used; i++ {
		slot := &stacklessCoroTypedBurst.slots[i]
		if slot.resume == fn {
			if slot.typ != typ || slot.frameSize != frameSize {
				// Multiple allocation types share one resume function (e.g.
				// runtime tests). Drop any cached entries and disable caching
				// for this resume function to preserve exact GC type safety.
				slot.ambiguous = true
				dropStacklessCoroTypedBurstSlotLocked(slot)
			}
			return slot
		}
	}
	if stacklessCoroTypedBurst.used < stacklessCoroTypedBurstSlots {
		slot := &stacklessCoroTypedBurst.slots[stacklessCoroTypedBurst.used]
		stacklessCoroTypedBurst.used++
		slot.resume = fn
		slot.typ = typ
		slot.frameSize = frameSize
		slot.allocSize = stacklessCoroTypedBurstCharge(frameSize)
		return slot
	}
	// Replace an empty non-ambiguous slot so long-lived processes adapt to new
	// hot coroutine worker types after idle GC clears old ones.
	for i := uint8(0); i < stacklessCoroTypedBurst.used; i++ {
		slot := &stacklessCoroTypedBurst.slots[i]
		if !slot.ambiguous && slot.count == 0 {
			slot.resume = fn
			slot.typ = typ
			slot.frameSize = frameSize
			slot.allocSize = stacklessCoroTypedBurstCharge(frameSize)
			slot.oldEnd = 0
			return slot
		}
	}
	return nil
}

func dropStacklessCoroTypedBurstSlotLocked(slot *stacklessCoroTypedBurstSlot) {
	if slot.count == 0 {
		return
	}
	for i := uint16(0); i < slot.count; i++ {
		slot.entries[i] = nil
	}
	stacklessCoroTypedBurst.totalBytes.Add(-uintptr(slot.count) * slot.allocSize)
	slot.count = 0
	slot.oldEnd = 0
}

func cacheStacklessCoroTypedBurst(child stacklessCoroResume, task *stacklessCoroTask) {
	if raceenabled || unsafe.Sizeof(uintptr(0)) != 8 {
		return
	}
	if stacklessCoroTypedBurst.totalBytes.Load() >= stacklessCoroTypedBurstBudget {
		return
	}
	fn := stacklessCoroResumeIdentity(child)
	if fn == nil {
		return
	}
	lock(&stacklessCoroSharedOperations.lock)
	var slot *stacklessCoroTypedBurstSlot
	for i := uint8(0); i < stacklessCoroTypedBurst.used; i++ {
		if stacklessCoroTypedBurst.slots[i].resume == fn {
			slot = &stacklessCoroTypedBurst.slots[i]
			break
		}
	}
	if slot == nil || slot.ambiguous || slot.count >= stacklessCoroTypedBurstCap ||
		stacklessCoroTypedBurst.totalBytes.Load()+slot.allocSize > stacklessCoroTypedBurstBudget {
		unlock(&stacklessCoroSharedOperations.lock)
		return
	}
	if task.state != stacklessCoroTaskComplete || task.resume != nil ||
		task.parentOrSource != nil || task.next != nil ||
		task.context.frame != nil || task.context.scheduler != nil ||
		task.flags != 0 || task.frameSize != 0 ||
		task.terminal != stacklessCoroTerminalNone || task.goexit ||
		task.resuming || task.readyPending {
		unlock(&stacklessCoroSharedOperations.lock)
		throw("runtime: invalid stackless coroutine typed burst admission")
	}
	// Clear frame pointers before caching so completed payloads remain
	// immediately collectible even while the combined task+frame waits in cache.
	memclrHasPointers(add(unsafe.Pointer(task), unsafe.Sizeof(stacklessCoroTask{})), slot.frameSize)
	slot.entries[slot.count] = task
	slot.count++
	stacklessCoroTypedBurst.totalBytes.Add(slot.allocSize)
	unlock(&stacklessCoroSharedOperations.lock)
}

func clearStacklessCoroTypedBurstCache() {
	if raceenabled || unsafe.Sizeof(uintptr(0)) != 8 {
		return
	}
	lock(&stacklessCoroSharedOperations.lock)
	for i := uint8(0); i < stacklessCoroTypedBurst.used; i++ {
		slot := &stacklessCoroTypedBurst.slots[i]
		if slot.count == 0 {
			continue
		}
		if slot.oldEnd > 0 {
			dropped := uintptr(slot.oldEnd) * slot.allocSize
			kept := slot.count - slot.oldEnd
			for j := uint16(0); j < kept; j++ {
				slot.entries[j] = slot.entries[slot.oldEnd+j]
			}
			for j := kept; j < slot.count; j++ {
				slot.entries[j] = nil
			}
			slot.count = kept
			stacklessCoroTypedBurst.totalBytes.Add(-dropped)
		}
		slot.oldEnd = slot.count
	}
	unlock(&stacklessCoroSharedOperations.lock)
}

func clearStacklessCoroBurstCaches(coalesce bool) {
	if coalesce {
		return
	}
	clearStacklessCoroGoroutineBurstCache()
	clearStacklessCoroTypedBurstCache()
	clearStacklessCoroManagedChannelCache()
}

func resetStacklessCoroBurstCachesForTest() {
	lock(&stacklessCoroSharedOperations.lock)
	for i := uint8(0); i < stacklessCoroTypedBurst.used; i++ {
		slot := &stacklessCoroTypedBurst.slots[i]
		dropStacklessCoroTypedBurstSlotLocked(slot)
		slot.resume = nil
		slot.typ = nil
		slot.frameSize = 0
		slot.allocSize = 0
		slot.ambiguous = false
	}
	stacklessCoroTypedBurst.used = 0
	unlock(&stacklessCoroSharedOperations.lock)
	resetStacklessCoroGoroutineBurstCacheForTest()
	resetStacklessCoroManagedChannelCacheForTest()
}
