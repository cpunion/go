// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

const (
	stacklessCoroPRunqSize       = 256
	stacklessCoroPOpCacheSize    = 16
	stacklessCoroPFusedCacheSize = 32
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
	opCacheCount   uint8
	ioOpCacheCount uint8
	fusedCount     uint8
	fusedNextEvict uint8
	opCache        *stacklessCoroOperation
	ioOpCache      *stacklessCoroOperation
	fused          [stacklessCoroPFusedCacheSize]stacklessCoroPFusedFrame
	runqHead       uint32
	runqTail       uint32
	runqSched      atomic.Pointer[stacklessCoroScheduler]
	runq           [stacklessCoroPRunqSize]*stacklessCoroTask
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
func coroRunqGrab(ps *stacklessCoroPState, s *stacklessCoroScheduler, batch *[stacklessCoroPRunqSize / 2]*stacklessCoroTask, limit uint32) uint32 {
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
	var batch [stacklessCoroPRunqSize / 2]*stacklessCoroTask
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
