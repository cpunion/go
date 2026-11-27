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

// Protected by lock. totalBytes mirrors the aggregate cached payload (global
// slots plus per-P multi-P caches) so cache admissions beyond the budget skip
// the lock. gen invalidates per-P caches on slot ambiguity or test resets.
var stacklessCoroTypedBurst struct {
	lock       mutex
	slots      [stacklessCoroTypedBurstSlots]stacklessCoroTypedBurstSlot
	used       uint8
	totalBytes atomic.Uintptr
	gen        atomic.Uint32
}

func init() {
	lockInit(&stacklessCoroTypedBurst.lock, lockRankStacklessCoroCache)
}

func drainCoroPTypedBurstOnP(pp *p) {
	ps := pp.coroP.Load()
	if ps == nil {
		return
	}
	dropCoroPTypedBurst(ps)
}

func drainStacklessCoroPTypedBurst() {
	systemstack(func() {
		forEachP(waitReasonFlushProcCaches, drainCoroPTypedBurstOnP)
	})
}

func tryReserveStacklessCoroTypedBurstBytes(allocSize uintptr) bool {
	for {
		cur := stacklessCoroTypedBurst.totalBytes.Load()
		if cur+allocSize > stacklessCoroTypedBurstBudget {
			return false
		}
		if stacklessCoroTypedBurst.totalBytes.CompareAndSwap(cur, cur+allocSize) {
			return true
		}
	}
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
	if gomaxprocs > 1 {
		return takeStacklessCoroTypedBurstMultiP(fn, typ, frameSize)
	}
	lock(&stacklessCoroTypedBurst.lock)
	slot := findOrInitStacklessCoroTypedBurstSlotLocked(fn, typ, frameSize)
	if slot == nil || slot.ambiguous || slot.count == 0 {
		if slot != nil && slot.ambiguous {
			stacklessCoroTypedBurst.gen.Add(1)
		}
		unlock(&stacklessCoroTypedBurst.lock)
		return nil
	}
	slot.count--
	task := slot.entries[slot.count]
	slot.entries[slot.count] = nil
	if slot.count < slot.oldEnd {
		slot.oldEnd = slot.count
	}
	stacklessCoroTypedBurst.totalBytes.Add(-slot.allocSize)
	unlock(&stacklessCoroTypedBurst.lock)
	task.state = stacklessCoroTaskNew
	return task
}

//go:noinline
func takeStacklessCoroTypedBurstMultiP(fn *funcval, typ *_type, frameSize uintptr) *stacklessCoroTask {
	gen := stacklessCoroTypedBurst.gen.Load()
	mp := acquirem()
	if pp := mp.p.ptr(); pp != nil {
		if ps := pp.coroP.Load(); ps != nil {
			if ps.typedBurstCount > 0 && ps.typedBurstGen != gen {
				dropCoroPTypedBurst(ps)
			}
			if ps.typedBurstCount > 0 && ps.typedBurstGen == gen &&
				ps.typedBurstResume == fn && ps.typedBurstTyp == typ &&
				ps.typedBurstFrameSize == frameSize {
				ps.typedBurstCount--
				task := ps.typedBurst[ps.typedBurstCount]
				ps.typedBurst[ps.typedBurstCount] = nil
				allocSize := ps.typedBurstAllocSize
				releasem(mp)
				stacklessCoroTypedBurst.totalBytes.Add(-allocSize)
				task.state = stacklessCoroTaskNew
				return task
			}
		}
	}
	releasem(mp)
	lock(&stacklessCoroTypedBurst.lock)
	slot := findOrInitStacklessCoroTypedBurstSlotLocked(fn, typ, frameSize)
	if slot == nil || slot.ambiguous || slot.count == 0 {
		ambiguous := slot != nil && slot.ambiguous
		if ambiguous {
			stacklessCoroTypedBurst.gen.Add(1)
		}
		unlock(&stacklessCoroTypedBurst.lock)
		if ambiguous {
			drainStacklessCoroPTypedBurst()
		}
		return nil
	}
	slot.count--
	task := slot.entries[slot.count]
	slot.entries[slot.count] = nil
	if slot.count < slot.oldEnd {
		slot.oldEnd = slot.count
	}
	allocSize := slot.allocSize
	stacklessCoroTypedBurst.totalBytes.Add(-allocSize)
	var batch [128]*stacklessCoroTask
	batchN := 0
	if slot.count > 0 {
		want := int(slot.count)
		if want > len(batch) {
			want = len(batch)
		}
		for batchN < want {
			slot.count--
			batch[batchN] = slot.entries[slot.count]
			slot.entries[slot.count] = nil
			batchN++
		}
		if slot.count < slot.oldEnd {
			slot.oldEnd = slot.count
		}
	}
	gen = stacklessCoroTypedBurst.gen.Load()
	unlock(&stacklessCoroTypedBurst.lock)
	if batchN > 0 {
		mp := acquirem()
		var ps *stacklessCoroPState
		mp, ps = ensureCoroPState(mp)
		stored := 0
		if ps != nil {
			if ps.typedBurstCount == 0 || ps.typedBurstGen != gen {
				if ps.typedBurstCount > 0 {
					dropCoroPTypedBurst(ps)
				}
				ps.typedBurstGen = gen
				ps.typedBurstResume = fn
				ps.typedBurstTyp = typ
				ps.typedBurstFrameSize = frameSize
				ps.typedBurstAllocSize = allocSize
			}
			if ps.typedBurstGen == gen && ps.typedBurstResume == fn &&
				ps.typedBurstTyp == typ && ps.typedBurstFrameSize == frameSize {
				for stored < batchN && ps.typedBurstCount < stacklessCoroPTypedBurstCap {
					ps.typedBurst[ps.typedBurstCount] = batch[stored]
					ps.typedBurstCount++
					stored++
				}
			}
		}
		releasem(mp)
		if stored < batchN {
			// Return any entries that could not fit on this P.
			lock(&stacklessCoroTypedBurst.lock)
			slot = findOrInitStacklessCoroTypedBurstSlotLocked(fn, typ, frameSize)
			for i := stored; i < batchN; i++ {
				if slot != nil && !slot.ambiguous && slot.count < stacklessCoroTypedBurstCap {
					slot.entries[slot.count] = batch[i]
					slot.count++
				} else {
					stacklessCoroTypedBurst.totalBytes.Add(-allocSize)
				}
			}
			unlock(&stacklessCoroTypedBurst.lock)
		}
	}
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
	if task.state != stacklessCoroTaskComplete || task.resume != nil ||
		task.parentOrSource != nil || task.next != nil ||
		task.context.frame != nil || task.context.scheduler != nil ||
		task.flags != 0 || task.frameSize != 0 ||
		task.terminal != stacklessCoroTerminalNone || task.goexit ||
		task.resuming || task.readyPending {
		throw("runtime: invalid stackless coroutine typed burst admission")
	}
	if gomaxprocs > 1 {
		cacheStacklessCoroTypedBurstMultiP(fn, task)
		return
	}
	lock(&stacklessCoroTypedBurst.lock)
	var slot *stacklessCoroTypedBurstSlot
	for i := uint8(0); i < stacklessCoroTypedBurst.used; i++ {
		if stacklessCoroTypedBurst.slots[i].resume == fn {
			slot = &stacklessCoroTypedBurst.slots[i]
			break
		}
	}
	if slot == nil || slot.ambiguous {
		unlock(&stacklessCoroTypedBurst.lock)
		return
	}
	if slot.count < stacklessCoroTypedBurstCap && tryReserveStacklessCoroTypedBurstBytes(slot.allocSize) {
		memclrHasPointers(add(unsafe.Pointer(task), unsafe.Sizeof(stacklessCoroTask{})), slot.frameSize)
		slot.entries[slot.count] = task
		slot.count++
	}
	unlock(&stacklessCoroTypedBurst.lock)
}

//go:noinline
func cacheStacklessCoroTypedBurstMultiP(fn *funcval, task *stacklessCoroTask) {
	gen := stacklessCoroTypedBurst.gen.Load()
	mp := acquirem()
	if pp := mp.p.ptr(); pp != nil {
		if ps := pp.coroP.Load(); ps != nil && ps.typedBurstGen == gen &&
			ps.typedBurstResume == fn && ps.typedBurstFrameSize != 0 &&
			ps.typedBurstCount < stacklessCoroPTypedBurstCap {
			frameSize := ps.typedBurstFrameSize
			allocSize := ps.typedBurstAllocSize
			if tryReserveStacklessCoroTypedBurstBytes(allocSize) {
				memclrHasPointers(add(unsafe.Pointer(task), unsafe.Sizeof(stacklessCoroTask{})), frameSize)
				ps.typedBurst[ps.typedBurstCount] = task
				ps.typedBurstCount++
				releasem(mp)
				return
			}
			releasem(mp)
			return
		}
	}
	releasem(mp)
	// If this P already has a full per-P buffer for fn, flush half to the global
	// slot in one lock acquisition alongside task.
	var flush [stacklessCoroPTypedBurstCap / 2]*stacklessCoroTask
	flushN := 0
	var flushAllocSize uintptr
	mp = acquirem()
	if pp := mp.p.ptr(); pp != nil {
		if ps := pp.coroP.Load(); ps != nil && ps.typedBurstGen == gen &&
			ps.typedBurstResume == fn && ps.typedBurstCount >= stacklessCoroPTypedBurstCap {
			flushAllocSize = ps.typedBurstAllocSize
			for flushN < len(flush) && ps.typedBurstCount > 0 {
				ps.typedBurstCount--
				flush[flushN] = ps.typedBurst[ps.typedBurstCount]
				ps.typedBurst[ps.typedBurstCount] = nil
				flushN++
			}
		}
	}
	releasem(mp)
	lock(&stacklessCoroTypedBurst.lock)
	var slot *stacklessCoroTypedBurstSlot
	for i := uint8(0); i < stacklessCoroTypedBurst.used; i++ {
		if stacklessCoroTypedBurst.slots[i].resume == fn {
			slot = &stacklessCoroTypedBurst.slots[i]
			break
		}
	}
	if slot == nil || slot.ambiguous {
		unlock(&stacklessCoroTypedBurst.lock)
		if flushN > 0 {
			stacklessCoroTypedBurst.totalBytes.Add(-uintptr(flushN) * flushAllocSize)
		}
		return
	}
	// Transfer already-accounted per-P overflow entries first.
	for i := 0; i < flushN; i++ {
		if slot.count < stacklessCoroTypedBurstCap {
			slot.entries[slot.count] = flush[i]
			slot.count++
		} else {
			stacklessCoroTypedBurst.totalBytes.Add(-flushAllocSize)
		}
	}
	frameSize := slot.frameSize
	allocSize := slot.allocSize
	typ := slot.typ
	if slot.count < stacklessCoroTypedBurstCap && tryReserveStacklessCoroTypedBurstBytes(allocSize) {
		// Clear frame pointers before caching so completed payloads remain
		// immediately collectible even while the combined task+frame waits in cache.
		memclrHasPointers(add(unsafe.Pointer(task), unsafe.Sizeof(stacklessCoroTask{})), frameSize)
		slot.entries[slot.count] = task
		slot.count++
	}
	gen = stacklessCoroTypedBurst.gen.Load()
	unlock(&stacklessCoroTypedBurst.lock)
	// Always seed this P's typedBurst metadata for valid slots so subsequent
	// completions on this P hit the lock-free per-P path even if the global slot
	// was temporarily full.
	if frameSize != 0 {
		mp := acquirem()
		var ps *stacklessCoroPState
		mp, ps = ensureCoroPState(mp)
		if ps != nil && ps.typedBurstCount == 0 {
			ps.typedBurstGen = gen
			ps.typedBurstResume = fn
			ps.typedBurstTyp = typ
			ps.typedBurstFrameSize = frameSize
			ps.typedBurstAllocSize = allocSize
		}
		releasem(mp)
	}
}

func clearStacklessCoroTypedBurstCache() {
	if raceenabled || unsafe.Sizeof(uintptr(0)) != 8 {
		return
	}
	lock(&stacklessCoroTypedBurst.lock)
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
	unlock(&stacklessCoroTypedBurst.lock)
}

func clearStacklessCoroBurstCaches(coalesce bool) {
	clearStacklessCoroPStates()
	clearStacklessCoroPooledGCache(!coalesce)
	if coalesce {
		return
	}
	clearStacklessCoroGoroutineBurstCache()
	clearStacklessCoroTypedBurstCache()
	clearStacklessCoroManagedChannelCache()
}

func resetStacklessCoroBurstCachesForTest() {
	stacklessCoroTypedBurst.gen.Add(1)
	drainStacklessCoroPTypedBurst()
	lock(&stacklessCoroTypedBurst.lock)
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
	unlock(&stacklessCoroTypedBurst.lock)
	resetStacklessCoroGoroutineBurstCacheForTest()
	resetStacklessCoroManagedChannelCacheForTest()
}
