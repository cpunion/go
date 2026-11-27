// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

const (
	coroPollErrorTag       = ^uintptr(0) ^ (^uintptr(0) >> 1)
	coroWriteNoProgressTag = coroPollErrorTag >> 1
	coroMaxRW              = 1 << 30
	coroIOAnchorShards     = 16
	coroBlockingWorkersMax = 16
)

// coroIOState holds lazy per-task I/O, timer, or select state. Pure CPU and
// single-channel send/recv tasks never allocate a coroIOState.
type coroIOState struct {
	// Direct netpoll socket/pipe I/O fields.
	pd        *pollDesc
	fd        int32
	mode      int32 // 'r' or 'w'
	buf       []byte
	written   int
	nOut      *int
	statusOut *uintptr
	readyNext uintptr // lock-free uintptr Treiber stack link (zero write barrier!)

	// GC anchor links (keeps task alive while parked in non-GC pollDesc.rg/wg).
	anchorShard uint8
	anchored    bool
	anchorPrev  *coroTask
	anchorNext  *coroTask

	// Channel comma-ok destination.
	chanOK *bool

	// Native timer for coroSleep.
	sleepTimer *timer

	// Multi-case select scratch.
	selectCases     []scase
	selectLockOrder []uint16
	selectSGList    *sudog
	selectWinner    *sudog
	selectNSends    int
	selectCase      int
	selectOK        bool
	selectChosen    *int
	selectOKOut     *bool
}

func (task *coroTask) ensureIOState() *coroIOState {
	if task.ioState == nil {
		task.ioState = new(coroIOState)
	}
	return task.ioState
}

// Sharded GC anchor table so tasks parked in non-GC pollDesc.{rg,wg} remain
// reachable to the GC without any global contention.
var coroIOAnchors [coroIOAnchorShards]struct {
	lock mutex
	head *coroTask
	_    [64]byte
}

// Lock-free uintptr Treiber stack of tasks woken directly by native netpollunblock.
var coroNetpollReady atomic.Uintptr

func init() {
	for i := range coroIOAnchors {
		lockInit(&coroIOAnchors[i].lock, lockRankCoroSched)
	}
	lockInit(&coroBlockPool.lock, lockRankCoroSched)
}

func coroIOAnchor(task *coroTask, io *coroIOState) {
	if io.anchored {
		return
	}
	shard := uint8((uintptr(unsafe.Pointer(task)) >> 7) & (coroIOAnchorShards - 1))
	io.anchorShard = shard
	sh := &coroIOAnchors[shard]
	lock(&sh.lock)
	io.anchorPrev = nil
	io.anchorNext = sh.head
	if sh.head != nil && sh.head.ioState != nil {
		sh.head.ioState.anchorPrev = task
	}
	sh.head = task
	io.anchored = true
	unlock(&sh.lock)
}

func coroIOUnanchor(task *coroTask, io *coroIOState) {
	if !io.anchored {
		return
	}
	sh := &coroIOAnchors[io.anchorShard]
	lock(&sh.lock)
	if io.anchored {
		if io.anchorPrev != nil {
			io.anchorPrev.ioState.anchorNext = io.anchorNext
		} else {
			sh.head = io.anchorNext
		}
		if io.anchorNext != nil && io.anchorNext.ioState != nil {
			io.anchorNext.ioState.anchorPrev = io.anchorPrev
		}
		io.anchorPrev = nil
		io.anchorNext = nil
		io.anchored = false
	}
	unlock(&sh.lock)
}

func coroNetpollHasReady() bool {
	return coroNetpollReady.Load() != 0
}

// coroNetpollWake is called directly from netpollunblock (//go:nowritebarrier!)
// when native epoll/kqueue marks a coroutine's pollDesc ready, closed, or timed out.
// It pushes the task onto the lock-free uintptr ready stack and returns an idle
// coroutine driver *g for immediate inclusion in netpoll's toRun gList.
//
//go:nowritebarrier
func coroNetpollWake(taskPtr uintptr) *g {
	task := (*coroTask)(unsafe.Pointer(taskPtr))
	io := task.ioState
	for {
		head := coroNetpollReady.Load()
		io.readyNext = head
		if coroNetpollReady.CompareAndSwap(head, taskPtr) {
			break
		}
	}
	return coroPopIdleDriver()
}

// coroDrainNetpollOne pops one task woken by native netpoll and claims it
// directly into Running on this driver (zero runq hop when already Parked!).
func coroDrainNetpollOne() *coroTask {
	for {
		head := coroNetpollReady.Load()
		if head == 0 {
			return nil
		}
		task := (*coroTask)(unsafe.Pointer(head))
		next := task.ioState.readyNext
		if !coroNetpollReady.CompareAndSwap(head, next) {
			continue
		}
		task.ioState.readyNext = 0
		// Claim directly if already Parked; otherwise set ReadyPending for active driver.
		for {
			s := task.state.Load()
			if s == coroStateParked {
				if task.state.CompareAndSwap(coroStateParked, coroStateRunning) {
					if raceenabled {
						raceacquire(unsafe.Pointer(task))
					}
					// If more netpoll completions remain, nudge a peer driver.
					if next != 0 {
						coroMaybeWakeDriver()
					}
					return task
				}
				continue
			}
			if s == coroStateWaiting {
				if task.state.CompareAndSwap(coroStateWaiting, coroStateWaiting|coroFlagReadyPending) {
					break
				}
				continue
			}
			throw("runtime: invalid coroDrainNetpoll state")
		}
	}
}

func coroRawRead(fd int32, buf []byte) int32 {
	if len(buf) == 0 {
		return 0
	}
	n := len(buf)
	if n > coroMaxRW {
		n = coroMaxRW
	}
	for {
		r := read(fd, unsafe.Pointer(&buf[0]), int32(n))
		if r != -_EINTR {
			KeepAlive(buf)
			return r
		}
	}
}

func coroRawWrite(fd int32, buf []byte) int32 {
	var p unsafe.Pointer
	n := len(buf)
	if n > coroMaxRW {
		n = coroMaxRW
	}
	if n > 0 {
		p = unsafe.Pointer(&buf[0])
	}
	for {
		r := write(uintptr(fd), p, int32(n))
		if r != -_EINTR {
			KeepAlive(buf)
			return r
		}
	}
}

func coroPrepareRecycleIO(task *coroTask) {
	if io := task.ioState; io != nil {
		if io.anchored {
			coroIOUnanchor(task, io)
		}
		*io = coroIOState{}
	}
}

// coroArmNetpoll installs task|1 into pd.rg or pd.wg. Returns (parked, pollErr).
func coroArmNetpoll(task *coroTask, io *coroIOState) (bool, int) {
	pd := io.pd
	mode := io.mode
	gpp := &pd.rg
	if mode == 'w' {
		gpp = &pd.wg
	}
	if gpp.CompareAndSwap(pdReady, pdNil) {
		return false, pollNoError
	}
	coroIOAnchor(task, io)
	task.flags |= coroFlagIOPending
	task.state.Store(coroStateWaiting)
	if raceenabled {
		racerelease(unsafe.Pointer(task))
	}
	tag := uintptr(unsafe.Pointer(task)) | 1
	for {
		if gpp.CompareAndSwap(pdReady, pdNil) {
			// Edge arrived concurrently; unanchor and retry syscall immediately.
			coroIOUnanchor(task, io)
			task.flags &^= coroFlagIOPending
			task.state.Store(coroStateRunning)
			return false, pollNoError
		}
		if gpp.CompareAndSwap(pdNil, tag) {
			netpollAdjustWaiters(1)
			break
		}
		if v := gpp.Load(); v != pdReady && v != pdNil {
			throw("runtime: double wait on pollDesc")
		}
	}
	if errCode := netpollcheckerr(pd, mode); errCode != pollNoError {
		if gpp.CompareAndSwap(tag, pdNil) {
			netpollAdjustWaiters(-1)
			coroIOUnanchor(task, io)
			task.flags &^= coroFlagIOPending
			task.state.Store(coroStateRunning)
			return false, errCode
		}
		// netpollunblock already claimed tag and pushed task to coroNetpollReady.
	}
	return true, pollNoError
}

// coroCompleteIO runs on the driver exclusively owning task after netpoll wake.
// Returns true if EAGAIN required re-arming and parking again.
func coroCompleteIO(task *coroTask) bool {
	io := task.ioState
	pd := io.pd
	mode := io.mode
	for {
		if errCode := netpollcheckerr(pd, mode); errCode != pollNoError {
			if mode == 'w' {
				*io.nOut = io.written
			} else {
				*io.nOut = 0
			}
			*io.statusOut = coroPollErrorTag | uintptr(errCode)
			coroIOUnanchor(task, io)
			task.flags &^= coroFlagIOPending
			io.buf = nil
			return false
		}
		if mode == 'r' {
			r := coroRawRead(io.fd, io.buf)
			if r == -_EAGAIN {
				parked, errCode := coroArmNetpoll(task, io)
				if errCode != pollNoError {
					*io.nOut = 0
					*io.statusOut = coroPollErrorTag | uintptr(errCode)
					io.buf = nil
					return false
				}
				if parked {
					return true
				}
				continue
			}
			if r < 0 {
				*io.nOut = -1
				*io.statusOut = uintptr(-r)
			} else {
				*io.nOut = int(r)
				*io.statusOut = 0
			}
			coroIOUnanchor(task, io)
			task.flags &^= coroFlagIOPending
			io.buf = nil
			return false
		}
		// Stream write loop.
		for {
			r := coroRawWrite(io.fd, io.buf[io.written:])
			if r == -_EAGAIN {
				parked, errCode := coroArmNetpoll(task, io)
				if errCode != pollNoError {
					*io.nOut = io.written
					*io.statusOut = coroPollErrorTag | uintptr(errCode)
					io.buf = nil
					return false
				}
				if parked {
					return true
				}
				break // retry immediately
			}
			if r < 0 {
				*io.nOut = io.written
				*io.statusOut = uintptr(-r)
				coroIOUnanchor(task, io)
				task.flags &^= coroFlagIOPending
				io.buf = nil
				return false
			}
			io.written += int(r)
			if io.written >= len(io.buf) {
				*io.nOut = io.written
				*io.statusOut = 0
				coroIOUnanchor(task, io)
				task.flags &^= coroFlagIOPending
				io.buf = nil
				return false
			}
			if r == 0 {
				*io.nOut = io.written
				*io.statusOut = coroWriteNoProgressTag
				coroIOUnanchor(task, io)
				task.flags &^= coroFlagIOPending
				io.buf = nil
				return false
			}
		}
	}
}

//go:linkname poll_runtime_coroSocketRead internal/poll.runtime_coroSocketRead
func poll_runtime_coroSocketRead(ctx unsafe.Pointer, pdPtr uintptr, fd int, p []byte, n *int, status *uintptr) bool {
	task := (*coroTask)(ctx)
	pd := (*pollDesc)(unsafe.Pointer(pdPtr))
	if errCode := netpollcheckerr(pd, 'r'); errCode != pollNoError {
		*n = 0
		*status = coroPollErrorTag | uintptr(errCode)
		return false
	}
	for {
		r := coroRawRead(int32(fd), p)
		if r != -_EAGAIN {
			if r < 0 {
				*n = -1
				*status = uintptr(-r)
			} else {
				*n = int(r)
				*status = 0
			}
			return false
		}
		io := task.ensureIOState()
		io.pd = pd
		io.fd = int32(fd)
		io.mode = 'r'
		io.buf = p
		io.nOut = n
		io.statusOut = status
		parked, errCode := coroArmNetpoll(task, io)
		if errCode != pollNoError {
			*n = 0
			*status = coroPollErrorTag | uintptr(errCode)
			io.buf = nil
			return false
		}
		if parked {
			return true
		}
	}
}

//go:linkname poll_runtime_coroSocketWrite internal/poll.runtime_coroSocketWrite
func poll_runtime_coroSocketWrite(ctx unsafe.Pointer, pdPtr uintptr, fd int, p []byte, n *int, status *uintptr) bool {
	task := (*coroTask)(ctx)
	pd := (*pollDesc)(unsafe.Pointer(pdPtr))
	if errCode := netpollcheckerr(pd, 'w'); errCode != pollNoError {
		*n = 0
		*status = coroPollErrorTag | uintptr(errCode)
		return false
	}
	written := 0
	for {
		r := coroRawWrite(int32(fd), p[written:])
		if r == -_EAGAIN {
			io := task.ensureIOState()
			io.pd = pd
			io.fd = int32(fd)
			io.mode = 'w'
			io.buf = p
			io.written = written
			io.nOut = n
			io.statusOut = status
			parked, errCode := coroArmNetpoll(task, io)
			if errCode != pollNoError {
				*n = written
				*status = coroPollErrorTag | uintptr(errCode)
				io.buf = nil
				return false
			}
			if parked {
				return true
			}
			continue
		}
		if r < 0 {
			*n = written
			*status = uintptr(-r)
			return false
		}
		written += int(r)
		if written >= len(p) {
			*n = written
			*status = 0
			return false
		}
		if r == 0 {
			*n = written
			*status = coroWriteNoProgressTag
			return false
		}
	}
}

// Blocking file I/O & fallback worker pool (used only for non-pollable files
// or contended FD locks).
type coroBlockJob struct {
	next *coroBlockJob
	task *coroTask
	fn   func()
}

var coroBlockPool struct {
	lock    mutex
	head    *coroBlockJob
	tail    *coroBlockJob
	workers int32
	wake    chan struct{}
}

func coroEnqueueBlockJob(task *coroTask, fn func()) {
	job := &coroBlockJob{task: task, fn: fn}
	task.state.Store(coroStateWaiting)
	if raceenabled {
		racerelease(unsafe.Pointer(task))
	}
	lock(&coroBlockPool.lock)
	if coroBlockPool.wake == nil {
		unlock(&coroBlockPool.lock)
		ch := make(chan struct{}, coroBlockingWorkersMax)
		lock(&coroBlockPool.lock)
		if coroBlockPool.wake == nil {
			coroBlockPool.wake = ch
		}
	}
	if coroBlockPool.tail == nil {
		coroBlockPool.head = job
	} else {
		coroBlockPool.tail.next = job
	}
	coroBlockPool.tail = job
	spawn := coroBlockPool.workers < coroBlockingWorkersMax
	if spawn {
		coroBlockPool.workers++
	}
	wake := coroBlockPool.wake
	unlock(&coroBlockPool.lock)
	if spawn {
		go coroBlockWorkerLoop(wake)
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

func coroBlockWorkerLoop(wake chan struct{}) {
	for {
		lock(&coroBlockPool.lock)
		job := coroBlockPool.head
		if job != nil {
			coroBlockPool.head = job.next
			if coroBlockPool.head == nil {
				coroBlockPool.tail = nil
			}
		}
		unlock(&coroBlockPool.lock)
		if job == nil {
			<-wake
			continue
		}
		if raceenabled {
			raceacquire(unsafe.Pointer(job.task))
		}
		job.fn()
		if raceenabled {
			racerelease(unsafe.Pointer(job.task))
		}
		coroReady(job.task)
	}
}

//go:linkname poll_runtime_coroFileRead internal/poll.runtime_coroFileRead
func poll_runtime_coroFileRead(ctx unsafe.Pointer, fd int, p []byte) (int, uintptr) {
	// Ordinary blocking syscall on caller G (managed-coro runs on real Go stack).
	entersyscall()
	r := coroRawRead(int32(fd), p)
	exitsyscall()
	if r < 0 {
		return -1, uintptr(-r)
	}
	return int(r), 0
}

//go:linkname poll_runtime_coroFileWrite internal/poll.runtime_coroFileWrite
func poll_runtime_coroFileWrite(ctx unsafe.Pointer, fd int, p []byte) (int, uintptr) {
	written := 0
	for {
		entersyscall()
		r := coroRawWrite(int32(fd), p[written:])
		exitsyscall()
		if r < 0 {
			return written, uintptr(-r)
		}
		written += int(r)
		if written >= len(p) {
			return written, 0
		}
		if r == 0 {
			return written, coroWriteNoProgressTag
		}
	}
}

//go:linkname poll_runtime_coroCallRead internal/poll.runtime_coroCallRead
func poll_runtime_coroCallRead(ctx unsafe.Pointer, call func()) {
	task := (*coroTask)(ctx)
	coroEnqueueBlockJob(task, call)
}

// coroSleep suspends the current coroutine for ns nanoseconds using Go's native timer.
func coroSleep(ctx unsafe.Pointer, ns int64) bool {
	if ns <= 0 {
		return false
	}
	task := (*coroTask)(ctx)
	when := nanotime() + ns
	if when < 0 {
		when = maxWhen
	}
	io := task.ensureIOState()
	t := io.sleepTimer
	if t == nil {
		t = new(timer)
		io.sleepTimer = t
	}
	task.state.Store(coroStateWaiting)
	if raceenabled {
		racerelease(unsafe.Pointer(task))
	}
	t.init(coroTimerWake, task)
	t.modify(when, 0, coroTimerWake, task, 0)
	return true
}

func coroTimerWake(arg any, seq uintptr, delay int64) {
	task := arg.(*coroTask)
	coroReady(task)
}

func coroGC(ctx unsafe.Pointer) {
	GC()
}
