// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

const (
	stacklessCoroPRunqSize           = 512
	stacklessCoroPRunqStealBatch     = 128
	stacklessCoroPOpCacheSize        = 128
	stacklessCoroPFusedCacheSize     = 32
	stacklessCoroPGoroutineCacheSize = 128
	stacklessCoroPTypedBurstCap      = 256
	stacklessCoroPCacheTaskCap       = 256
	stacklessCoroPCacheStealBatch    = 64
	stacklessCoroPSpawnSiteCount     = 16
	// Limit per-P fused frame caching to small objects without a malloc header
	// so span.elemsize equals roundupsize(size, false) exactly.
	stacklessCoroPFusedMaxSize = 512
)

type stacklessCoroPFusedFrame struct {
	resume   *funcval
	frame    unsafe.Pointer
	elemSize uintptr
}

// stacklessCoroPState holds lazily allocated per-P multi-P coroutine caches
// and a work-stealable runnable ring buffer. Keeping only a single pointer on
// runtime.p avoids enlarging runtime.p or perturbing single-P cache layout.
// All idle storage is dropped under STW during GC mark termination.
type stacklessCoroPState struct {
	opCacheCount        uint8
	ioOpCacheCount      uint8
	fusedCount          uint8
	fusedNextEvict      uint8
	typedBurstCount     uint16
	goroutineFreeCount  uint16
	typedBurstGen       uint32
	opCache             *stacklessCoroOperation
	ioOpCache           *stacklessCoroOperation
	goroutineFree       *stacklessCoroGoroutine
	gUnhosted           atomic.Int64
	pendingTypedTask    atomic.Pointer[stacklessCoroTask]
	cacheTaskHead       uint32
	cacheTaskTail       uint32
	cacheTaskFrameSize  atomic.Uint32
	cacheTaskSched      atomic.Pointer[stacklessCoroScheduler]
	cacheTaskResume     atomic.Pointer[funcval]
	cacheTask           [stacklessCoroPCacheTaskCap]*stacklessCoroTask
	typedBurstResume    *funcval
	typedBurstTyp       *_type
	typedBurstFrameSize uintptr
	typedBurstAllocSize uintptr
	typedBurst          [stacklessCoroPTypedBurstCap]*stacklessCoroTask
	spawnSites          [stacklessCoroPSpawnSiteCount]stacklessCoroSpawnSite
	fused               [stacklessCoroPFusedCacheSize]stacklessCoroPFusedFrame
	runqHead            uint32
	runqTail            uint32
	runqSched           atomic.Pointer[stacklessCoroScheduler]
	runq                [stacklessCoroPRunqSize]*stacklessCoroTask
}

type stacklessCoroTaskLifecycleBytes struct {
	state        stacklessCoroTaskState
	resuming     bool
	readyPending bool
	terminal     stacklessCoroTerminalKind
}

func packTaskLifecycle(state stacklessCoroTaskState, resuming, readyPending bool, terminal stacklessCoroTerminalKind) uint32 {
	b := stacklessCoroTaskLifecycleBytes{
		state:        state,
		resuming:     resuming,
		readyPending: readyPending,
		terminal:     terminal,
	}
	return *(*uint32)(unsafe.Pointer(&b))
}

func unpackTaskLifecycle(word uint32) stacklessCoroTaskLifecycleBytes {
	return *(*stacklessCoroTaskLifecycleBytes)(unsafe.Pointer(&word))
}

var (
	taskWordWaitingResuming = packTaskLifecycle(stacklessCoroTaskWaiting, true, false, stacklessCoroTerminalNone)
	taskWordRunnable        = packTaskLifecycle(stacklessCoroTaskRunnable, false, false, stacklessCoroTerminalNone)
	taskWordRunning         = packTaskLifecycle(stacklessCoroTaskRunning, true, false, stacklessCoroTerminalNone)
	taskWordComplete        = packTaskLifecycle(stacklessCoroTaskComplete, false, false, stacklessCoroTerminalNone)
)

func init() {
	base := unsafe.Offsetof(stacklessCoroTask{}.state)
	if base%4 != 0 ||
		unsafe.Offsetof(stacklessCoroTask{}.resuming) != base+1 ||
		unsafe.Offsetof(stacklessCoroTask{}.readyPending) != base+2 ||
		unsafe.Offsetof(stacklessCoroTask{}.terminal) != base+3 {
		throw("runtime: invalid stackless coroutine task lifecycle layout")
	}
}

//go:nosplit
func (task *stacklessCoroTask) lifecycleWord() *uint32 {
	return (*uint32)(unsafe.Pointer(&task.state))
}

//go:nosplit
func (task *stacklessCoroTask) loadLifecycle() uint32 {
	return atomic.Load(task.lifecycleWord())
}

//go:nosplit
func (task *stacklessCoroTask) setLifecycle(word uint32) {
	atomic.Store(task.lifecycleWord(), word)
}

// parkWaitAtomic transitions a Waiting+resuming task to Waiting+idle.
// Returns false if a producer already set readyPending.
func (task *stacklessCoroTask) parkWaitAtomic() bool {
	p := task.lifecycleWord()
	for {
		old := atomic.Load(p)
		b := unpackTaskLifecycle(old)
		if b.state != stacklessCoroTaskWaiting || !b.resuming {
			throw("runtime: stackless coroutine wait without operation")
		}
		if b.readyPending {
			return false
		}
		next := packTaskLifecycle(stacklessCoroTaskWaiting, false, false, b.terminal)
		if atomic.Cas(p, old, next) {
			return true
		}
	}
}

// completeWaitAtomic synchronizes a producer completing an operation or child
// with the executor leaving the resume that initiated the wait.
// termUpdate controls the terminal field in the same atomic word:
// 0 preserves it, >0 sets stacklessCoroTerminalPanic, <0 clears it to None.
// Returns true if the task is still resuming and readyPending was set.
// Returns false if the task was idle and has been transitioned to Runnable.
func (task *stacklessCoroTask) completeWaitAtomic(termUpdate int) bool {
	p := task.lifecycleWord()
	for {
		old := atomic.Load(p)
		b := unpackTaskLifecycle(old)
		if b.state != stacklessCoroTaskWaiting || b.readyPending {
			throw("runtime: invalid stackless coroutine ready transition")
		}
		term := b.terminal
		if termUpdate > 0 {
			term = stacklessCoroTerminalPanic
		} else if termUpdate < 0 {
			term = stacklessCoroTerminalNone
		}
		if b.resuming {
			next := packTaskLifecycle(stacklessCoroTaskWaiting, true, true, term)
			if atomic.Cas(p, old, next) {
				return true
			}
			continue
		}
		next := packTaskLifecycle(stacklessCoroTaskRunnable, false, false, term)
		if atomic.Cas(p, old, next) {
			return false
		}
	}
}

// takeIdleWaitingAtomic atomically claims a suspended (non-resuming) Waiting
// task directly into Running+resuming. Returns false if the task is still
// resuming or already has readyPending.
func (task *stacklessCoroTask) takeIdleWaitingAtomic() bool {
	p := task.lifecycleWord()
	for {
		old := atomic.Load(p)
		b := unpackTaskLifecycle(old)
		if b.state != stacklessCoroTaskWaiting || b.resuming || b.readyPending {
			return false
		}
		next := packTaskLifecycle(stacklessCoroTaskRunning, true, false, b.terminal)
		if atomic.Cas(p, old, next) {
			return true
		}
	}
}

func (s *stacklessCoroScheduler) multiPFastPath() bool {
	return !raceenabled && gomaxprocs > 1 &&
		s.loadExecutorState() == stacklessCoroExecutorStateRunning &&
		!stacklessCoroIsPullComparison(s)
}

func (s *stacklessCoroScheduler) pRunqUsed() bool {
	return s.lifetime.Load()&stacklessCoroLifetimePRunqUsed != 0
}

func ensureCoroPState(mp *m) (*m, *stacklessCoroPState) {
	pp := mp.p.ptr()
	if pp == nil {
		return mp, nil
	}
	if ps := pp.coroP.Load(); ps != nil {
		return mp, ps
	}
	releasem(mp)
	allocated := new(stacklessCoroPState)
	mp = acquirem()
	pp = mp.p.ptr()
	if pp == nil {
		return mp, nil
	}
	if !pp.coroP.CompareAndSwap(nil, allocated) {
		return mp, pp.coroP.Load()
	}
	return mp, allocated
}

func coroRunqEligible(s *stacklessCoroScheduler, task *stacklessCoroTask) bool {
	if task == &s.root || task.parent() != nil {
		return false
	}
	r := task.source()
	return r != nil && r.inRegistry && !r.hosted && !r.public && r.scheduler == s
}

// coroRunqPut pushes an already-Runnable eligible task into the current P's
// work-stealable ring buffer. Returns (runnableState, true) on success.
func coroRunqPut(s *stacklessCoroScheduler, task *stacklessCoroTask) (uint32, bool) {
	mp := acquirem()
	var ps *stacklessCoroPState
	mp, ps = ensureCoroPState(mp)
	if ps == nil {
		releasem(mp)
		return 0, false
	}
	h := atomic.LoadAcq(&ps.runqHead)
	t := ps.runqTail
	if t-h >= stacklessCoroPRunqSize {
		releasem(mp)
		return 0, false
	}
	if t == h {
		ps.runqSched.Store(s)
	} else if ps.runqSched.Load() != s {
		releasem(mp)
		return 0, false
	}
	if s.lifetime.Load()&stacklessCoroLifetimePRunqUsed == 0 {
		s.lifetime.Or(stacklessCoroLifetimePRunqUsed)
	}
	ps.runq[t&(stacklessCoroPRunqSize-1)] = task
	// Increment runnableState before publishing runqTail so a concurrent
	// thief can never decrement runnableState below zero.
	runnable := s.runnableState.Add(1)
	atomic.StoreRel(&ps.runqTail, t+1)
	releasem(mp)
	return runnable, true
}

// coroRunqPutBatchNoCount moves already-counted Runnable tasks (e.g. transferred
// from s.head without decrementing runnableState) into the current P's runq.
// Returns the number of tasks placed in ps.runq.
func coroRunqPutBatchNoCount(s *stacklessCoroScheduler, tasks []*stacklessCoroTask) int {
	mp := acquirem()
	var ps *stacklessCoroPState
	mp, ps = ensureCoroPState(mp)
	if ps == nil {
		releasem(mp)
		return 0
	}
	h := atomic.LoadAcq(&ps.runqHead)
	t := ps.runqTail
	if t == h {
		ps.runqSched.Store(s)
	} else if ps.runqSched.Load() != s {
		releasem(mp)
		return 0
	}
	avail := stacklessCoroPRunqSize - (t - h)
	n := len(tasks)
	if uint32(n) > avail {
		n = int(avail)
	}
	for i := 0; i < n; i++ {
		ps.runq[(t+uint32(i))&(stacklessCoroPRunqSize-1)] = tasks[i]
	}
	if n > 0 {
		atomic.StoreRel(&ps.runqTail, t+uint32(n))
	}
	releasem(mp)
	return n
}

func coroRunqActivate(task *stacklessCoroTask) *stacklessCoroTask {
	b := unpackTaskLifecycle(task.loadLifecycle())
	if b.state != stacklessCoroTaskRunnable || b.resuming || b.readyPending {
		throw("runtime: dequeued non-runnable stackless coroutine")
	}
	task.setLifecycle(packTaskLifecycle(stacklessCoroTaskRunning, true, false, b.terminal))
	return task
}

// coroRunqGet pops one task belonging to s from the current P's ring buffer.
func coroRunqGet(s *stacklessCoroScheduler) *stacklessCoroTask {
	mp := acquirem()
	pp := mp.p.ptr()
	if pp == nil {
		releasem(mp)
		return nil
	}
	ps := pp.coroP.Load()
	if ps == nil || ps.runqSched.Load() != s {
		releasem(mp)
		return nil
	}
	t := ps.runqTail
	for {
		h := atomic.LoadAcq(&ps.runqHead)
		if t == h {
			releasem(mp)
			return nil
		}
		task := ps.runq[h&(stacklessCoroPRunqSize-1)]
		if atomic.CasRel(&ps.runqHead, h, h+1) {
			s.runnableState.Add(-1)
			releasem(mp)
			return coroRunqActivate(task)
		}
	}
}

// coroRunqGrab steals up to limit tasks belonging to s from victim ps into batch.
// runqSched is only modified by its owning P while h == t; verifying it after
// loading a non-empty [h, t) range guarantees that if CasRel(h, h+take) succeeds,
// every slot in [h, h+take) belonged to s throughout the read.
func coroRunqGrab(ps *stacklessCoroPState, s *stacklessCoroScheduler, batch *[stacklessCoroPRunqStealBatch]*stacklessCoroTask, limit uint32) uint32 {
	for {
		h := atomic.LoadAcq(&ps.runqHead)
		t := atomic.LoadAcq(&ps.runqTail)
		n := t - h
		if n == 0 || n > stacklessCoroPRunqSize {
			return 0
		}
		if ps.runqSched.Load() != s {
			return 0
		}
		take := (n + 1) / 2
		if take > limit {
			take = limit
		}
		for i := uint32(0); i < take; i++ {
			batch[i] = ps.runq[(h+i)&(stacklessCoroPRunqSize-1)]
		}
		if atomic.CasRel(&ps.runqHead, h, h+take) {
			return take
		}
	}
}

// coroRunqSteal steals up to half of another P's runnable tasks for s.
// Returns one task immediately and installs the rest into the current P's runq.
func coroRunqSteal(s *stacklessCoroScheduler) *stacklessCoroTask {
	mp := acquirem()
	myp := mp.p.ptr()
	nprocs := len(allp)
	if myp == nil || nprocs <= 1 {
		releasem(mp)
		return nil
	}
	myps := myp.coroP.Load()
	var batch [stacklessCoroPRunqStealBatch]*stacklessCoroTask
	start := (int(myp.id) + 1) % nprocs
	for offset := 0; offset < nprocs-1; offset++ {
		pp := allp[(start+offset)%nprocs]
		if pp == nil || pp == myp {
			continue
		}
		ps := pp.coroP.Load()
		if ps == nil || ps.runqSched.Load() != s {
			continue
		}
		limit := uint32(1)
		if myps != nil {
			mh := atomic.LoadAcq(&myps.runqHead)
			mt := myps.runqTail
			if mh == mt || myps.runqSched.Load() == s {
				avail := stacklessCoroPRunqSize - (mt - mh)
				if avail > 0 {
					limit = avail + 1
					if limit > uint32(len(batch)) {
						limit = uint32(len(batch))
					}
				}
			}
		}
		stolen := coroRunqGrab(ps, s, &batch, limit)
		if stolen == 0 {
			continue
		}
		if stolen > 1 && myps != nil {
			mt := myps.runqTail
			if atomic.LoadAcq(&myps.runqHead) == mt {
				myps.runqSched.Store(s)
			}
			for i := uint32(1); i < stolen; i++ {
				myps.runq[(mt+i-1)&(stacklessCoroPRunqSize-1)] = batch[i]
			}
			atomic.StoreRel(&myps.runqTail, mt+stolen-1)
		}
		s.runnableState.Add(-1)
		task := batch[0]
		releasem(mp)
		return coroRunqActivate(task)
	}
	releasem(mp)
	return nil
}

func coroPOpCacheGet() *stacklessCoroOperation {
	mp := acquirem()
	pp := mp.p.ptr()
	if pp == nil {
		releasem(mp)
		return nil
	}
	ps := pp.coroP.Load()
	if ps == nil || ps.opCache == nil {
		releasem(mp)
		return nil
	}
	op := ps.opCache
	ps.opCache = op.next
	ps.opCacheCount--
	op.next = nil
	releasem(mp)
	return op
}

func coroPOpCachePut(op *stacklessCoroOperation) bool {
	if raceenabled {
		return false
	}
	mp := acquirem()
	var ps *stacklessCoroPState
	mp, ps = ensureCoroPState(mp)
	if ps == nil || ps.opCacheCount >= stacklessCoroPOpCacheSize {
		releasem(mp)
		return false
	}
	op.next = ps.opCache
	ps.opCache = op
	ps.opCacheCount++
	releasem(mp)
	return true
}

func coroPIOOpCacheGet() *stacklessCoroOperation {
	mp := acquirem()
	pp := mp.p.ptr()
	if pp == nil {
		releasem(mp)
		return nil
	}
	ps := pp.coroP.Load()
	if ps == nil || ps.ioOpCache == nil {
		releasem(mp)
		return nil
	}
	op := ps.ioOpCache
	ps.ioOpCache = op.next
	ps.ioOpCacheCount--
	op.next = nil
	releasem(mp)
	return op
}

func coroPIOOpCachePut(op *stacklessCoroOperation) bool {
	if raceenabled {
		return false
	}
	mp := acquirem()
	var ps *stacklessCoroPState
	mp, ps = ensureCoroPState(mp)
	if ps == nil || ps.ioOpCacheCount >= stacklessCoroPOpCacheSize {
		releasem(mp)
		return false
	}
	op.next = ps.ioOpCache
	ps.ioOpCache = op
	ps.ioOpCacheCount++
	releasem(mp)
	return true
}

func coroPFusedFrameGet(resume *funcval, size uintptr) unsafe.Pointer {
	if size == 0 || size > stacklessCoroPFusedMaxSize {
		return nil
	}
	want := roundupsize(size, false)
	mp := acquirem()
	pp := mp.p.ptr()
	if pp == nil {
		releasem(mp)
		return nil
	}
	ps := pp.coroP.Load()
	if ps == nil || ps.fusedCount == 0 {
		releasem(mp)
		return nil
	}
	for i := int(ps.fusedCount) - 1; i >= 0; i-- {
		entry := ps.fused[i]
		if entry.resume == resume && entry.elemSize == want {
			last := int(ps.fusedCount) - 1
			if i != last {
				ps.fused[i] = ps.fused[last]
			}
			ps.fused[last] = stacklessCoroPFusedFrame{}
			ps.fusedCount = uint8(last)
			releasem(mp)
			return entry.frame
		}
	}
	releasem(mp)
	return nil
}

func coroPFusedFramePut(resume *funcval, frame unsafe.Pointer) {
	if raceenabled || resume == nil || frame == nil {
		return
	}
	span := spanOf(uintptr(frame))
	if span == nil || span.elemsize == 0 || span.elemsize > stacklessCoroPFusedMaxSize {
		return
	}
	mp := acquirem()
	var ps *stacklessCoroPState
	mp, ps = ensureCoroPState(mp)
	if ps == nil {
		releasem(mp)
		return
	}
	entry := stacklessCoroPFusedFrame{
		resume:   resume,
		frame:    frame,
		elemSize: span.elemsize,
	}
	if ps.fusedCount < stacklessCoroPFusedCacheSize {
		ps.fused[ps.fusedCount] = entry
		ps.fusedCount++
	} else {
		idx := ps.fusedNextEvict & (stacklessCoroPFusedCacheSize - 1)
		ps.fused[idx] = entry
		ps.fusedNextEvict = idx + 1
	}
	releasem(mp)
}

// drainStacklessCoroPState runs under STW when a P is destroyed by procresize.
// Any runnable tasks remaining in pp's local coroutine ring buffer are moved back
// onto their scheduler's shared queue before pp is detached from allp.
func drainStacklessCoroPState(pp *p) {
	assertWorldStopped()
	ps := pp.coroP.Load()
	if ps == nil {
		return
	}
	dropCoroPTypedBurst(ps)
	if delta := ps.gUnhosted.Swap(0); delta != 0 {
		stacklessCoroGoroutines.unhosted.Add(delta)
	}
	if ps.goroutineFree != nil {
		// Return any pooled records to lock-free freeChunks so they stay reusable.
		pushCoroPooledGChunk(ps.goroutineFree)
		ps.goroutineFree = nil
		ps.goroutineFreeCount = 0
	}
	if cs := ps.cacheTaskSched.Load(); cs != nil {
		h := ps.cacheTaskHead
		t := ps.cacheTaskTail
		if h != t {
			var bytes uint16
			for i := h; i != t; i++ {
				if task := ps.cacheTask[i&(stacklessCoroPCacheTaskCap-1)]; task != nil {
					bytes += task.frameSize
				}
			}
			keep := !cs.drained() && cs.cachedFrameBytes >= cs.freeFrameBytes+bytes
			for i := h; i != t; i++ {
				idx := i & (stacklessCoroPCacheTaskCap - 1)
				task := ps.cacheTask[idx]
				ps.cacheTask[idx] = nil
				if keep && task != nil {
					task.next = cs.freeTasks
					cs.freeTasks = task
				}
			}
			if keep {
				cs.freeFrameBytes += bytes
			}
		}
		ps.cacheTaskHead = 0
		ps.cacheTaskTail = 0
		ps.cacheTaskSched.Store(nil)
		ps.cacheTaskResume.Store(nil)
		ps.cacheTaskFrameSize.Store(0)
	}
	if pt := ps.pendingTypedTask.Load(); pt != nil {
		ps.pendingTypedTask.Store(nil)
		if parent := pt.parent(); parent != nil && parent.context.scheduler != nil {
			s := parent.context.scheduler
			pt.next = s.reservedTasks
			s.reservedTasks = pt
		}
	}
	if s := ps.runqSched.Load(); s != nil {
		for ps.runqHead != ps.runqTail {
			idx := ps.runqHead & (stacklessCoroPRunqSize - 1)
			task := ps.runq[idx]
			ps.runq[idx] = nil
			ps.runqHead++
			if task == nil {
				continue
			}
			task.next = nil
			if s.tail == nil {
				s.head = task
			} else {
				s.tail.next = task
			}
			s.tail = task
		}
	}
	pp.coroP.Store(nil)
}

// clearStacklessCoroPStates runs under STW during GC mark termination.
// Clears all per-P cached operations, fused frames, and idle runq slots;
// drops the entire lazy P-state allocation when its runq is empty so idle
// per-P storage retains zero bytes across GC.
func clearStacklessCoroPStates() {
	assertWorldStopped()
	for _, pp := range allp {
		if pp == nil {
			continue
		}
		ps := pp.coroP.Load()
		if ps == nil {
			continue
		}
		ps.opCache = nil
		ps.opCacheCount = 0
		ps.ioOpCache = nil
		ps.ioOpCacheCount = 0
		ps.goroutineFree = nil
		ps.goroutineFreeCount = 0
		if delta := ps.gUnhosted.Swap(0); delta != 0 {
			stacklessCoroGoroutines.unhosted.Add(delta)
		}
		if cs := ps.cacheTaskSched.Load(); cs != nil {
			h := ps.cacheTaskHead
			t := ps.cacheTaskTail
			if h != t {
				var bytes uint16
				for i := h; i != t; i++ {
					if task := ps.cacheTask[i&(stacklessCoroPCacheTaskCap-1)]; task != nil {
						bytes += task.frameSize
					}
				}
				keep := !cs.drained() && cs.cachedFrameBytes >= cs.freeFrameBytes+bytes
				for i := h; i != t; i++ {
					idx := i & (stacklessCoroPCacheTaskCap - 1)
					task := ps.cacheTask[idx]
					if keep && task != nil {
						task.next = cs.freeTasks
						cs.freeTasks = task
					}
				}
				if keep {
					cs.freeFrameBytes += bytes
				}
			}
			for i := range ps.cacheTask {
				ps.cacheTask[i] = nil
			}
			ps.cacheTaskHead = 0
			ps.cacheTaskTail = 0
			ps.cacheTaskSched.Store(nil)
			ps.cacheTaskResume.Store(nil)
			ps.cacheTaskFrameSize.Store(0)
		}
		if pt := ps.pendingTypedTask.Load(); pt != nil {
			ps.pendingTypedTask.Store(nil)
			if parent := pt.parent(); parent != nil && parent.context.scheduler != nil {
				s := parent.context.scheduler
				pt.next = s.reservedTasks
				s.reservedTasks = pt
			}
		}
		dropCoroPTypedBurst(ps)
		if ps.fusedCount != 0 {
			for i := range ps.fusedCount {
				ps.fused[i] = stacklessCoroPFusedFrame{}
			}
			ps.fusedCount = 0
			ps.fusedNextEvict = 0
		}
		h := ps.runqHead
		t := ps.runqTail
		if h == t {
			pp.coroP.Store(nil)
			continue
		}
		live := t - h
		if live >= stacklessCoroPRunqSize {
			continue
		}
		for offset := live; offset < stacklessCoroPRunqSize; offset++ {
			idx := (h + offset) & (stacklessCoroPRunqSize - 1)
			ps.runq[idx] = nil
		}
	}
}

//go:noinline
func (s *stacklessCoroScheduler) startOperationMultiP(task *stacklessCoroTask,
	kind stacklessCoroOperationKind) *stacklessCoroOperation {
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		task.terminal != stacklessCoroTerminalNone || task.goexit {
		throw("runtime: stackless coroutine operation outside resume")
	}
	task.setLifecycle(taskWordWaitingResuming)
	var op *stacklessCoroOperation
	if kind == stacklessCoroOperationIO {
		op = coroPIOOpCacheGet()
		if op == nil {
			op = takeStacklessCoroSharedOperation(kind)
			if op == nil {
				op = newStacklessCoroIOOperation()
			}
			op.ensureIOState()
		}
	} else {
		op = coroPOpCacheGet()
		if op == nil {
			op = takeStacklessCoroSharedOperation(kind)
			if op == nil {
				op = newStacklessCoroChannelOperation()
			}
		}
	}
	op.task = task
	return op
}

//go:noinline
func (s *stacklessCoroScheduler) completeOperationMultiP(op *stacklessCoroOperation,
	task *stacklessCoroTask, isIO bool) {
	early := task.completeWaitAtomic(0)
	var cached bool
	if isIO {
		cached = coroPIOOpCachePut(op)
	} else {
		cached = coroPOpCachePut(op)
	}
	if !cached {
		cacheStacklessCoroSharedOperation(op)
	}
	if !early {
		task.next = nil
		stacklessCoroSetLogicalLeaf(task, nil)
		queued := false
		if coroRunqEligible(s, task) {
			_, queued = coroRunqPut(s, task)
		}
		if !queued {
			lock(&s.lock)
			s.enqueueRunnableLocked(task)
			unlock(&s.lock)
		}
		s.signalIfWaiting()
	}
}

//go:noinline
func (s *stacklessCoroScheduler) readyLockedMultiP(task *stacklessCoroTask) uint32 {
	if task.state == stacklessCoroTaskWaiting {
		if task.completeWaitAtomic(0) {
			if raceenabled {
				racereleasemerge(unsafe.Pointer(task))
			}
			return 0
		}
		return s.enqueueRunnableLocked(task)
	}
	if task.resuming {
		throw("runtime: invalid stackless coroutine early ready transition")
	}
	switch task.state {
	case stacklessCoroTaskNew, stacklessCoroTaskRunning:
	default:
		throw("runtime: invalid stackless coroutine ready transition")
	}
	if task.readyPending {
		throw("runtime: stackless coroutine has pending ready transition")
	}
	task.setLifecycle(packTaskLifecycle(stacklessCoroTaskRunnable, false, false, task.terminal))
	return s.enqueueRunnableLocked(task)
}

//go:noinline
func (s *stacklessCoroScheduler) yieldTaskMultiP(task *stacklessCoroTask,
	allowDirect, takeNext, public bool) (next *stacklessCoroTask, resumeDirectly, foreignReturner, handled bool) {
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		task.terminal != stacklessCoroTerminalNone || task.goexit {
		throw("runtime: invalid stackless coroutine yield")
	}
	if allowDirect && !s.drained() && s.runnableState.Load() == 0 && coroRunqEligible(s, task) {
		return nil, true, false, true
	}
	if takeNext && !s.driverDone(public) {
		next = coroRunqGet(s)
		if next == nil && s.runnableState.Load()&^stacklessCoroForeignReturnerBit != 0 {
			next = coroRunqSteal(s)
		}
	}
	if next == nil {
		return nil, false, false, false
	}
	if coroRunqEligible(s, task) {
		task.setLifecycle(taskWordRunnable)
		task.next = nil
		stacklessCoroSetLogicalLeaf(task, nil)
		if runnable, ok := coroRunqPut(s, task); ok {
			foreignReturner = runnable&stacklessCoroForeignReturnerBit != 0
			if foreignReturner {
				next.setLifecycle(taskWordRunnable)
				next.next = nil
				stacklessCoroSetLogicalLeaf(next, nil)
				if _, ok2 := coroRunqPut(s, next); !ok2 {
					lock(&s.lock)
					s.enqueueRunnableLocked(next)
					unlock(&s.lock)
				}
				return nil, false, true, true
			}
			return next, false, false, true
		}
		lock(&s.lock)
		runnable := s.enqueueRunnableLocked(task)
		foreignReturner = runnable&stacklessCoroForeignReturnerBit != 0
		if foreignReturner {
			next.setLifecycle(taskWordRunnable)
			s.enqueueRunnableLocked(next)
			next = nil
		}
		unlock(&s.lock)
		return next, false, foreignReturner, true
	}
	lock(&s.lock)
	task.resuming = false
	runnable := s.readyLocked(task)
	foreignReturner = runnable&stacklessCoroForeignReturnerBit != 0
	if foreignReturner {
		next.setLifecycle(taskWordRunnable)
		s.enqueueRunnableLocked(next)
		next = nil
	}
	unlock(&s.lock)
	return next, false, foreignReturner, true
}

//go:noinline
func (s *stacklessCoroScheduler) waitingTaskMultiP(task *stacklessCoroTask,
	native, takeNext, public bool) (next *stacklessCoroTask, resumeDirectly, handled bool) {
	if task.parkWaitAtomic() {
		if takeNext && !s.driverDone(public) {
			next = s.take()
		}
		return next, false, true
	}
	if native && !s.drained() && s.runnableState.Load() == 0 && coroRunqEligible(s, task) {
		b := unpackTaskLifecycle(task.loadLifecycle())
		task.setLifecycle(packTaskLifecycle(stacklessCoroTaskRunning, true, false, b.terminal))
		return nil, true, true
	}
	return nil, false, false
}

func coroPPutPendingTypedTask(task *stacklessCoroTask) {
	mp := acquirem()
	var ps *stacklessCoroPState
	mp, ps = ensureCoroPState(mp)
	if ps != nil {
		ps.pendingTypedTask.Store(task)
	}
	releasem(mp)
}

func coroPTryPutPendingTypedTask(task *stacklessCoroTask) bool {
	mp := acquirem()
	var ps *stacklessCoroPState
	mp, ps = ensureCoroPState(mp)
	ok := ps != nil && ps.pendingTypedTask.CompareAndSwap(nil, task)
	releasem(mp)
	return ok
}

func coroPPeekPendingTypedTask() *stacklessCoroTask {
	mp := acquirem()
	var task *stacklessCoroTask
	if pp := mp.p.ptr(); pp != nil {
		if ps := pp.coroP.Load(); ps != nil {
			task = ps.pendingTypedTask.Load()
		}
	}
	releasem(mp)
	return task
}

func coroPTakePendingTypedTask(parent *stacklessCoroTask, frame unsafe.Pointer,
	child stacklessCoroResume) *stacklessCoroTask {
	mp := acquirem()
	pp := mp.p.ptr()
	if pp == nil {
		releasem(mp)
		return nil
	}
	ps := pp.coroP.Load()
	if ps == nil {
		releasem(mp)
		return nil
	}
	task := ps.pendingTypedTask.Load()
	if task == nil || task.context.frame != frame || task.parent() != parent ||
		stacklessCoroResumeIdentity(task.resume) != stacklessCoroResumeIdentity(child) {
		releasem(mp)
		return nil
	}
	if !ps.pendingTypedTask.CompareAndSwap(task, nil) {
		releasem(mp)
		return nil
	}
	releasem(mp)
	return task
}

// coroPTakePendingTypedTaskAnyP is the cold fallback when a goroutine migrated
// Ps between coroTakeTypedTaskFrame and coroSpawnFrame/coroAwaitFrame.
func coroPTakePendingTypedTaskAnyP(parent *stacklessCoroTask, frame unsafe.Pointer,
	child stacklessCoroResume) *stacklessCoroTask {
	if frame == nil || gomaxprocs <= 1 {
		return nil
	}
	wantResume := stacklessCoroResumeIdentity(child)
	mp := acquirem()
	for _, pp := range allp {
		if pp == nil {
			continue
		}
		ps := pp.coroP.Load()
		if ps == nil {
			continue
		}
		task := ps.pendingTypedTask.Load()
		if task != nil && task.context.frame == frame && task.parent() == parent &&
			stacklessCoroResumeIdentity(task.resume) == wantResume &&
			ps.pendingTypedTask.CompareAndSwap(task, nil) {
			releasem(mp)
			return task
		}
	}
	releasem(mp)
	return nil
}

// coroPCancelPendingTypedTask removes any unconsumed pending reservation owned
// by parent across all Ps during panic/Goexit unwinding.
func coroPCancelPendingTypedTask(parent *stacklessCoroTask) *stacklessCoroTask {
	mp := acquirem()
	for _, pp := range allp {
		if pp == nil {
			continue
		}
		ps := pp.coroP.Load()
		if ps == nil {
			continue
		}
		task := ps.pendingTypedTask.Load()
		if task != nil && task.parent() == parent &&
			ps.pendingTypedTask.CompareAndSwap(task, nil) {
			releasem(mp)
			return task
		}
	}
	releasem(mp)
	return nil
}

func coroPGoroutineGet() *stacklessCoroGoroutine {
	mp := acquirem()
	pp := mp.p.ptr()
	if pp == nil {
		releasem(mp)
		return nil
	}
	ps := pp.coroP.Load()
	if ps == nil || ps.goroutineFree == nil {
		releasem(mp)
		return nil
	}
	r := ps.goroutineFree
	ps.goroutineFree = r.next
	ps.goroutineFreeCount--
	r.next = nil
	r.shard = stacklessCoroPooledGTransit
	releasem(mp)
	return r
}

func coroPGoroutinePutBatch(head *stacklessCoroGoroutine) {
	if head == nil {
		return
	}
	mp := acquirem()
	var ps *stacklessCoroPState
	mp, ps = ensureCoroPState(mp)
	if ps != nil {
		for head != nil && ps.goroutineFreeCount < stacklessCoroPGoroutineCacheSize {
			next := head.next
			head.shard = stacklessCoroPooledGShard
			head.next = ps.goroutineFree
			ps.goroutineFree = head
			ps.goroutineFreeCount++
			head = next
		}
	}
	if head != nil {
		for cur := head; cur != nil; cur = cur.next {
			cur.shard = stacklessCoroPooledGShard
		}
		pushCoroPooledGChunk(head)
	}
	releasem(mp)
}

func coroPSpawnSiteLookup(callerPC uintptr) uintptr {
	mp := acquirem()
	pp := mp.p.ptr()
	if pp == nil {
		releasem(mp)
		return 0
	}
	ps := pp.coroP.Load()
	if ps == nil {
		releasem(mp)
		return 0
	}
	site := &ps.spawnSites[(callerPC/sys.PCQuantum)%uintptr(len(ps.spawnSites))]
	var sourcePC uintptr
	if site.returnPC == callerPC {
		sourcePC = site.sourcePC
	}
	releasem(mp)
	return sourcePC
}

func coroPSpawnSiteStore(callerPC, sourcePC uintptr) {
	mp := acquirem()
	var ps *stacklessCoroPState
	mp, ps = ensureCoroPState(mp)
	if ps != nil {
		site := &ps.spawnSites[(callerPC/sys.PCQuantum)%uintptr(len(ps.spawnSites))]
		*site = stacklessCoroSpawnSite{returnPC: callerPC, sourcePC: sourcePC}
	}
	releasem(mp)
}

func dropCoroPTypedBurst(ps *stacklessCoroPState) {
	if ps.typedBurstCount == 0 {
		ps.typedBurstResume = nil
		ps.typedBurstTyp = nil
		return
	}
	dropped := uintptr(ps.typedBurstCount) * ps.typedBurstAllocSize
	for i := uint16(0); i < ps.typedBurstCount; i++ {
		ps.typedBurst[i] = nil
	}
	ps.typedBurstCount = 0
	ps.typedBurstResume = nil
	ps.typedBurstTyp = nil
	ps.typedBurstFrameSize = 0
	ps.typedBurstAllocSize = 0
	stacklessCoroTypedBurst.totalBytes.Add(-dropped)
}

func coroPCacheTaskGrab(ps *stacklessCoroPState, s *stacklessCoroScheduler, fn *funcval, size uint32,
	batch *[stacklessCoroPCacheStealBatch]*stacklessCoroTask, limit uint32) uint32 {
	for {
		h := atomic.LoadAcq(&ps.cacheTaskHead)
		t := atomic.LoadAcq(&ps.cacheTaskTail)
		n := t - h
		if n == 0 || n > stacklessCoroPCacheTaskCap {
			return 0
		}
		if ps.cacheTaskSched.Load() != s || ps.cacheTaskResume.Load() != fn || ps.cacheTaskFrameSize.Load() != size {
			return 0
		}
		take := (n + 1) / 2
		if take > limit {
			take = limit
		}
		for i := uint32(0); i < take; i++ {
			batch[i] = ps.cacheTask[(h+i)&(stacklessCoroPCacheTaskCap-1)]
		}
		if atomic.CasRel(&ps.cacheTaskHead, h, h+take) {
			return take
		}
	}
}

func coroPHasCacheTasks(s *stacklessCoroScheduler) bool {
	mp := acquirem()
	for _, pp := range allp {
		if pp == nil {
			continue
		}
		ps := pp.coroP.Load()
		if ps == nil || ps.cacheTaskSched.Load() != s {
			continue
		}
		if atomic.LoadAcq(&ps.cacheTaskHead) != atomic.LoadAcq(&ps.cacheTaskTail) {
			releasem(mp)
			return true
		}
	}
	releasem(mp)
	return false
}

func coroPCacheTaskGet(s *stacklessCoroScheduler, resume stacklessCoroResume, size uint16) *stacklessCoroTask {
	fn := stacklessCoroResumeIdentity(resume)
	wantSize := uint32(size)
	mp := acquirem()
	myp := mp.p.ptr()
	if myp == nil {
		releasem(mp)
		return nil
	}
	myps := myp.coroP.Load()
	if myps != nil && myps.cacheTaskSched.Load() == s &&
		myps.cacheTaskResume.Load() == fn && myps.cacheTaskFrameSize.Load() == wantSize {
		t := myps.cacheTaskTail
		for {
			h := atomic.LoadAcq(&myps.cacheTaskHead)
			if t == h {
				break
			}
			task := myps.cacheTask[h&(stacklessCoroPCacheTaskCap-1)]
			if atomic.CasRel(&myps.cacheTaskHead, h, h+1) {
				releasem(mp)
				return task
			}
		}
	}
	nprocs := len(allp)
	if nprocs <= 1 {
		releasem(mp)
		return nil
	}
	var batch [stacklessCoroPCacheStealBatch]*stacklessCoroTask
	start := (int(myp.id) + 1) % nprocs
	for offset := 0; offset < nprocs-1; offset++ {
		pp := allp[(start+offset)%nprocs]
		if pp == nil || pp == myp {
			continue
		}
		vps := pp.coroP.Load()
		if vps == nil || vps.cacheTaskSched.Load() != s {
			continue
		}
		limit := uint32(1)
		if myps != nil {
			mh := atomic.LoadAcq(&myps.cacheTaskHead)
			mt := myps.cacheTaskTail
			if mh == mt || (myps.cacheTaskSched.Load() == s && myps.cacheTaskResume.Load() == fn && myps.cacheTaskFrameSize.Load() == wantSize) {
				avail := stacklessCoroPCacheTaskCap - (mt - mh)
				if avail > 0 {
					limit = avail + 1
					if limit > uint32(len(batch)) {
						limit = uint32(len(batch))
					}
				}
			}
		}
		stolen := coroPCacheTaskGrab(vps, s, fn, wantSize, &batch, limit)
		if stolen == 0 {
			continue
		}
		if stolen > 1 && myps != nil {
			mt := myps.cacheTaskTail
			if atomic.LoadAcq(&myps.cacheTaskHead) == mt {
				myps.cacheTaskSched.Store(s)
				myps.cacheTaskResume.Store(fn)
				myps.cacheTaskFrameSize.Store(wantSize)
			}
			for i := uint32(1); i < stolen; i++ {
				myps.cacheTask[(mt+i-1)&(stacklessCoroPCacheTaskCap-1)] = batch[i]
			}
			atomic.StoreRel(&myps.cacheTaskTail, mt+stolen-1)
		}
		task := batch[0]
		releasem(mp)
		return task
	}
	releasem(mp)
	return nil
}

func coroPCacheTaskPutBatch(s *stacklessCoroScheduler, resume stacklessCoroResume, size uint16, tasks []*stacklessCoroTask) {
	if len(tasks) == 0 {
		return
	}
	fn := stacklessCoroResumeIdentity(resume)
	wantSize := uint32(size)
	mp := acquirem()
	var ps *stacklessCoroPState
	mp, ps = ensureCoroPState(mp)
	stored := 0
	if ps != nil {
		h := atomic.LoadAcq(&ps.cacheTaskHead)
		t := ps.cacheTaskTail
		if h == t {
			ps.cacheTaskSched.Store(s)
			ps.cacheTaskResume.Store(fn)
			ps.cacheTaskFrameSize.Store(wantSize)
		}
		if ps.cacheTaskSched.Load() == s && ps.cacheTaskResume.Load() == fn && ps.cacheTaskFrameSize.Load() == wantSize {
			avail := int(stacklessCoroPCacheTaskCap - (t - h))
			n := len(tasks)
			if n > avail {
				n = avail
			}
			for i := 0; i < n; i++ {
				ps.cacheTask[(t+uint32(i))&(stacklessCoroPCacheTaskCap-1)] = tasks[i]
			}
			if n > 0 {
				atomic.StoreRel(&ps.cacheTaskTail, t+uint32(n))
				stored = n
			}
		}
	}
	releasem(mp)
	if stored < len(tasks) {
		var bytes uint16
		for i := stored; i < len(tasks); i++ {
			bytes += tasks[i].frameSize
		}
		lock(&s.lock)
		if !s.drained() && s.cachedFrameBytes >= s.freeFrameBytes+bytes {
			for i := stored; i < len(tasks); i++ {
				t := tasks[i]
				t.next = s.freeTasks
				s.freeTasks = t
			}
			s.freeFrameBytes += bytes
		}
		unlock(&s.lock)
	}
}

func coroPCacheTaskPut(s *stacklessCoroScheduler, task *stacklessCoroTask) {
	fn := stacklessCoroResumeIdentity(task.resume)
	wantSize := uint32(task.frameSize)
	mp := acquirem()
	var ps *stacklessCoroPState
	mp, ps = ensureCoroPState(mp)
	if ps == nil {
		releasem(mp)
		lock(&s.lock)
		if !s.drained() && s.cachedFrameBytes >= s.freeFrameBytes+task.frameSize {
			task.next = s.freeTasks
			s.freeTasks = task
			s.freeFrameBytes += task.frameSize
		}
		unlock(&s.lock)
		return
	}
	h := atomic.LoadAcq(&ps.cacheTaskHead)
	t := ps.cacheTaskTail
	if h == t {
		ps.cacheTaskSched.Store(s)
		ps.cacheTaskResume.Store(fn)
		ps.cacheTaskFrameSize.Store(wantSize)
		ps.cacheTask[t&(stacklessCoroPCacheTaskCap-1)] = task
		atomic.StoreRel(&ps.cacheTaskTail, t+1)
		releasem(mp)
		return
	}
	if ps.cacheTaskSched.Load() == s && ps.cacheTaskResume.Load() == fn &&
		ps.cacheTaskFrameSize.Load() == wantSize && t-h < stacklessCoroPCacheTaskCap {
		ps.cacheTask[t&(stacklessCoroPCacheTaskCap-1)] = task
		atomic.StoreRel(&ps.cacheTaskTail, t+1)
		releasem(mp)
		return
	}
	// If the local ring belongs to a different scheduler, drain it first so this
	// P switches cleanly to s.
	var oldSched *stacklessCoroScheduler
	var oldHead *stacklessCoroTask
	var oldBytes uint16
	if curSched := ps.cacheTaskSched.Load(); curSched != nil && curSched != s {
		for {
			ch := atomic.LoadAcq(&ps.cacheTaskHead)
			if ch == t {
				break
			}
			item := ps.cacheTask[ch&(stacklessCoroPCacheTaskCap-1)]
			if atomic.CasRel(&ps.cacheTaskHead, ch, ch+1) {
				if item != nil {
					item.next = oldHead
					oldHead = item
					oldBytes += item.frameSize
				}
			}
		}
		oldSched = curSched
		ps.cacheTaskSched.Store(s)
		ps.cacheTaskResume.Store(fn)
		ps.cacheTaskFrameSize.Store(wantSize)
		ps.cacheTask[t&(stacklessCoroPCacheTaskCap-1)] = task
		atomic.StoreRel(&ps.cacheTaskTail, t+1)
		releasem(mp)
		if oldHead != nil && !oldSched.drained() {
			lock(&oldSched.lock)
			if !oldSched.drained() && oldSched.cachedFrameBytes >= oldSched.freeFrameBytes+oldBytes {
				for oldHead != nil {
					next := oldHead.next
					oldHead.next = oldSched.freeTasks
					oldSched.freeTasks = oldHead
					oldHead = next
				}
				oldSched.freeFrameBytes += oldBytes
			}
			unlock(&oldSched.lock)
		}
		return
	}
	releasem(mp)
	lock(&s.lock)
	if !s.drained() && s.cachedFrameBytes >= s.freeFrameBytes+task.frameSize {
		task.next = s.freeTasks
		s.freeTasks = task
		s.freeFrameBytes += task.frameSize
	}
	unlock(&s.lock)
}

func coroPFlushCacheTasks(s *stacklessCoroScheduler) {
	mp := acquirem()
	pp := mp.p.ptr()
	if pp == nil {
		releasem(mp)
		return
	}
	ps := pp.coroP.Load()
	if ps == nil {
		releasem(mp)
		return
	}
	flushSched := ps.cacheTaskSched.Load()
	if flushSched == nil || (s != nil && flushSched != s) {
		releasem(mp)
		return
	}
	t := ps.cacheTaskTail
	if atomic.LoadAcq(&ps.cacheTaskHead) == t {
		releasem(mp)
		return
	}
	var flushHead *stacklessCoroTask
	var flushBytes uint16
	keep := !flushSched.drained()
	for {
		h := atomic.LoadAcq(&ps.cacheTaskHead)
		if h == t {
			break
		}
		item := ps.cacheTask[h&(stacklessCoroPCacheTaskCap-1)]
		if atomic.CasRel(&ps.cacheTaskHead, h, h+1) {
			if keep && item != nil {
				item.next = flushHead
				flushHead = item
				flushBytes += item.frameSize
			}
		}
	}
	releasem(mp)
	if flushHead != nil && !flushSched.drained() {
		lock(&flushSched.lock)
		if !flushSched.drained() && flushSched.cachedFrameBytes >= flushSched.freeFrameBytes+flushBytes {
			for flushHead != nil {
				next := flushHead.next
				flushHead.next = flushSched.freeTasks
				flushSched.freeTasks = flushHead
				flushHead = next
			}
			flushSched.freeFrameBytes += flushBytes
		}
		unlock(&flushSched.lock)
	}
}
