// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
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
)

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
	result     uint32
	flags      uint32
	_          uint32
}

type coroDeferNode struct {
	next   *coroDeferNode
	fn     func()
	frame  unsafe.Pointer
	resume func(unsafe.Pointer) uint8
}

type coroPanicState struct {
	val    any
	active bool
	goexit bool
}

const (
	coroPRunqSize     = 512
	coroPStealBatch   = 128
	coroPCacheSlots   = 2
	coroPCacheSlotCap = 1024
	coroCacheBudget   = 512 << 10
	coroMaxWorkers    = 128
	coroMaxRootSlots  = 128
	coroTotalDrivers  = coroMaxWorkers + coroMaxRootSlots
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
	runnext *coroTask
	runqH   uint32
	runqT   uint32
	runq    [coroPRunqSize]*coroTask

	cache      [coroPCacheSlots]coroTypeCacheSlot
	cacheNext  uint8
	unhosted   int64 // buffered delta folded to coroSched.unhosted on threshold/GC
}

var coroSched struct {
	lock         mutex
	runqHead     *coroTask
	runqTail     *coroTask
	runqLen      int32
	runnable     atomic.Int32
	unhosted     atomic.Int64
	cacheBytes   atomic.Uintptr
	activeDriver atomic.Int32
	workerCount  atomic.Int32
	idleCount    atomic.Int32
	idleDrivers  [coroTotalDrivers]atomic.Uintptr // guintptr of parked driver Gs
	rootSlotUsed [coroMaxRootSlots]atomic.Uint32  // 0=free, 1=claimed by parked root caller
}

func init() {
	lockInit(&coroSched.lock, lockRankCoroSched)
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

func coroUnhostedCount() uint64 {
	n := coroSched.unhosted.Load()
	if n < 0 {
		return 0
	}
	return uint64(n)
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

// coroAlloc allocates (or pops from the current P's 2-way *_type freelist) a
// combined struct{ coroTask; frame } object and returns its pointer (which is
// both *coroTask and *F_frame).
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
						allocSize := slot.allocSize
						releasem(mp)
						coroSched.cacheBytes.Add(-allocSize)
						task.next = nil
						task.typ = typ
						task.flags = coroFlagPooled
						task.state.Store(coroStateNew)
						return unsafe.Pointer(task)
					}
				}
			}
		}
		releasem(mp)
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
	if raceenabled || task == nil || task.typ == nil || task.flags&coroFlagPooled == 0 {
		return
	}
	typ := task.typ
	allocSize := coroTypeAllocSize(typ)
	// Fast atomic budget reservation.
	for {
		cur := coroSched.cacheBytes.Load()
		if cur+allocSize > coroCacheBudget {
			return
		}
		if coroSched.cacheBytes.CompareAndSwap(cur, cur+allocSize) {
			break
		}
	}
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
	// Clear task header fields (retain clean ioState? Drop it to keep zero retention).
	task.resume = nil
	task.next = nil
	task.parent = nil
	task.typ = nil
	task.defers = nil
	task.panicBox = nil
	task.ioState = nil
	task.selectDone.Store(0)
	task.state.Store(coroStateNew)
	task.rootSlot.Store(0)
	task.result = 0
	task.flags = 0

	mp := acquirem()
	var ps *coroPState
	mp, ps = coroEnsurePState(mp)
	if ps == nil {
		releasem(mp)
		coroSched.cacheBytes.Add(-allocSize)
		return
	}
	// Find matching slot, empty slot, or evict oldest slot.
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
		coroSched.cacheBytes.Add(-allocSize)
		return
	}
	slot := &ps.cache[idx]
	if int(slot.youngCount)+int(slot.oldCount) >= coroPCacheSlotCap {
		releasem(mp)
		coroSched.cacheBytes.Add(-allocSize)
		return
	}
	task.next = slot.young
	slot.young = task
	slot.youngCount++
	releasem(mp)
}

// coroClearPools runs under STW in clearpools(). Ages per-P 2-way caches once
// per GC so two idle collections reclaim 100% of cached frames (0 B retained).
func coroClearPools() {
	assertWorldStopped()
	for _, pp := range allp {
		if pp == nil {
			continue
		}
		ps := pp.coro.Load()
		if ps == nil {
			continue
		}
		if ps.unhosted != 0 {
			coroSched.unhosted.Add(ps.unhosted)
			ps.unhosted = 0
		}
		hasCached := false
		for i := range ps.cache {
			slot := &ps.cache[i]
			if slot.oldCount > 0 {
				coroSched.cacheBytes.Add(-uintptr(slot.oldCount) * slot.allocSize)
				// Break chain links so no stray pointer pins the list.
				for cur := slot.old; cur != nil; {
					nxt := cur.next
					cur.next = nil
					cur = nxt
				}
				slot.old = nil
				slot.oldCount = 0
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
		if !hasCached && ps.runnext == nil && ps.runqH == ps.runqT {
			pp.coro.Store(nil)
		}
	}
}

// coroDrainP runs under STW when pp is destroyed by procresize.
func coroDrainP(pp *p) {
	assertWorldStopped()
	ps := pp.coro.Load()
	if ps == nil {
		return
	}
	if ps.unhosted != 0 {
		coroSched.unhosted.Add(ps.unhosted)
		ps.unhosted = 0
	}
	// Drop per-P freelist caches cleanly.
	for i := range ps.cache {
		slot := &ps.cache[i]
		dropped := uintptr(slot.youngCount+slot.oldCount) * slot.allocSize
		if dropped > 0 {
			coroSched.cacheBytes.Add(-dropped)
		}
		slot.young = nil
		slot.old = nil
		slot.youngCount = 0
		slot.oldCount = 0
		slot.typ = nil
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
		coroSched.runqLen++
	}
	if ps.runnext != nil {
		pushGlobal(ps.runnext)
		ps.runnext = nil
	}
	for ps.runqH != ps.runqT {
		idx := ps.runqH & (coroPRunqSize - 1)
		t := ps.runq[idx]
		ps.runq[idx] = nil
		ps.runqH++
		pushGlobal(t)
	}
	pp.coro.Store(nil)
}

// coroEnqueue puts a runnable task onto the current P's runnext/runq (or global
// queue on overflow) and wakes an idle driver if multi-P parallelism is available.
func coroEnqueue(task *coroTask, next bool) {
	if raceenabled {
		racerelease(unsafe.Pointer(task))
	}
	mp := acquirem()
	var ps *coroPState
	mp, ps = coroEnsurePState(mp)
	pushed := false
	if ps != nil {
		if next {
			old := ps.runnext
			ps.runnext = task
			if old == nil {
				coroSched.runnable.Add(1)
				pushed = true
			} else {
				task = old // demote displaced runnext into ring
			}
		}
		if !pushed {
			h := atomic.LoadAcq(&ps.runqH)
			t := ps.runqT
			if t-h < coroPRunqSize {
				ps.runq[t&(coroPRunqSize-1)] = task
				coroSched.runnable.Add(1)
				atomic.StoreRel(&ps.runqT, t+1)
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
		coroSched.runqLen++
		coroSched.runnable.Add(1)
		unlock(&coroSched.lock)
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
			racerelease(unsafe.Pointer(cur))
		}
	}
	// Put up to half a P-ring locally on current P, rest on global queue for instant stealing.
	mp := acquirem()
	var ps *coroPState
	mp, ps = coroEnsurePState(mp)
	var localN int32
	if ps != nil {
		h := atomic.LoadAcq(&ps.runqH)
		t := ps.runqT
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
			coroSched.runnable.Add(localN)
			atomic.StoreRel(&ps.runqT, t+uint32(localN))
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
		coroSched.runqLen += rem
		coroSched.runnable.Add(rem)
		unlock(&coroSched.lock)
	}
	// Wake up to gomaxprocs-1 drivers for batch fan-out (e.g. close(ch)).
	want := count
	if want > gomaxprocs {
		want = gomaxprocs
	}
	for i := int32(0); i < want; i++ {
		if !coroMaybeWakeDriver() {
			break
		}
	}
}

func coroDequeue() *coroTask {
	// 1. Check local P runnext & ring (lock-free).
	mp := acquirem()
	myp := mp.p.ptr()
	if myp != nil {
		if ps := myp.coro.Load(); ps != nil {
			if t := ps.runnext; t != nil {
				ps.runnext = nil
				coroSched.runnable.Add(-1)
				releasem(mp)
				if raceenabled {
					raceacquire(unsafe.Pointer(t))
				}
				return t
			}
			t := ps.runqT
			for {
				h := atomic.LoadAcq(&ps.runqH)
				if h == t {
					break
				}
				task := ps.runq[h&(coroPRunqSize-1)]
				if atomic.CasRel(&ps.runqH, h, h+1) {
					coroSched.runnable.Add(-1)
					releasem(mp)
					if raceenabled {
						raceacquire(unsafe.Pointer(task))
					}
					return task
				}
			}
		}
	}
	releasem(mp)

	if coroSched.runnable.Load() <= 0 {
		return nil
	}

	// 2. Check global overflow queue (pull a small batch to local P).
	if coroSched.runqLen > 0 {
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
			coroSched.runqLen--
			// Also grab up to min(runqLen/gomaxprocs+1, 64) tasks for local P.
			want := coroSched.runqLen / max(gomaxprocs, 1)
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
				coroSched.runqLen--
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
			coroSched.runnable.Add(-1)
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
		t := ps.runqT
		avail := int32(coroPRunqSize - (t - h))
		for stored < avail && head != nil {
			cur := head
			head = cur.next
			cur.next = nil
			ps.runq[(t+uint32(stored))&(coroPRunqSize-1)] = cur
			stored++
		}
		if stored > 0 {
			atomic.StoreRel(&ps.runqT, t+uint32(stored))
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
		coroSched.runqLen += rem
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
		if vps == nil {
			continue
		}
		for {
			h := atomic.LoadAcq(&vps.runqH)
			t := atomic.LoadAcq(&vps.runqT)
			n := t - h
			if n == 0 || n > coroPRunqSize {
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
				first := batch[0]
				coroSched.runnable.Add(-1)
				if take > 1 {
					var myps *coroPState
					mp, myps = coroEnsurePState(mp)
					if myps != nil {
						mh := atomic.LoadAcq(&myps.runqH)
						mt := myps.runqT
						avail := coroPRunqSize - (mt - mh)
						stored := uint32(0)
						for i := uint32(1); i < take && stored < avail; i++ {
							myps.runq[(mt+stored)&(coroPRunqSize-1)] = batch[i]
							stored++
						}
						if stored > 0 {
							atomic.StoreRel(&myps.runqT, mt+stored)
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
							coroSched.runqLen += ln
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
	if maxW <= 0 || coroSched.activeDriver.Load() >= gomaxprocs {
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
	return coroSched.runnable.Load() > 0 || coroNetpollHasReady()
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

func coroWakeRootOwner(root *coroTask) {
	slotPlus1 := root.rootSlot.Load()
	if slotPlus1 == 0 {
		return
	}
	idx := slotPlus1 - 1
	if idx >= coroTotalDrivers {
		return
	}
	gp := coroSched.idleDrivers[idx].Load()
	if gp == 0 || (*g)(unsafe.Pointer(gp)) == getg() {
		return
	}
	if coroSched.idleDrivers[idx].CompareAndSwap(gp, 0) {
		coroSched.idleCount.Add(-1)
		goready((*g)(unsafe.Pointer(gp)), 0)
	}
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
	coroSched.unhosted.Add(1)
	coroEnqueue(task, false)
	return coroSched.runnable.Load() >= 128
}

// coroAwaitFrame initiates a compiler-lowered structured call from cur to child.
// Both share the same logical goroutine; the driver switches directly to child.
func coroAwaitFrame(ctx, childFrame unsafe.Pointer, resume func(unsafe.Pointer) uint8) uint8 {
	cur := (*coroTask)(ctx)
	child := (*coroTask)(childFrame)
	child.resume = resume
	child.parent = cur
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
	var action uint8

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
			} else {
				task = coroDequeue()
			}
			if task == nil {
				if root != nil && root.state.Load() == coroStateDone {
					return true, nil, false
				}
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
		}

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
		if raceenabled {
			racerelease(unsafe.Pointer(task))
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
		if task.flags&coroFlagRoot == 0 {
			coroRecycle(task)
		}
		if isDefer {
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
		// Normal structured await child completed: resume parent directly!
		parent.state.Store(coroStateRunning)
		return parent
	}
	if task.flags&coroFlagUnhosted != 0 {
		coroSched.unhosted.Add(-1)
	}
	if task.flags&coroFlagRoot != 0 {
		if raceenabled {
			racerelease(unsafe.Pointer(task))
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
		coroSched.unhosted.Add(-1)
	}
	if task.flags&coroFlagRoot != 0 {
		if raceenabled {
			racerelease(unsafe.Pointer(task))
		}
		task.state.Swap(coroStateDone)
		coroWakeRootOwner(task)
		return nil
	}
	// Independent `go` goroutine terminated with unhandled panic or Goexit.
	task.state.Store(coroStateDone)
	if pb != nil && pb.active && !pb.goexit {
		val := pb.val
		coroRecycle(task)
		// Unhandled panic in independent goroutine crashes the process, matching Go spec.
		panic(val)
	}
	coroRecycle(task)
	return nil
}
