// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

var stacklessCoroNetpoll struct {
	started   atomic.Uint32
	head      atomic.Uintptr
	wake      atomic.Uintptr
	loopG     atomic.Pointer[g]
	drainLock mutex
	drained   atomic.Uintptr
}

func init() {
	lockInit(&stacklessCoroNetpoll.drainLock, lockRankStacklessCoro)
}

const (
	stacklessCoroPollDescWord = iota
	stacklessCoroPollNextWord
)

const stacklessCoroPollErrorTag = ^uintptr(0) ^ (^uintptr(0) >> 1)

func startStacklessCoroSocketRead(ctx unsafe.Pointer, fd int, buffer []byte, n *int, errno *uintptr) {
	op := newStacklessCoroSocketReadOperation(ctx, fd, buffer, n, errno)
	if len(buffer) == 0 {
		registerStacklessCoroOperation(op)
		stacklessCoroSocketReadFinish(op, 0, 0)
		return
	}

	netpollGenericInit()
	pd, openErr := poll_runtime_pollOpen(uintptr(fd))
	if openErr != 0 {
		registerStacklessCoroOperation(op)
		stacklessCoroSocketReadFinish(op, -1, uintptr(openErr))
		return
	}
	stacklessCoroSocketReadStart(op, pd, true)
}

//go:linkname poll_runtime_coroSocketRead internal/poll.runtime_coroSocketRead
func poll_runtime_coroSocketRead(ctx unsafe.Pointer, pd uintptr,
	fd int, buffer []byte, n *int, errno *uintptr) bool {
	if len(buffer) == 0 {
		*n, *errno = 0, 0
		return false
	}
	descriptor := (*pollDesc)(unsafe.Pointer(pd))
	if descriptor == nil {
		throw("runtime: nil stackless coroutine poll descriptor")
	}
	if resetErr := poll_runtime_pollReset(descriptor, 'r'); resetErr != pollNoError {
		*n, *errno = -1, stacklessCoroPollErrorTag|uintptr(resetErr)
		return false
	}
	count := stacklessCoroRead(int32(fd), unsafe.Pointer(&buffer[0]), stacklessCoroReadLength(len(buffer)), true)
	KeepAlive(buffer)
	if count != -_EAGAIN {
		*n, *errno = stacklessCoroReadResult(count)
		return false
	}
	// The read lock keeps the descriptor borrowed until the caller finishes.
	// Only a read that would block needs a registered operation.
	op := newStacklessCoroSocketReadOperation(ctx, fd, buffer, n, errno)
	registerStacklessCoroSocketRead(op, descriptor, false)
	if !stacklessCoroSocketReadArm(op, descriptor) {
		stacklessCoroSocketReadAttempt(op)
	}
	return true
}

func newStacklessCoroSocketReadOperation(ctx unsafe.Pointer, fd int,
	buffer []byte, n *int, errno *uintptr) *stacklessCoroOperation {
	op := stacklessCoroStartIOOperation(ctx, "read")
	op.resourceState().fd = int32(fd)
	op.resourceState().buffer = buffer
	op.resourceState().n = n
	op.resourceState().errno = errno
	return op
}

func stacklessCoroSocketReadStart(op *stacklessCoroOperation, pd *pollDesc, ownsPollDesc bool) {
	registerStacklessCoroSocketRead(op, pd, ownsPollDesc)
	stacklessCoroSocketReadAttempt(op)
}

func registerStacklessCoroSocketRead(op *stacklessCoroOperation, pd *pollDesc, ownsPollDesc bool) {
	op.resourceState().packet[stacklessCoroPollDescWord] = uint64(uintptr(unsafe.Pointer(pd)))
	op.resourceState().ownsPollDesc = ownsPollDesc
	registerStacklessCoroOperation(op)
}

func stacklessCoroSocketReadAttempt(op *stacklessCoroOperation) {
	pd := (*pollDesc)(unsafe.Pointer(uintptr(op.resourceState().packet[stacklessCoroPollDescWord])))
	if pd == nil || len(op.resourceState().buffer) == 0 {
		throw("runtime: invalid stackless coroutine socket read")
	}
	for {
		if resetErr := poll_runtime_pollReset(pd, 'r'); resetErr != pollNoError {
			stacklessCoroSocketReadPollFinish(op, resetErr)
			return
		}
		count := read(op.resourceState().fd, unsafe.Pointer(&op.resourceState().buffer[0]),
			stacklessCoroReadLength(len(op.resourceState().buffer)))
		KeepAlive(op.resourceState().buffer)
		if count == -_EINTR && !op.resourceState().ownsPollDesc {
			continue
		}
		if count != -_EAGAIN {
			var readErrno uintptr
			if count < 0 {
				readErrno = uintptr(-count)
			}
			stacklessCoroSocketReadFinish(op, count, readErrno)
			return
		}
		if stacklessCoroSocketReadArm(op, pd) {
			return
		}
	}
}

// stacklessCoroSocketReadArm reports whether the operation was armed or
// completed with an error. If readiness raced with arming, the caller retries.
func stacklessCoroSocketReadArm(op *stacklessCoroOperation, pd *pollDesc) bool {
	waiting, waitErr := netpollCoroReadArm(pd, op)
	if waitErr != pollNoError {
		stacklessCoroSocketReadPollFinish(op, waitErr)
		return true
	}
	if waiting {
		stacklessCoroNetpollInit()
	}
	return waiting
}

// stacklessCoroPollIOAtIdle claims one I/O operation that belongs to s and does
// not belong to either excluded task. The caller permanently excludes the task
// that just armed and rotates the second exclusion between bounded attempts so
// multiple older waiters get a chance. This lets readiness produced by another
// task in the same scheduler complete without a locked-M round trip. An
// unavailable operation is rearmed before this function returns.
func stacklessCoroPollIOAtIdle(s *stacklessCoroScheduler,
	exclude, previous *stacklessCoroTask) *stacklessCoroTask {
	lock(&stacklessCoroOperations.lock)
	registry := stacklessCoroOperations.registry
	scanned := 0
	start := int(registry.scan)
	for offset := range len(registry.buckets) {
		index := (start + offset) % len(registry.buckets)
		for op := registry.buckets[index]; op != nil; op = op.next {
			if scanned == stacklessCoroIdlePollScanLimit {
				registry.scan = uint16((index + 1) % len(registry.buckets))
				unlock(&stacklessCoroOperations.lock)
				return nil
			}
			scanned++
			if !op.registeredIO || op.scheduler() != s || op.task == exclude ||
				op.task == previous || op.resourceState().async ||
				op.resourceState().packet[stacklessCoroPollDescWord] == 0 {
				continue
			}
			pd := (*pollDesc)(unsafe.Pointer(
				uintptr(op.resourceState().packet[stacklessCoroPollDescWord])))
			mode := int32('r')
			if op.resourceState().pollWrite {
				mode = 'w'
			}
			if !netpollCoroClaim(pd, op, mode) {
				continue
			}
			task := op.task
			registry.scan = uint16((index + 1) % len(registry.buckets))
			unlock(&stacklessCoroOperations.lock)
			stacklessCoroSocketAttempt(op)
			return task
		}
	}
	registry.scan = uint16((start + 1) % len(registry.buckets))
	unlock(&stacklessCoroOperations.lock)
	return nil
}

//go:noinline
func stacklessCoroPollIOBeforePark(s *stacklessCoroScheduler,
	exclude *stacklessCoroTask) *stacklessCoroTask {
	if stacklessCoroOperations.ioCount.Load() == 0 {
		return nil
	}
	if task := stacklessCoroDrainReadyIO(s); task != nil {
		return task
	}
	if gomaxprocs > 1 {
		wokeLoop := stacklessCoroNetpollPollNonblocking()
		task := stacklessCoroDrainReadyIO(s)
		if wokeLoop {
			stacklessCoroNetpollReleaseLoop()
		}
		if task != nil {
			return task
		}
		return s.take()
	}
	var previous *stacklessCoroTask
	for attempt := 0; attempt < stacklessCoroIdlePollAttempts; attempt++ {
		claimed := stacklessCoroPollIOAtIdle(s, exclude, previous)
		if claimed == nil && previous != nil {
			// Every other waiter had a chance. Start another bounded pass
			// while continuing to exclude the task that just armed.
			previous = nil
			claimed = stacklessCoroPollIOAtIdle(s, exclude, nil)
		}
		if claimed == nil {
			return nil
		}
		// A successful retry may have made its waiter runnable. Take it
		// before another bounded retry; if the operation remains unavailable,
		// the scheduler parks after the final attempt.
		if task := s.take(); task != nil {
			return task
		}
		previous = claimed
		if attempt+1 < stacklessCoroIdlePollAttempts {
			procyield(30)
		}
	}
	return nil
}

// stacklessCoroPollIOForReplacement checks for ready I/O before an idle
// replacement executor parks without entering a native stack episode.
func stacklessCoroPollIOForReplacement(s *stacklessCoroScheduler) bool {
	if stacklessCoroOperations.ioCount.Load() == 0 {
		return false
	}
	if stacklessCoroDrainReadyIOForReplacement(s) {
		return true
	}
	if gomaxprocs == 1 {
		return stacklessCoroPollIOAtIdle(s, nil, nil) != nil &&
			s.runnableState.Load()&^stacklessCoroForeignReturnerBit != 0
	}
	wokeLoop := stacklessCoroNetpollPollNonblocking()
	ready := stacklessCoroDrainReadyIOForReplacement(s)
	if wokeLoop {
		stacklessCoroNetpollReleaseLoop()
	}
	return ready
}

// stacklessCoroNetpollPollNonblocking performs a single-flight nonblocking
// netpoll(0) on the system stack, sharing sched.pollingNet with findRunnable.
// Ready coroutine operations are published to stacklessCoroNetpoll.head by
// netpollCoroDispatch; any ready ordinary goroutines are injected into the
// Go scheduler. If netpoll(0) returned the parked coroutine completion G, it
// withholds readying that G and returns true so the caller can drain the
// newly arrived completions inline and re-arm the parked G without an extra
// wakeup when nothing remains.
func stacklessCoroNetpollPollNonblocking() bool {
	if !netpollinited() || !netpollAnyWaiters() || sched.lastpoll.Load() == 0 {
		return false
	}
	if sched.pollingNet.Swap(1) != 0 {
		return false
	}
	loopG := stacklessCoroNetpoll.loopG.Load()
	wokeLoop := false
	systemstack(func() {
		list, delta := netpoll(0)
		sched.pollingNet.Store(0)
		if !list.empty() {
			var other gList
			for !list.empty() {
				gp := list.pop()
				if loopG != nil && gp == loopG {
					wokeLoop = true
					continue
				}
				other.push(gp)
			}
			if !other.empty() {
				injectglist(&other)
			}
			if delta != 0 {
				netpollAdjustWaiters(delta)
			}
		}
	})
	return wokeLoop
}

// stacklessCoroNetpollReleaseLoop either returns the withheld parked
// completion G to stacklessCoroNetpoll.wake when all completions have already
// been drained, or readies it if more completions remain or raced in.
func stacklessCoroNetpollReleaseLoop() {
	loopG := stacklessCoroNetpoll.loopG.Load()
	lock(&stacklessCoroNetpoll.drainLock)
	if !stacklessCoroNetpoll.wake.CompareAndSwap(pdReady, uintptr(unsafe.Pointer(loopG))) {
		unlock(&stacklessCoroNetpoll.drainLock)
		throw("runtime: unexpected stackless coroutine netpoll wake state")
	}
	needWake := false
	if stacklessCoroNetpoll.drained.Load() != 0 || stacklessCoroNetpoll.head.Load() != 0 {
		needWake = stacklessCoroNetpoll.wake.CompareAndSwap(uintptr(unsafe.Pointer(loopG)), pdReady)
	}
	unlock(&stacklessCoroNetpoll.drainLock)
	if needWake {
		systemstack(func() {
			var list gList
			list.push(loopG)
			injectglist(&list)
		})
	}
}

// stacklessCoroDrainReadyIO processes already-dispatched netpoll completions
// until s has a runnable task or the ready queue is empty.
func stacklessCoroDrainReadyIO(s *stacklessCoroScheduler) *stacklessCoroTask {
	for {
		op := stacklessCoroNetpollPopOne(true)
		if op == nil {
			return nil
		}
		stacklessCoroSocketAttempt(op)
		if task := s.take(); task != nil {
			return task
		}
	}
}

func stacklessCoroDrainReadyIOForReplacement(s *stacklessCoroScheduler) bool {
	for {
		if s.runnableState.Load()&^stacklessCoroForeignReturnerBit != 0 {
			return true
		}
		op := stacklessCoroNetpollPopOne(true)
		if op == nil {
			return s.runnableState.Load()&^stacklessCoroForeignReturnerBit != 0
		}
		stacklessCoroSocketAttempt(op)
	}
}

// stacklessCoroNetpollPopOne claims one ready I/O operation from the netpoll
// completion list so multiple idle executors and the netpoll completion
// goroutine can execute nonblocking I/O retries in parallel.
func stacklessCoroNetpollPopOne(checkFast bool) *stacklessCoroOperation {
	if checkFast && stacklessCoroNetpoll.drained.Load() == 0 && stacklessCoroNetpoll.head.Load() == 0 {
		return nil
	}
	lock(&stacklessCoroNetpoll.drainLock)
	head := stacklessCoroNetpoll.drained.Load()
	if head == 0 {
		head = stacklessCoroNetpoll.head.Swap(0)
	}
	if head == 0 {
		unlock(&stacklessCoroNetpoll.drainLock)
		return nil
	}
	op := (*stacklessCoroOperation)(unsafe.Pointer(head))
	next := uintptr(op.resourceState().packet[stacklessCoroPollNextWord])
	op.resourceState().packet[stacklessCoroPollNextWord] = 0
	stacklessCoroNetpoll.drained.Store(next)
	unlock(&stacklessCoroNetpoll.drainLock)
	if op.id == 0 {
		throw("runtime: invalid stackless coroutine poll completion")
	}
	return op
}

func stacklessCoroSocketReadPollFinish(op *stacklessCoroOperation, pollErr int) {
	status := uintptr(pollErr)
	if !op.resourceState().ownsPollDesc {
		status |= stacklessCoroPollErrorTag
	}
	stacklessCoroSocketReadFinish(op, -1, status)
}

func stacklessCoroSocketReadFinish(op *stacklessCoroOperation, count int32, readErrno uintptr) {
	stacklessCoroSocketFinish(op, int(count), readErrno)
}

func stacklessCoroSocketFinish(op *stacklessCoroOperation, count int, errno uintptr) {
	if op.resourceState().packet[stacklessCoroPollNextWord] != 0 {
		throw("runtime: queued stackless coroutine socket completion")
	}
	pd := (*pollDesc)(unsafe.Pointer(uintptr(op.resourceState().packet[stacklessCoroPollDescWord])))
	completed := takeStacklessCoroOperation(op.id)
	if completed != op {
		throw("runtime: mismatched stackless coroutine socket completion")
	}
	op.resourceState().packet[stacklessCoroPollDescWord] = 0
	if pd != nil && op.resourceState().ownsPollDesc {
		poll_runtime_pollUnblock(pd)
		poll_runtime_pollClose(pd)
	}
	if op.resourceState().n != nil {
		if count < 0 {
			*op.resourceState().n = -1
		} else {
			*op.resourceState().n = count
		}
	}
	if op.resourceState().errno != nil {
		*op.resourceState().errno = errno
	}
	completeStacklessCoroOperation(op)
}

func stacklessCoroSocketAttempt(op *stacklessCoroOperation) {
	if op.resourceState().pollWrite {
		stacklessCoroSocketWriteAttempt(op)
	} else {
		stacklessCoroSocketReadAttempt(op)
	}
}

func stacklessCoroNetpollInit() {
	if stacklessCoroNetpoll.started.CompareAndSwap(0, 1) {
		go stacklessCoroNetpollLoop()
	}
}

// stacklessCoroNetpollReady publishes a pointer-free completion link and
// returns the single safe completion G when it needs to be made runnable.
//
//go:nowritebarrier
func stacklessCoroNetpollReady(op *stacklessCoroOperation) *g {
	for {
		head := stacklessCoroNetpoll.head.Load()
		op.resourceState().packet[stacklessCoroPollNextWord] = uint64(head)
		if stacklessCoroNetpoll.head.CompareAndSwap(head,
			uintptr(unsafe.Pointer(op))) {
			break
		}
	}

	for {
		old := stacklessCoroNetpoll.wake.Load()
		if old == pdReady {
			return nil
		}
		if stacklessCoroNetpoll.wake.CompareAndSwap(old, pdReady) {
			if old > pdWait {
				return (*g)(unsafe.Pointer(old))
			}
			return nil
		}
	}
}

func stacklessCoroNetpollLoop() {
	stacklessCoroNetpoll.loopG.Store(getg())
	for {
		for {
			op := stacklessCoroNetpollPopOne(true)
			if op == nil {
				break
			}
			if getg().stacklessCoro != nil {
				throw("runtime: invalid stackless coroutine poll completion")
			}
			stacklessCoroSocketAttempt(op)
		}

		if stacklessCoroNetpoll.wake.CompareAndSwap(pdReady, pdNil) {
			continue
		}
		// Serialize with concurrent drainLock holders before parking so an
		// operation moving from head to drained cannot be missed.
		if op := stacklessCoroNetpollPopOne(false); op != nil {
			if getg().stacklessCoro != nil {
				throw("runtime: invalid stackless coroutine poll completion")
			}
			stacklessCoroSocketAttempt(op)
			continue
		}
		if stacklessCoroNetpoll.wake.CompareAndSwap(pdNil, pdWait) {
			gopark(stacklessCoroNetpollPark,
				unsafe.Pointer(&stacklessCoroNetpoll.wake),
				waitReasonIOWait, traceBlockNet, 1)
		}
	}
}

func stacklessCoroNetpollPark(gp *g, state unsafe.Pointer) bool {
	wake := (*atomic.Uintptr)(state)
	return wake.CompareAndSwap(pdWait, uintptr(unsafe.Pointer(gp)))
}
