// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
	"internal/cpu"
	"internal/runtime/atomic"
	"internal/runtime/gc"
	"unsafe"
)

// Coroutine resume action codes returned by compiler-generated .coro.func
// state machines.
const (
	coroActionComplete uint8 = 0
	coroActionYield    uint8 = 1
	coroActionWait     uint8 = 2
	coroActionPanic    uint8 = 3
	coroActionGoexit   uint8 = 4
	coroActionSwitch   uint8 = 5 // structured await transferred to child in task.next
)

// Packed task lifecycle states stored in coroTask.state.
const (
	coroStateNew      uint32 = 0
	coroStateRunnable uint32 = 1
	coroStateRunning  uint32 = 2
	coroStateWaiting  uint32 = 3
	coroStateParked   uint32 = 4
	coroStateDone     uint32 = 5

	coroStateMask        uint32 = 0xff
	coroFlagReadyPending uint32 = 1 << 8
)

// Non-atomic task flags (owned by active driver, or immutable after creation).
const (
	coroFlagRoot          uint32 = 1 << iota // synchronous root driven by coroRunFrame
	coroFlagUnhosted                         // independent go-spawned logical goroutine
	coroFlagPooled                           // allocated via coroAlloc; eligible for per-P reuse
	coroFlagDeferChild                       // structured child running a deferred coroutine
	coroFlagSelectPending                    // waking from multi-case select; needs cleanup
	coroFlagIOPending                        // waking from direct netpoll I/O; needs syscall completion
	coroFlagChanSend                         // parked in channel send (for closed-chan panic)
	coroFlagChanOK                           // channel receive / select ok flag
	coroFlagUnhostedChild                    // structured await child of an unhosted coroutine
	coroFlagTracked                          // currently registered in coroParkedShards
)

type coroSudog struct {
	coro *coroTask // stackless coroutine waiter (nil for ordinary G)
}

func (s *coroSudog) coroTask() *coroTask { return s.coro }

// coroTask is embedded at offset 0 of every compiler-generated coroutine frame
// struct: type F_frame struct { _ [7]unsafe.Pointer; _ [6]uint32; ... }.
// Keeping all 7 pointer fields first gives a uniform 0x7f leading GC bitmap on
// both 32-bit and 64-bit targets with zero trailing struct padding (80 B on
// 64-bit, 52 B on 32-bit).
type coroTask struct {
	resume     func(unsafe.Pointer) uint8
	next       *coroTask
	parent     *coroTask
	typ        *_type
	defers     *coroDeferNode
	panicBox   *coroPanicState
	ioState    *coroIOState
	selectDone atomic.Uint32
	state      atomic.Uint32
	rootSlot   atomic.Uint32
	result     atomic.Uint32
	flags      uint32
	semaTicket atomic.Uint32
}

type coroDeferNode struct {
	next   *coroDeferNode
	fn     func()
	frame  unsafe.Pointer
	resume func(unsafe.Pointer) uint8
}

type coroPanicState struct {
	val      any
	tracePCs []uintptr
	active   bool
	goexit   bool
}

const (
	coroPRunqSize        = 512
	coroPStealBatch      = 128
	coroPCacheSlots      = 4
	coroPCacheSlotCap    = 1024
	coroPCacheSpillAt    = 128
	coroPCacheSpillCnt   = 64
	coroGCacheSlots      = 4
	coroCacheBudget      = 512 << 10
	coroCacheCreditChunk = 32 << 10
	coroCacheCreditHigh  = 64 << 10
	coroUnhostedShards   = 64
	coroMaxWorkers       = 128
	coroMaxRootSlots     = 128
	coroTotalDrivers     = coroMaxWorkers + coroMaxRootSlots
)

type coroTypeCacheSlot struct {
	typ        *_type
	allocSize  uintptr
	young      *coroTask
	old        *coroTask
	youngCount uint16
	oldCount   uint16
}

// coroPState is lazily allocated on p.coro on first coroutine use on that P.
type coroPState struct {
	runnext atomic.Pointer[coroTask]
	hasWork atomic.Uint32
	runqH   uint32
	runqT   uint32
	runq    [coroPRunqSize]*coroTask

	cache         [coroPCacheSlots]coroTypeCacheSlot
	cacheCredit   uintptr // pre-reserved budget bytes owned by this P (zero global atomics on hot path)
	cacheNext     uint8
	runnextStreak uint8
}

var coroUnhostedShardsArr [coroUnhostedShards]struct {
	v atomic.Int64
	_ cpu.CacheLinePad
}

var coroSched struct {
	lock             mutex
	runqHead         *coroTask
	runqTail         *coroTask
	runqLen          atomic.Int32
	_                cpu.CacheLinePad
	cacheLock        mutex
	globalCache      [coroGCacheSlots]coroTypeCacheSlot
	globalCacheCount atomic.Int32
	_                cpu.CacheLinePad
	nonEmptyP        atomic.Int32
	_                cpu.CacheLinePad
	cacheBytes       atomic.Uintptr
	_                cpu.CacheLinePad
	activeDriver     atomic.Int32
	workerCount      atomic.Int32
	idleCount        atomic.Int32
	_                cpu.CacheLinePad
	idleDrivers      [coroTotalDrivers]atomic.Uintptr // guintptr of parked driver Gs
	rootSlotUsed     [coroMaxRootSlots]atomic.Uint32  // 0=free, 1=claimed by parked root caller
}

func init() {
	lockInit(&coroSched.lock, lockRankCoroSched)
	lockInit(&coroSched.cacheLock, lockRankCoroSched)
	// Verify coroTask layout matches compiler's [7]unsafe.Pointer + [6]uint32 header.
	wantSize := 7*unsafe.Sizeof(uintptr(0)) + 24
	if unsafe.Sizeof(coroTask{}) != wantSize {
		throw("runtime: unexpected coroTask size")
	}
	typ := abi.TypeFor[coroTask]()
	if typ.PtrBytes != 7*unsafe.Sizeof(uintptr(0)) || typ.GCData == nil || *typ.GCData != 0x7f {
		throw("runtime: unexpected coroTask GC bitmap")
	}
}

func coroAddUnhosted(delta int64) {
	mp := acquirem()
	idx := 0
	if pp := mp.p.ptr(); pp != nil {
		idx = int(pp.id) & (coroUnhostedShards - 1)
	}
	coroUnhostedShardsArr[idx].v.Add(delta)
	releasem(mp)
}

func coroUnhostedCount() uint64 {
	var total int64
	for i := range coroUnhostedShardsArr {
		total += coroUnhostedShardsArr[i].v.Load()
	}
	if total < 0 {
		return 0
	}
	return uint64(total)
}

func coroTryWinSelect(t *coroTask) bool {
	return t.selectDone.CompareAndSwap(0, 1)
}

func coroEnsurePState(mp *m) (*m, *coroPState) {
	pp := mp.p.ptr()
	if pp == nil {
		return mp, nil
	}
	if ps := pp.coro.Load(); ps != nil {
		return mp, ps
	}
	releasem(mp)
	fresh := new(coroPState)
	mp = acquirem()
	pp = mp.p.ptr()
	if pp == nil {
		return mp, nil
	}
	if !pp.coro.CompareAndSwap(nil, fresh) {
		return mp, pp.coro.Load()
	}
	return mp, fresh
}

func coroTypeAllocSize(typ *_type) uintptr {
	size := typ.Size_
	charge := roundupsize(size, typ.PtrBytes == 0)
	if typ.PtrBytes != 0 && size > gc.MinSizeForMallocHeader && size <= maxSmallSize-gc.MallocHeaderSize {
		charge += gc.MallocHeaderSize
	}
	return charge
}

func coroIOAllocSize() uintptr {
	return coroTypeAllocSize(abi.TypeFor[coroIOState]())
}

func coroTryReserveBudget(bytes uintptr) bool {
	for {
		cur := coroSched.cacheBytes.Load()
		if cur+bytes > coroCacheBudget {
			return false
		}
		if coroSched.cacheBytes.CompareAndSwap(cur, cur+bytes) {
			return true
		}
	}
}

func coroEvictChain(head *coroTask, count uint16, allocSize uintptr) {
	if count == 0 && head == nil {
		return
	}
	ioSize := coroIOAllocSize()
	var refund uintptr
	for cur := head; cur != nil; {
		nxt := cur.next
		cur.next = nil
		refund += allocSize
		if cur.ioState != nil {
			refund += ioSize
			cur.ioState = nil
		}
		cur = nxt
	}
	if refund > 0 {
		coroSched.cacheBytes.Add(-refund)
	}
}

func coroSpillGlobalCache(typ *_type, allocSize uintptr, head, tail *coroTask, count uint16) {
	if head == nil || count == 0 {
		return
	}
	lock(&coroSched.cacheLock)
	idx := -1
	for i := range coroSched.globalCache {
		if coroSched.globalCache[i].typ == typ {
			idx = i
			break
		}
	}
	if idx < 0 {
		for i := range coroSched.globalCache {
			if coroSched.globalCache[i].young == nil && coroSched.globalCache[i].old == nil {
				idx = i
				coroSched.globalCache[i].typ = typ
				coroSched.globalCache[i].allocSize = allocSize
				break
			}
		}
	}
	if idx < 0 {
		unlock(&coroSched.cacheLock)
		coroEvictChain(head, count, allocSize)
		return
	}
	gs := &coroSched.globalCache[idx]
	tail.next = gs.young
	gs.young = head
	gs.youngCount += count
	coroSched.globalCacheCount.Add(int32(count))
	unlock(&coroSched.cacheLock)
}

func coroRefillFromGlobalCache(typ *_type) (*coroTask, *coroTask, *coroTask, uint16, uintptr) {
	if coroSched.globalCacheCount.Load() <= 0 {
		return nil, nil, nil, 0, 0
	}
	lock(&coroSched.cacheLock)
	var first, restHead, restTail *coroTask
	var restCount uint16
	var allocSize uintptr
	for i := range coroSched.globalCache {
		gs := &coroSched.globalCache[i]
		if gs.typ != typ || (gs.young == nil && gs.old == nil) {
			continue
		}
		allocSize = gs.allocSize
		var taken uint16
		const maxTake = coroPCacheSpillAt
		for taken < maxTake {
			var t *coroTask
			if gs.young != nil {
				t = gs.young
				gs.young = t.next
				gs.youngCount--
			} else if gs.old != nil {
				t = gs.old
				gs.old = t.next
				gs.oldCount--
			} else {
				break
			}
			t.next = nil
			if first == nil {
				first = t
			} else {
				if restTail == nil {
					restHead = t
				} else {
					restTail.next = t
				}
				restTail = t
				restCount++
			}
			taken++
		}
		if taken > 0 {
			coroSched.globalCacheCount.Add(-int32(taken))
		}
		if gs.youngCount == 0 && gs.oldCount == 0 {
			gs.typ = nil
			gs.allocSize = 0
		}
		break
	}
	unlock(&coroSched.cacheLock)
	return first, restHead, restTail, restCount, allocSize
}

// coroAlloc allocates (or pops from the current P's 2-way *_type freelist,
// refilling in batches from the global spill cache on multi-P misses) a
// combined struct{ coroTask; frame } object and returns its pointer.
func coroAlloc(typ *_type) unsafe.Pointer {
	if !raceenabled && typ != nil && typ.Size_ >= unsafe.Sizeof(coroTask{}) {
		mp := acquirem()
		if pp := mp.p.ptr(); pp != nil {
			if ps := pp.coro.Load(); ps != nil {
				for i := range ps.cache {
					slot := &ps.cache[i]
					if slot.typ != typ {
						continue
					}
					var task *coroTask
					if slot.young != nil {
						task = slot.young
						slot.young = task.next
						slot.youngCount--
					} else if slot.old != nil {
						task = slot.old
						slot.old = task.next
						slot.oldCount--
					}
					if task != nil {
						refund := slot.allocSize
						if task.ioState != nil {
							refund += coroIOAllocSize()
						}
						ps.cacheCredit += refund
						var returnGlobal uintptr
						if ps.cacheCredit > coroCacheCreditHigh {
							returnGlobal = ps.cacheCredit - coroCacheCreditChunk
							ps.cacheCredit = coroCacheCreditChunk
						}
						task.next = nil
						task.typ = typ
						task.flags = (task.flags & coroFlagTracked) | coroFlagPooled
						task.state.Store(coroStateNew)
						releasem(mp)
						if returnGlobal > 0 {
							coroSched.cacheBytes.Add(-returnGlobal)
						}
						return unsafe.Pointer(task)
					}
				}
			}
		}
		releasem(mp)

		if first, restHead, restTail, restCount, allocSize := coroRefillFromGlobalCache(typ); first != nil {
			refund := allocSize
			if first.ioState != nil {
				refund += coroIOAllocSize()
			}
			coroSched.cacheBytes.Add(-refund)
			if restHead != nil {
				mp = acquirem()
				var ps *coroPState
				mp, ps = coroEnsurePState(mp)
				stashed := false
				if ps != nil {
					idx := -1
					for i := range ps.cache {
						if ps.cache[i].typ == typ {
							idx = i
							break
						}
					}
					if idx < 0 {
						for i := range ps.cache {
							if ps.cache[i].young == nil && ps.cache[i].old == nil {
								idx = i
								ps.cache[i].typ = typ
								ps.cache[i].allocSize = allocSize
								break
							}
						}
					}
					if idx >= 0 {
						slot := &ps.cache[idx]
						restTail.next = slot.young
						slot.young = restHead
						slot.youngCount += restCount
						stashed = true
					}
				}
				releasem(mp)
				if !stashed {
					coroSpillGlobalCache(typ, allocSize, restHead, restTail, restCount)
				}
			}
			mp = acquirem()
			first.next = nil
			first.typ = typ
			first.flags = (first.flags & coroFlagTracked) | coroFlagPooled
			first.state.Store(coroStateNew)
			releasem(mp)
			return unsafe.Pointer(first)
		}
	}
	p := newobject(typ)
	task := (*coroTask)(p)
	task.typ = typ
	if !raceenabled {
		task.flags = coroFlagPooled
	}
	return p
}

// coroRecycle clears a completed combined task+frame and returns it to the
// current P's 2-way *_type freelist if within the global 512 KiB budget.
func coroRecycle(task *coroTask) {
	if task == nil {
		return
	}
	coroPrepareRecycleIO(task)
	if raceenabled || task.typ == nil || task.flags&coroFlagPooled == 0 {
		task.ioState = nil
		if task.flags&coroFlagTracked != 0 {
			coroUntrackParked(task)
		}
		return
	}
	typ := task.typ
	allocSize := coroTypeAllocSize(typ)
	ioSize := uintptr(0)
	if task.ioState != nil {
		ioSize = coroIOAllocSize()
	}

	mp := acquirem()
	var ps *coroPState
	mp, ps = coroEnsurePState(mp)
	if ps == nil {
		releasem(mp)
		task.ioState = nil
		if task.flags&coroFlagTracked != 0 {
			coroUntrackParked(task)
		}
		return
	}
	idx := -1
	for i := range ps.cache {
		if ps.cache[i].typ == typ {
			idx = i
			break
		}
	}
	if idx < 0 {
		for i := range ps.cache {
			if ps.cache[i].young == nil && ps.cache[i].old == nil {
				idx = i
				ps.cache[i].typ = typ
				ps.cache[i].allocSize = allocSize
				break
			}
		}
	}
	if idx < 0 {
		releasem(mp)
		task.ioState = nil
		if task.flags&coroFlagTracked != 0 {
			coroUntrackParked(task)
		}
		return
	}
	slot := &ps.cache[idx]
	if int(slot.youngCount)+int(slot.oldCount) >= coroPCacheSlotCap {
		releasem(mp)
		task.ioState = nil
		if task.flags&coroFlagTracked != 0 {
			coroUntrackParked(task)
		}
		return
	}

	// Charge per-P budget credit first (zero global atomics on hot path!).
	need := allocSize + ioSize
	if ps.cacheCredit < need {
		chunk := uintptr(coroCacheCreditChunk)
		if chunk < need {
			chunk = need
		}
		if coroTryReserveBudget(chunk) {
			ps.cacheCredit += chunk
		} else if coroTryReserveBudget(need) {
			ps.cacheCredit += need
		} else if ioSize > 0 && (ps.cacheCredit >= allocSize || coroTryReserveBudget(allocSize)) {
			if ps.cacheCredit < allocSize {
				ps.cacheCredit += allocSize
			}
			task.ioState = nil
			need = allocSize
		} else {
			releasem(mp)
			task.ioState = nil
			if task.flags&coroFlagTracked != 0 {
				coroUntrackParked(task)
			}
			return
		}
	}
	ps.cacheCredit -= need

	// Clear frame payload while keeping exact GC type safety.
	hdrSize := unsafe.Sizeof(coroTask{})
	if typ.Size_ > hdrSize {
		framePtr := add(unsafe.Pointer(task), hdrSize)
		frameLen := typ.Size_ - hdrSize
		if typ.PtrBytes > hdrSize {
			memclrHasPointers(framePtr, frameLen)
		} else {
			memclrNoHeapPointers(framePtr, frameLen)
		}
	}
	// Clear task header fields (keeping zeroed ioState if reserved above and
	// preserving coroFlagTracked/result across pool reuse for zero hot-path overhead).
	tracked := task.flags & coroFlagTracked
	task.resume = nil
	task.next = nil
	task.parent = nil
	task.typ = nil
	task.defers = nil
	task.panicBox = nil
	task.selectDone.Store(0)
	task.state.Store(coroStateNew)
	task.rootSlot.Store(0)
	if tracked == 0 {
		task.result.Store(0)
	}
	task.flags = tracked
	task.semaTicket.Store(0)

	task.next = slot.young
	slot.young = task
	slot.youngCount++

	// On multi-P, spill half a batch to globalCache when local young exceeds threshold
	// so producer Ps (spawning many workers) can reuse frames recycled on worker Ps.
	var spillHead, spillTail *coroTask
	var spillCount uint16
	if gomaxprocs > 1 && slot.youngCount >= coroPCacheSpillAt {
		spillHead = slot.young
		cur := spillHead
		for i := uint16(1); i < coroPCacheSpillCnt && cur.next != nil; i++ {
			cur = cur.next
			spillCount++
		}
		spillCount++ // include spillHead
		spillTail = cur
		slot.young = cur.next
		spillTail.next = nil
		slot.youngCount -= spillCount
	}
	releasem(mp)
	if spillHead != nil {
		coroSpillGlobalCache(typ, allocSize, spillHead, spillTail, spillCount)
	}
}

// coroTrimChain keeps at most keep tasks at the head of chain and evicts the
// remainder back to GC + budget accounting.
func coroTrimChain(head *coroTask, count uint16, keep uint16, allocSize uintptr) (*coroTask, uint16) {
	if count <= keep || head == nil {
		return head, count
	}
	if keep == 0 {
		coroEvictChain(head, count, allocSize)
		return nil, 0
	}
	cur := head
	for i := uint16(1); i < keep && cur.next != nil; i++ {
		cur = cur.next
	}
	evict := cur.next
	cur.next = nil
	if evict != nil {
		coroEvictChain(evict, count-keep, allocSize)
	}
	return head, keep
}

// coroClearPools runs under STW in clearpools(). Ages per-P and global 2-way
// caches once per GC and caps single-GC carry-over so bursty workloads shed
// excess cached frames on GC1 and reclaim 100% on GC2 (0 B retained).
func coroClearPools() {
	assertWorldStopped()
	const gcCarryMax = 64 // max frames per slot carried across one GC boundary
	for i := range coroSched.globalCache {
		gs := &coroSched.globalCache[i]
		if gs.oldCount > 0 {
			coroSched.globalCacheCount.Add(-int32(gs.oldCount))
			coroEvictChain(gs.old, gs.oldCount, gs.allocSize)
			gs.old = nil
			gs.oldCount = 0
		}
		if gs.youngCount > gcCarryMax {
			dropped := gs.youngCount - gcCarryMax
			gs.young, gs.youngCount = coroTrimChain(gs.young, gs.youngCount, gcCarryMax, gs.allocSize)
			coroSched.globalCacheCount.Add(-int32(dropped))
		}
		gs.old = gs.young
		gs.oldCount = gs.youngCount
		gs.young = nil
		gs.youngCount = 0
		if gs.oldCount == 0 {
			gs.typ = nil
			gs.allocSize = 0
		}
	}
	for _, pp := range allp {
		if pp == nil {
			continue
		}
		ps := pp.coro.Load()
		if ps == nil {
			continue
		}
		if ps.cacheCredit > 0 {
			coroSched.cacheBytes.Add(-ps.cacheCredit)
			ps.cacheCredit = 0
		}
		hasCached := false
		for i := range ps.cache {
			slot := &ps.cache[i]
			if slot.oldCount > 0 {
				coroEvictChain(slot.old, slot.oldCount, slot.allocSize)
				slot.old = nil
				slot.oldCount = 0
			}
			if slot.youngCount > gcCarryMax {
				slot.young, slot.youngCount = coroTrimChain(slot.young, slot.youngCount, gcCarryMax, slot.allocSize)
			}
			slot.old = slot.young
			slot.oldCount = slot.youngCount
			slot.young = nil
			slot.youngCount = 0
			if slot.oldCount == 0 {
				slot.typ = nil
				slot.allocSize = 0
			} else {
				hasCached = true
			}
		}
		if !hasCached && ps.runnext.Load() == nil && ps.runqH == ps.runqT && ps.hasWork.Load() == 0 {
			pp.coro.Store(nil)
		}
	}
	coroClearParkedEmpty()
}

// coroDrainP runs under STW when pp is destroyed by procresize.
func coroDrainP(pp *p) {
	assertWorldStopped()
	ps := pp.coro.Load()
	if ps == nil {
		return
	}
	if ps.cacheCredit > 0 {
		coroSched.cacheBytes.Add(-ps.cacheCredit)
		ps.cacheCredit = 0
	}
	// Drop per-P freelist caches cleanly.
	for i := range ps.cache {
		slot := &ps.cache[i]
		if slot.young != nil {
			coroEvictChain(slot.young, slot.youngCount, slot.allocSize)
			slot.young = nil
			slot.youngCount = 0
		}
		if slot.old != nil {
			coroEvictChain(slot.old, slot.oldCount, slot.allocSize)
			slot.old = nil
			slot.oldCount = 0
		}
		slot.typ = nil
		slot.allocSize = 0
	}
	// Move runnable tasks to global queue (world stopped, no lock needed).
	pushGlobal := func(t *coroTask) {
		if t == nil {
			return
		}
		t.next = nil
		if coroSched.runqTail == nil {
			coroSched.runqHead = t
		} else {
			coroSched.runqTail.next = t
		}
		coroSched.runqTail = t
		coroSched.runqLen.Add(1)
	}
	if rn := ps.runnext.Load(); rn != nil {
		ps.runnext.Store(nil)
		pushGlobal(rn)
	}
	for ps.runqH != ps.runqT {
		idx := ps.runqH & (coroPRunqSize - 1)
		t := ps.runq[idx]
		ps.runq[idx] = nil
		ps.runqH++
		pushGlobal(t)
	}
	if ps.hasWork.Load() != 0 {
		ps.hasWork.Store(0)
		coroSched.nonEmptyP.Add(-1)
	}
	pp.coro.Store(nil)
}

func coroMarkPNonEmpty(ps *coroPState) {
	if ps.hasWork.Load() == 0 && ps.hasWork.CompareAndSwap(0, 1) {
		coroSched.nonEmptyP.Add(1)
	}
}

func coroTryMarkPEmpty(ps *coroPState) {
	if ps.hasWork.Load() != 0 && ps.hasWork.CompareAndSwap(1, 0) {
		coroSched.nonEmptyP.Add(-1)
		if ps.runnext.Load() != nil || atomic.LoadAcq(&ps.runqT) != atomic.LoadAcq(&ps.runqH) {
			if ps.hasWork.CompareAndSwap(0, 1) {
				coroSched.nonEmptyP.Add(1)
			}
		}
	}
}

// coroEnqueue puts a runnable task onto the current P's runnext/runq (or global
// queue on overflow) with zero global atomic contention on busy Ps, and wakes
// the root owner directly (if applicable) or an idle helper driver.
func coroEnqueue(task *coroTask, next bool) {
	isRoot := task.flags&coroFlagRoot != 0
	if raceenabled {
		racereleasemerge(unsafe.Pointer(task))
	}
	mp := acquirem()
	var ps *coroPState
	mp, ps = coroEnsurePState(mp)
	pushed := false
	if ps != nil {
		if next && gomaxprocs == 1 {
			old := ps.runnext.Load()
			ps.runnext.Store(task)
			if old == nil {
				coroMarkPNonEmpty(ps)
				pushed = true
			} else {
				task = old // demote displaced runnext into stealable ring
			}
		}
		if !pushed {
			h := atomic.LoadAcq(&ps.runqH)
			t := atomic.Load(&ps.runqT)
			if t-h < coroPRunqSize {
				ps.runq[t&(coroPRunqSize-1)] = task
				atomic.Xadd(&ps.runqT, 1)
				coroMarkPNonEmpty(ps)
				pushed = true
			}
		}
	}
	releasem(mp)
	if !pushed {
		task.next = nil
		lock(&coroSched.lock)
		if coroSched.runqTail == nil {
			coroSched.runqHead = task
		} else {
			coroSched.runqTail.next = task
		}
		coroSched.runqTail = task
		coroSched.runqLen.Add(1)
		unlock(&coroSched.lock)
	}
	if isRoot && coroWakeRootOwner(task) {
		return
	}
	coroMaybeWakeDriver()
}

// coroEnqueueBatch pushes a linked list of already-Runnable tasks in one shot.
func coroEnqueueBatch(head, tail *coroTask, count int32) {
	if head == nil || count == 0 {
		return
	}
	if raceenabled {
		for cur := head; cur != nil; cur = cur.next {
			racereleasemerge(unsafe.Pointer(cur))
		}
	}
	// Put up to half a P-ring locally on current P, rest on global queue for instant stealing.
	mp := acquirem()
	var ps *coroPState
	mp, ps = coroEnsurePState(mp)
	var localN int32
	if ps != nil {
		h := atomic.LoadAcq(&ps.runqH)
		t := atomic.Load(&ps.runqT)
		avail := int32(coroPRunqSize - (t - h))
		limit := avail
		if gomaxprocs > 1 && limit > coroPStealBatch {
			limit = coroPStealBatch
		}
		for localN < limit && head != nil {
			cur := head
			head = cur.next
			cur.next = nil
			ps.runq[(t+uint32(localN))&(coroPRunqSize-1)] = cur
			localN++
		}
		if localN > 0 {
			atomic.Xadd(&ps.runqT, localN)
			coroMarkPNonEmpty(ps)
		}
	}
	releasem(mp)
	rem := count - localN
	if head != nil && rem > 0 {
		tail.next = nil
		lock(&coroSched.lock)
		if coroSched.runqTail == nil {
			coroSched.runqHead = head
		} else {
			coroSched.runqTail.next = head
		}
		coroSched.runqTail = tail
		coroSched.runqLen.Add(rem)
		unlock(&coroSched.lock)
	}
	// Wake up to gomaxprocs - activeDriver drivers for batch fan-out (e.g. close(ch), netpoll).
	want := count
	if avail := gomaxprocs - coroSched.activeDriver.Load(); want > avail {
		want = avail
	}
	for i := int32(0); i < want; i++ {
		if !coroMaybeWakeDriver() {
			break
		}
	}
}

func coroDequeue() *coroTask {
	// 1. Check local P runnext & ring (lock-free, zero global atomics while busy).
	mp := acquirem()
	myp := mp.p.ptr()
	if myp != nil {
		if ps := myp.coro.Load(); ps != nil {
			skipRunnext := false
			if t := ps.runnext.Load(); t != nil {
				ps.runnextStreak++
				if ps.runnextStreak&31 == 0 && atomic.LoadAcq(&ps.runqH) != atomic.Load(&ps.runqT) {
					skipRunnext = true
				}
				if !skipRunnext && ps.runnext.CompareAndSwap(t, nil) {
					if atomic.LoadAcq(&ps.runqH) == atomic.Load(&ps.runqT) {
						coroTryMarkPEmpty(ps)
					}
					releasem(mp)
					if raceenabled {
						raceacquire(unsafe.Pointer(t))
					}
					return t
				}
			}
			t := atomic.Load(&ps.runqT)
			for {
				h := atomic.LoadAcq(&ps.runqH)
				if h == t {
					if !skipRunnext {
						coroTryMarkPEmpty(ps)
					}
					break
				}
				task := ps.runq[h&(coroPRunqSize-1)]
				if atomic.CasRel(&ps.runqH, h, h+1) {
					ps.runnextStreak = 0
					if h+1 == t && ps.runnext.Load() == nil {
						coroTryMarkPEmpty(ps)
					}
					releasem(mp)
					if raceenabled {
						raceacquire(unsafe.Pointer(task))
					}
					return task
				}
			}
			if skipRunnext {
				if rn := ps.runnext.Load(); rn != nil && ps.runnext.CompareAndSwap(rn, nil) {
					if atomic.LoadAcq(&ps.runqH) == atomic.Load(&ps.runqT) {
						coroTryMarkPEmpty(ps)
					}
					releasem(mp)
					if raceenabled {
						raceacquire(unsafe.Pointer(rn))
					}
					return rn
				}
			}
		}
	}
	releasem(mp)

	// Fast exit: zero global or remote P work (1 ns, avoids allp scan on I/O paths).
	if coroSched.runqLen.Load() == 0 && coroSched.nonEmptyP.Load() <= 0 {
		return nil
	}

	// 2. Check global overflow queue (pull a small batch to local P).
	if coroSched.runqLen.Load() > 0 {
		lock(&coroSched.lock)
		first := coroSched.runqHead
		var batchHead, batchTail *coroTask
		var batchN int32
		if first != nil {
			coroSched.runqHead = first.next
			if coroSched.runqHead == nil {
				coroSched.runqTail = nil
			}
			first.next = nil
			remLen := coroSched.runqLen.Add(-1)
			// Also grab up to min(runqLen/gomaxprocs, 64) tasks for local P.
			want := remLen / max(gomaxprocs, 1)
			if want > 64 {
				want = 64
			}
			for batchN < want && coroSched.runqHead != nil {
				cur := coroSched.runqHead
				coroSched.runqHead = cur.next
				if coroSched.runqHead == nil {
					coroSched.runqTail = nil
				}
				cur.next = nil
				coroSched.runqLen.Add(-1)
				if batchTail == nil {
					batchHead = cur
				} else {
					batchTail.next = cur
				}
				batchTail = cur
				batchN++
			}
		}
		unlock(&coroSched.lock)
		if first != nil {
			if batchHead != nil {
				coroPutStolenToLocal(batchHead, batchTail, batchN)
			}
			if raceenabled {
				raceacquire(unsafe.Pointer(first))
			}
			return first
		}
	}

	// 3. Steal half from another P's ring (lock-free).
	if coroSched.nonEmptyP.Load() <= 0 {
		return nil
	}
	return coroStealFromOtherP()
}

//go:noinline
func coroPutStolenToLocal(head, tail *coroTask, count int32) {
	mp := acquirem()
	var ps *coroPState
	mp, ps = coroEnsurePState(mp)
	var stored int32
	if ps != nil {
		h := atomic.LoadAcq(&ps.runqH)
		t := atomic.Load(&ps.runqT)
		avail := int32(coroPRunqSize - (t - h))
		for stored < avail && head != nil {
			cur := head
			head = cur.next
			cur.next = nil
			ps.runq[(t+uint32(stored))&(coroPRunqSize-1)] = cur
			stored++
		}
		if stored > 0 {
			atomic.Xadd(&ps.runqT, stored)
			coroMarkPNonEmpty(ps)
		}
	}
	releasem(mp)
	if head != nil {
		rem := count - stored
		tail.next = nil
		lock(&coroSched.lock)
		if coroSched.runqTail == nil {
			coroSched.runqHead = head
		} else {
			coroSched.runqTail.next = head
		}
		coroSched.runqTail = tail
		coroSched.runqLen.Add(rem)
		unlock(&coroSched.lock)
	}
}

//go:noinline
func coroStealFromOtherP() *coroTask {
	mp := acquirem()
	myp := mp.p.ptr()
	nprocs := len(allp)
	if myp == nil || nprocs <= 1 {
		releasem(mp)
		return nil
	}
	var batch [coroPStealBatch]*coroTask
	start := (int(myp.id) + 1) % nprocs
	for off := 0; off < nprocs-1; off++ {
		vp := allp[(start+off)%nprocs]
		if vp == nil || vp == myp {
			continue
		}
		vps := vp.coro.Load()
		if vps == nil || vps.hasWork.Load() == 0 {
			continue
		}
		for {
			h := atomic.LoadAcq(&vps.runqH)
			t := atomic.LoadAcq(&vps.runqT)
			n := t - h
			if n == 0 || n > coroPRunqSize {
				if vps.runnext.Load() == nil {
					coroTryMarkPEmpty(vps)
				}
				break
			}
			take := (n + 1) / 2
			if take > coroPStealBatch {
				take = coroPStealBatch
			}
			for i := uint32(0); i < take; i++ {
				batch[i] = vps.runq[(h+i)&(coroPRunqSize-1)]
			}
			if atomic.CasRel(&vps.runqH, h, h+take) {
				if h+take == t && vps.runnext.Load() == nil {
					coroTryMarkPEmpty(vps)
				}
				first := batch[0]
				if take > 1 {
					var myps *coroPState
					mp, myps = coroEnsurePState(mp)
					if myps != nil {
						mh := atomic.LoadAcq(&myps.runqH)
						mt := atomic.Load(&myps.runqT)
						avail := coroPRunqSize - (mt - mh)
						stored := uint32(0)
						for i := uint32(1); i < take && stored < avail; i++ {
							myps.runq[(mt+stored)&(coroPRunqSize-1)] = batch[i]
							stored++
						}
						if stored > 0 {
							atomic.Xadd(&myps.runqT, int32(stored))
							coroMarkPNonEmpty(myps)
						}
						if stored+1 < take {
							// Put leftovers onto global queue after releasem.
							var lh, lt *coroTask
							var ln int32
							for i := stored + 1; i < take; i++ {
								x := batch[i]
								x.next = nil
								if lt == nil {
									lh = x
								} else {
									lt.next = x
								}
								lt = x
								ln++
							}
							releasem(mp)
							lock(&coroSched.lock)
							if coroSched.runqTail == nil {
								coroSched.runqHead = lh
							} else {
								coroSched.runqTail.next = lh
							}
							coroSched.runqTail = lt
							coroSched.runqLen.Add(ln)
							unlock(&coroSched.lock)
							if raceenabled {
								raceacquire(unsafe.Pointer(first))
							}
							return first
						}
					}
				}
				releasem(mp)
				if raceenabled {
					raceacquire(unsafe.Pointer(first))
				}
				return first
			}
		}
		if rn := vps.runnext.Load(); rn != nil {
			if vps.runnext.CompareAndSwap(rn, nil) {
				if atomic.LoadAcq(&vps.runqH) == atomic.LoadAcq(&vps.runqT) {
					coroTryMarkPEmpty(vps)
				}
				releasem(mp)
				if raceenabled {
					raceacquire(unsafe.Pointer(rn))
				}
				return rn
			}
		}
	}
	releasem(mp)
	return nil
}

// coroPopIdleDriver atomically pops one parked driver goroutine without locks
// or write barriers (safe to call from netpollunblock).
//
//go:nowritebarrier
func coroPopIdleDriver() *g {
	if coroSched.idleCount.Load() <= 0 {
		return nil
	}
	// Check permanent workers first, then parked root drivers.
	nw := coroSched.workerCount.Load()
	for i := int32(0); i < nw; i++ {
		if gp := coroSched.idleDrivers[i].Load(); gp != 0 {
			if coroSched.idleDrivers[i].CompareAndSwap(gp, 0) {
				coroSched.idleCount.Add(-1)
				return (*g)(unsafe.Pointer(gp))
			}
		}
	}
	for i := int32(coroMaxWorkers); i < coroTotalDrivers; i++ {
		if gp := coroSched.idleDrivers[i].Load(); gp != 0 {
			if coroSched.idleDrivers[i].CompareAndSwap(gp, 0) {
				coroSched.idleCount.Add(-1)
				return (*g)(unsafe.Pointer(gp))
			}
		}
	}
	return nil
}

// coroMaybeWakeDriver ensures a driver is active when work exists and CPU
// parallelism is available. Returns true if a driver was woken or spawned.
func coroMaybeWakeDriver() bool {
	if coroSched.activeDriver.Load() >= gomaxprocs {
		return false
	}
	if coroSched.idleCount.Load() > 0 {
		if gp := coroPopIdleDriver(); gp != nil {
			goready(gp, 0)
			return true
		}
	}
	// Lazily spawn permanent helper workers up to min(gomaxprocs, coroMaxWorkers)
	// when active drivers < gomaxprocs. On P=1 with a running root driver,
	// activeDriver == 1 >= gomaxprocs so zero helpers are spawned; if a detached
	// child outlives its root (or an ordinary G spawns directly), activeDriver == 0
	// lazily admits 1 worker.
	maxW := gomaxprocs
	if maxW > coroMaxWorkers {
		maxW = coroMaxWorkers
	}
	if maxW <= 0 {
		return false
	}
	for {
		cur := coroSched.workerCount.Load()
		if cur >= maxW {
			return false
		}
		if coroSched.workerCount.CompareAndSwap(cur, cur+1) {
			wid := cur
			go coroWorkerMain(wid)
			return true
		}
	}
}

func coroWorkerMain(wid int32) {
	coroDriverLoop(nil, wid)
}

func coroHasWork() bool {
	return coroSched.nonEmptyP.Load() > 0 || coroSched.runqLen.Load() > 0 || coroNetpollHasReady()
}

func coroRunnableCount() int32 {
	total := coroSched.runqLen.Load()
	mp := acquirem()
	for _, pp := range allp {
		if pp == nil {
			continue
		}
		if ps := pp.coro.Load(); ps != nil {
			if ps.runnext.Load() != nil {
				total++
			}
			h := atomic.LoadAcq(&ps.runqH)
			t := atomic.LoadAcq(&ps.runqT)
			if t > h && t-h <= coroPRunqSize {
				total += int32(t - h)
			}
		}
	}
	releasem(mp)
	return total
}

type coroDriverParkArg struct {
	slot int32
	root *coroTask
}

func coroDriverParkCommit(gp *g, arg unsafe.Pointer) bool {
	pa := (*coroDriverParkArg)(arg)
	slot := &coroSched.idleDrivers[pa.slot]
	slot.Store(uintptr(unsafe.Pointer(gp)))
	if pa.root != nil {
		// Record our idle slot before the full-barrier idleCount.Add(1).
		pa.root.rootSlot.Store(uint32(pa.slot) + 1)
	}
	coroSched.idleCount.Add(1)
	// Dekker re-check after publishing our idle slot and full barrier.
	if (pa.root != nil && pa.root.state.Load() == coroStateDone) || coroHasWork() {
		if slot.CompareAndSwap(uintptr(unsafe.Pointer(gp)), 0) {
			coroSched.idleCount.Add(-1)
			if pa.root != nil {
				pa.root.rootSlot.Store(0)
			}
			return false // cancel park; stay running!
		}
	}
	return true
}

func coroWakeRootOwner(root *coroTask) bool {
	slotPlus1 := root.rootSlot.Load()
	if slotPlus1 == 0 {
		return false
	}
	idx := slotPlus1 - 1
	if idx >= coroTotalDrivers {
		return false
	}
	gp := coroSched.idleDrivers[idx].Load()
	if gp == 0 || (*g)(unsafe.Pointer(gp)) == getg() {
		return false
	}
	if coroSched.idleDrivers[idx].CompareAndSwap(gp, 0) {
		coroSched.idleCount.Add(-1)
		goready((*g)(unsafe.Pointer(gp)), 0)
		return true
	}
	return false
}

func coroClaimRootSlot() int32 {
	for i := int32(0); i < coroMaxRootSlots; i++ {
		if coroSched.rootSlotUsed[i].Load() == 0 && coroSched.rootSlotUsed[i].CompareAndSwap(0, 1) {
			return coroMaxWorkers + i
		}
	}
	return -1
}

func coroReleaseRootSlot(slot int32) {
	if slot >= coroMaxWorkers && slot < coroTotalDrivers {
		coroSched.rootSlotUsed[slot-coroMaxWorkers].Store(0)
	}
}

// coroReady transitions a Waiting/Parked task to ReadyPending or Runnable.
func coroReady(task *coroTask) {
	for {
		s := task.state.Load()
		if s == coroStateWaiting {
			if task.state.CompareAndSwap(coroStateWaiting, coroStateWaiting|coroFlagReadyPending) {
				return
			}
			continue
		}
		if s == coroStateParked {
			if task.state.CompareAndSwap(coroStateParked, coroStateRunnable) {
				if raceenabled {
					raceacquire(unsafe.Pointer(task))
				}
				coroEnqueue(task, true)
				return
			}
			continue
		}
		throw("runtime: invalid coroReady transition")
	}
}

// coroRunFrame is the public entry point emitted by the compiler for any
// lowered Go function called synchronously from an ordinary Go goroutine.
// frame points to a combined struct{ coroTask; frame } allocated via coroAlloc.
func coroRunFrame(frame unsafe.Pointer, resume func(unsafe.Pointer) uint8) {
	coroClearPanicTrace(getg().goid)
	task := (*coroTask)(frame)
	task.resume = resume
	task.flags |= coroFlagRoot
	task.state.Store(coroStateRunning)
	coroDriverLoop(task, -1)
	if raceenabled {
		raceacquire(unsafe.Pointer(task))
	}
	// Propagate unhandled panic or runtime.Goexit() onto the caller's Go stack.
	if pb := task.panicBox; pb != nil {
		if pb.goexit {
			coroRecycle(task)
			Goexit()
		}
		if pb.active {
			val := pb.val
			coroPublishPanicTrace(getg().goid, pb.tracePCs)
			coroRecycle(task)
			panic(val)
		}
	}
	// Note: caller copies return values out of frame before calling coroReleaseFrame,
	// OR for zero-return roots caller may recycle immediately.
}

// coroReleaseFrame recycles a completed root frame after the public wrapper has
// read its return values.
func coroReleaseFrame(frame unsafe.Pointer) {
	if frame != nil {
		coroRecycle((*coroTask)(frame))
	}
}

// coroSpawnFrame publishes an independent logical goroutine (`go fn(...)`).
// frame points to a combined struct{ coroTask; frame } allocated via coroAlloc
// with arguments already populated by the caller.
func coroSpawnFrame(ctx, frame unsafe.Pointer, resume func(unsafe.Pointer) uint8) bool {
	task := (*coroTask)(frame)
	task.resume = resume
	task.parent = nil
	task.flags |= coroFlagUnhosted
	task.state.Store(coroStateRunnable)
	coroAddUnhosted(1)
	coroEnqueue(task, false)
	return false
}

// coroAwaitFrame initiates a compiler-lowered structured call from cur to child.
// Both share the same logical goroutine; the driver switches directly to child.
func coroAwaitFrame(ctx, childFrame unsafe.Pointer, resume func(unsafe.Pointer) uint8) uint8 {
	cur := (*coroTask)(ctx)
	child := (*coroTask)(childFrame)
	child.resume = resume
	child.parent = cur
	if cur.flags&(coroFlagUnhosted|coroFlagUnhostedChild) != 0 {
		child.flags |= coroFlagUnhostedChild
	}
	child.state.Store(coroStateRunning)
	// Stash child in cur.next for immediate coroActionSwitch pickup in coroDriverLoop.
	cur.next = child
	return coroActionSwitch
}

// coroDefer pushes a plain deferred closure onto the current task's LIFO stack.
func coroDefer(ctx unsafe.Pointer, fn func()) {
	task := (*coroTask)(ctx)
	task.defers = &coroDeferNode{next: task.defers, fn: fn}
}

// coroDeferCoro pushes a lowered coroutine defer onto the current task's LIFO stack.
func coroDeferCoro(ctx, frame unsafe.Pointer, resume func(unsafe.Pointer) uint8) {
	task := (*coroTask)(ctx)
	task.defers = &coroDeferNode{next: task.defers, frame: frame, resume: resume}
}

// coroPanic records a panic payload on the active task and switches to defer unwinding.
func coroPanic(ctx unsafe.Pointer, val any) {
	task := (*coroTask)(ctx)
	if task.panicBox == nil {
		task.panicBox = &coroPanicState{}
	}
	task.panicBox.val = val
	task.panicBox.active = true
	coroRecordPanicChain(task, task.panicBox)
}

// coroGoexit marks the active task as terminating via runtime.Goexit().
func coroGoexit(ctx unsafe.Pointer) {
	task := (*coroTask)(ctx)
	if task.panicBox == nil {
		task.panicBox = &coroPanicState{}
	}
	task.panicBox.active = false
	task.panicBox.val = nil
	task.panicBox.goexit = true
}

// coroRecover implements recover() inside a deferred coroutine or closure
// running on behalf of a stackless coroutine task.
func coroRecover(ctx unsafe.Pointer) any {
	task := (*coroTask)(ctx)
	target := task
	if task.flags&coroFlagDeferChild != 0 && task.parent != nil {
		target = task.parent
	}
	if target.panicBox == nil || !target.panicBox.active {
		return nil
	}
	val := target.panicBox.val
	target.panicBox.active = false
	target.panicBox.val = nil
	if val == nil {
		if debug.panicnil.Load() != 1 {
			val = new(PanicNilError)
		} else {
			panicnil.IncNonDefault()
		}
	}
	return val
}

// coroRunDefers drains task.defers LIFO. If a deferred entry is a lowered
// coroutine, it returns that child task for immediate coroActionSwitch execution;
// otherwise it runs plain deferred closures inline (preserving recover scope).
func coroRunDefers(task *coroTask) *coroTask {
	for task.defers != nil {
		d := task.defers
		task.defers = d.next
		d.next = nil
		if d.frame != nil && d.resume != nil {
			child := (*coroTask)(d.frame)
			child.resume = d.resume
			child.parent = task
			child.flags |= coroFlagDeferChild
			if task.flags&(coroFlagUnhosted|coroFlagUnhostedChild) != 0 {
				child.flags |= coroFlagUnhostedChild
			}
			child.state.Store(coroStateRunning)
			return child
		}
		if d.fn != nil {
			// Run plain closure with panic interception so a panic in a defer
			// replaces task's active panic per Go spec.
			coroInvokePlainDefer(task, d.fn)
		}
	}
	return nil
}

func coroInvokePlainDefer(task *coroTask, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			coroPanic(unsafe.Pointer(task), r)
		}
	}()
	fn()
}

// coroDriverLoop executes stackless coroutine tasks directly on the calling Go
// goroutine's stack (managed-coro). When root != nil, it returns as soon as
// root completes; when root == nil, it is a permanent multi-P helper worker.
func coroDriverLoop(root *coroTask, workerID int32) {
	coroSched.activeDriver.Add(1)
	defer func() {
		coroSched.activeDriver.Add(-1)
		if coroHasWork() {
			coroMaybeWakeDriver()
		}
	}()

	var task *coroTask
	if root != nil {
		task = root
	}
	startTerminal := false
	for {
		done, nextTask, nextTerminal := coroDriverStepLoop(root, workerID, task, startTerminal)
		if done {
			return
		}
		task = nextTask
		startTerminal = nextTerminal
	}
}

func coroDriverStepLoop(root *coroTask, workerID int32, task *coroTask, startTerminal bool) (done bool, nextTask *coroTask, nextTerminal bool) {
	var active *coroTask
	defer func() {
		if active != nil {
			if r := recover(); r != nil {
				coroPanic(unsafe.Pointer(active), r)
				done = false
				nextTask = active
				nextTerminal = true
			}
		}
	}()

	yieldBudget := uint8(0)
	stepBudget := uint8(0)
	var action uint8
	var lastParkedIO *coroTask
	var lastProbedIO *coroTask
	ioProbeMisses := 0
	didWork := false

	for {
		if startTerminal && task != nil {
			startTerminal = false
			goto handleTerminal
		}
		if task == nil {
			if root != nil && root.state.Load() == coroStateDone {
				return true, nil, false
			}
			// Drain ready direct-netpoll completions before dequeuing runq.
			if readyTask := coroDrainNetpollOne(); readyTask != nil {
				task = readyTask
				ioProbeMisses = 0
				lastProbedIO = nil
			} else {
				task = coroDequeue()
				if task != nil {
					ioProbeMisses = 0
					lastProbedIO = nil
				}
			}
			if task == nil {
				if root != nil && root.state.Load() == coroStateDone {
					return true, nil, false
				}
				// Before parking, try directly polling a peer parked I/O task when
				// this driver recently performed work (avoids kevent/epoll + futex trip).
				if didWork && ioProbeMisses < 4 {
					if claimed := coroTryPollParkedIO(lastParkedIO, lastProbedIO); claimed != nil {
						lastProbedIO = claimed
						ioProbeMisses++
						task = claimed
						goto resume
					}
				}
				didWork = false
				ioProbeMisses = 0
				lastParkedIO = nil
				lastProbedIO = nil
				var slot int32
				if workerID >= 0 {
					slot = workerID
				} else {
					slot = coroClaimRootSlot()
					if slot < 0 {
						Gosched()
						continue
					}
				}
				arg := coroDriverParkArg{slot: slot, root: root}
				coroSched.activeDriver.Add(-1)
				gopark(coroDriverParkCommit, noescape(unsafe.Pointer(&arg)), waitReasonCoroutine, traceBlockGeneric, 1)
				coroSched.activeDriver.Add(1)
				if root != nil {
					root.rootSlot.Store(0)
					coroReleaseRootSlot(slot)
					if root.state.Load() == coroStateDone {
						return true, nil, false
					}
				}
				continue
			}
			task.state.Store(coroStateRunning)
		}

	resume:
		// Perform deferred cleanup for multi-case select or direct netpoll I/O
		// now that this driver exclusively owns task.
		if task.flags&coroFlagSelectPending != 0 {
			if coroSelectFinish(task) {
				goto handleTerminal
			}
		}
		if task.flags&coroFlagIOPending != 0 {
			if coroCompleteIO(task) {
				goto handleWait
			}
			// Direct I/O completed! Reset probe misses so we can chain to the peer.
			ioProbeMisses = 0
			lastProbedIO = nil
		}

		// Periodically honor Go scheduler preemption across all coroutine actions
		// (channel ping-pong, sync.Mutex loops, deep awaits, I/O), not only Gosched.
		stepBudget++
		if stepBudget&63 == 0 && getg().preempt {
			if root == nil || task != root || coroHasWork() {
				task.state.Store(coroStateRunnable)
				coroEnqueue(task, false)
				task = nil
				Gosched()
				continue
			}
			Gosched()
		}

		didWork = true
		active = task
		action = task.resume(unsafe.Pointer(task))
		active = nil

		switch action {
		case coroActionComplete:
			if task.defers != nil {
				if child := coroRunDefers(task); child != nil {
					task = child
					goto resume
				}
			}
			if task.panicBox != nil && (task.panicBox.active || task.panicBox.goexit) {
				goto handleTerminal
			}
			task = coroFinishTask(task, root)
			if task != nil {
				goto resume
			}
			continue

		case coroActionSwitch:
			child := task.next
			task.next = nil
			if child == nil {
				throw("runtime: coroActionSwitch without child")
			}
			task = child
			goto resume

		case coroActionYield:
			yieldBudget++
			// Periodically check preemption / host goroutines so ordinary Gs never starve.
			if yieldBudget&63 == 0 || getg().preempt {
				if root == nil || task != root || coroHasWork() {
					task.state.Store(coroStateRunnable)
					coroEnqueue(task, false)
					task = nil
					Gosched()
					continue
				}
				Gosched()
				goto resume
			}
			// Fast path: if no other coroutine work is pending, continue directly!
			if !coroHasWork() {
				goto resume
			}
			// Swap with next runnable task.
			if next := coroDequeue(); next != nil {
				task.state.Store(coroStateRunnable)
				coroEnqueue(task, false)
				next.state.Store(coroStateRunning)
				task = next
				goto resume
			}
			goto resume

		case coroActionWait:
			goto handleWait

		case coroActionPanic, coroActionGoexit:
			goto handleTerminal

		default:
			throw("runtime: invalid coroutine resume action")
		}

	handleWait:
		if task.flags&(coroFlagUnhosted|coroFlagUnhostedChild) != 0 && task.flags&coroFlagTracked == 0 {
			coroTrackParked(task)
		}
		if raceenabled {
			racereleasemerge(unsafe.Pointer(task))
		}
		if task.flags&coroFlagIOPending != 0 {
			lastParkedIO = task
		}
		if task.state.CompareAndSwap(coroStateWaiting, coroStateParked) {
			task = nil
			continue
		}
		if !task.state.CompareAndSwap(coroStateWaiting|coroFlagReadyPending, coroStateRunning) {
			throw("runtime: invalid coroActionWait transition")
		}
		if raceenabled {
			raceacquire(unsafe.Pointer(task))
		}
		goto resume

	handleTerminal:
		if task.defers != nil {
			if child := coroRunDefers(task); child != nil {
				task = child
				goto resume
			}
		}
		if task.panicBox != nil && !task.panicBox.active && !task.panicBox.goexit {
			// Recovered inside a defer! Complete normally.
			task = coroFinishTask(task, root)
			if task != nil {
				goto resume
			}
			continue
		}
		task = coroFinishTerminal(task, root)
		if task != nil {
			goto resume
		}
		continue
	}
}

// coroFinishTask retires a normally completed task and returns the next task
// to execute immediately (e.g. structured parent), or nil.
func coroFinishTask(task, driverRoot *coroTask) *coroTask {
	parent := task.parent
	task.parent = nil
	if parent != nil {
		isDefer := task.flags&coroFlagDeferChild != 0
		if isDefer {
			if task.flags&coroFlagRoot == 0 {
				coroRecycle(task)
			}
			// Deferred coroutine finished; continue draining parent's remaining defers
			// or finish parent.
			if child := coroRunDefers(parent); child != nil {
				return child
			}
			if parent.panicBox != nil && (parent.panicBox.active || parent.panicBox.goexit) {
				return coroFinishTerminal(parent, driverRoot)
			}
			return coroFinishTask(parent, driverRoot)
		}
		// Normal structured await child completed: parent reads return values from
		// child's frame and then calls coroReleaseFrame(child).
		task.state.Store(coroStateDone)
		parent.state.Store(coroStateRunning)
		return parent
	}
	if task.flags&coroFlagUnhosted != 0 {
		coroAddUnhosted(-1)
	}
	if task.flags&coroFlagRoot != 0 {
		if raceenabled {
			racereleasemerge(unsafe.Pointer(task))
		}
		task.state.Swap(coroStateDone)
		coroWakeRootOwner(task)
		return nil
	}
	task.state.Store(coroStateDone)
	coroRecycle(task)
	return nil
}

// coroFinishTerminal propagates an unhandled panic or runtime.Goexit() up the
// structured parent chain, or terminates the logical goroutine / root.
func coroFinishTerminal(task, driverRoot *coroTask) *coroTask {
	pb := task.panicBox
	parent := task.parent
	task.parent = nil
	if parent != nil {
		// Propagate panic / goexit to parent.
		if pb != nil && (pb.active || pb.goexit) {
			if parent.panicBox == nil {
				parent.panicBox = pb
			} else if pb.goexit {
				parent.panicBox.goexit = true
				parent.panicBox.active = false
				parent.panicBox.val = nil
			} else if pb.active {
				parent.panicBox.val = pb.val
				parent.panicBox.tracePCs = pb.tracePCs
				parent.panicBox.active = true
			}
			task.panicBox = nil
		}
		if task.flags&coroFlagRoot == 0 {
			coroRecycle(task)
		}
		// Unwind parent's defers.
		if child := coroRunDefers(parent); child != nil {
			return child
		}
		if parent.panicBox != nil && !parent.panicBox.active && !parent.panicBox.goexit {
			return coroFinishTask(parent, driverRoot)
		}
		return coroFinishTerminal(parent, driverRoot)
	}
	if task.flags&coroFlagUnhosted != 0 {
		coroAddUnhosted(-1)
	}
	if task.flags&coroFlagRoot != 0 {
		if raceenabled {
			racereleasemerge(unsafe.Pointer(task))
		}
		task.state.Swap(coroStateDone)
		coroWakeRootOwner(task)
		return nil
	}
	// Independent `go` goroutine terminated with unhandled panic or Goexit.
	task.state.Store(coroStateDone)
	if pb != nil && pb.active && !pb.goexit {
		val := pb.val
		coroPublishPanicTrace(getg().goid, pb.tracePCs)
		coroRecycle(task)
		// Unhandled panic in independent goroutine crashes the process, matching Go spec.
		panic(val)
	}
	coroRecycle(task)
	return nil
}
