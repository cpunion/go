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
	started atomic.Uint32
	head    atomic.Uintptr
	wake    atomic.Uintptr
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

func startStacklessCoroSocketReadWithPollDesc(ctx unsafe.Pointer, pd uintptr,
	fd int, buffer []byte, n *int, errno *uintptr) {
	descriptor := (*pollDesc)(unsafe.Pointer(pd))
	op := newStacklessCoroSocketReadOperation(ctx, fd, buffer, n, errno)
	if len(buffer) == 0 {
		registerStacklessCoroOperation(op)
		stacklessCoroSocketReadFinish(op, 0, 0)
		return
	}
	if descriptor == nil {
		throw("runtime: nil stackless coroutine poll descriptor")
	}
	stacklessCoroSocketReadStart(op, descriptor, false)
}

//go:linkname poll_runtime_coroSocketRead internal/poll.runtime_coroSocketRead
func poll_runtime_coroSocketRead(ctx unsafe.Pointer, pd uintptr,
	fd int, buffer []byte, n *int, errno *uintptr) {
	startStacklessCoroSocketReadWithPollDesc(ctx, pd, fd, buffer, n, errno)
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
	op.resourceState().packet[stacklessCoroPollDescWord] = uint64(uintptr(unsafe.Pointer(pd)))
	op.resourceState().ownsPollDesc = ownsPollDesc
	registerStacklessCoroOperation(op)
	stacklessCoroSocketReadAttempt(op)
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
		waiting, waitErr := netpollCoroReadArm(pd, op)
		if waitErr != pollNoError {
			stacklessCoroSocketReadPollFinish(op, waitErr)
			return
		}
		if waiting {
			stacklessCoroNetpollInit()
			return
		}
	}
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
			if op.ioState() == nil || op.scheduler() != s || op.task == exclude ||
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
	for {
		head := stacklessCoroNetpoll.head.Swap(0)
		for head != 0 {
			op := (*stacklessCoroOperation)(unsafe.Pointer(head))
			head = uintptr(op.resourceState().packet[stacklessCoroPollNextWord])
			op.resourceState().packet[stacklessCoroPollNextWord] = 0
			if getg().stacklessCoro != nil || op.id == 0 ||
				findStacklessCoroOperation(op.id) != op {
				throw("runtime: invalid stackless coroutine poll completion")
			}
			stacklessCoroSocketAttempt(op)
		}

		if !stacklessCoroNetpoll.wake.CompareAndSwap(pdReady, pdNil) &&
			stacklessCoroNetpoll.head.Load() == 0 &&
			stacklessCoroNetpoll.wake.CompareAndSwap(pdNil, pdWait) {
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
