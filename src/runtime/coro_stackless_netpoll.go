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
	op := stacklessCoroStartOperation(ctx, "read")
	op.fd = int32(fd)
	op.buffer = buffer
	op.n = n
	op.errno = errno
	return op
}

func stacklessCoroSocketReadStart(op *stacklessCoroOperation, pd *pollDesc, ownsPollDesc bool) {
	op.packet[stacklessCoroPollDescWord] = uint64(uintptr(unsafe.Pointer(pd)))
	op.ownsPollDesc = ownsPollDesc
	registerStacklessCoroOperation(op)
	stacklessCoroSocketReadAttempt(op)
}

func stacklessCoroSocketReadAttempt(op *stacklessCoroOperation) {
	pd := (*pollDesc)(unsafe.Pointer(uintptr(op.packet[stacklessCoroPollDescWord])))
	if pd == nil || len(op.buffer) == 0 {
		throw("runtime: invalid stackless coroutine socket read")
	}
	for {
		if resetErr := poll_runtime_pollReset(pd, 'r'); resetErr != pollNoError {
			stacklessCoroSocketReadPollFinish(op, resetErr)
			return
		}
		count := read(op.fd, unsafe.Pointer(&op.buffer[0]),
			stacklessCoroReadLength(len(op.buffer)))
		KeepAlive(op.buffer)
		if count == -_EINTR && !op.ownsPollDesc {
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

// stacklessCoroPollReadAtIdle claims one read that still belongs to s and does
// not belong to skip. The caller changes skip between bounded attempts so
// multiple waiters get a chance. This lets readiness produced by another task
// in the same scheduler complete without a locked-M round trip. An unavailable
// read is rearmed before this function returns.
func stacklessCoroPollReadAtIdle(s *stacklessCoroScheduler,
	skip *stacklessCoroTask) *stacklessCoroTask {
	lock(&stacklessCoroOperations.lock)
	for op := stacklessCoroOperations.head; op != nil; op = op.next {
		if op.scheduler != s || op.task == skip || op.async ||
			op.packet[stacklessCoroPollDescWord] == 0 {
			continue
		}
		pd := (*pollDesc)(unsafe.Pointer(
			uintptr(op.packet[stacklessCoroPollDescWord])))
		if !netpollCoroReadClaim(pd, op) {
			continue
		}
		task := op.task
		unlock(&stacklessCoroOperations.lock)
		stacklessCoroSocketReadAttempt(op)
		return task
	}
	unlock(&stacklessCoroOperations.lock)
	return nil
}

func stacklessCoroSocketReadPollFinish(op *stacklessCoroOperation, pollErr int) {
	status := uintptr(pollErr)
	if !op.ownsPollDesc {
		status |= stacklessCoroPollErrorTag
	}
	stacklessCoroSocketReadFinish(op, -1, status)
}

func stacklessCoroSocketReadFinish(op *stacklessCoroOperation, count int32, readErrno uintptr) {
	if op.packet[stacklessCoroPollNextWord] != 0 {
		throw("runtime: queued stackless coroutine socket completion")
	}
	pd := (*pollDesc)(unsafe.Pointer(uintptr(op.packet[stacklessCoroPollDescWord])))
	completed := takeStacklessCoroOperation(op.id)
	if completed != op {
		throw("runtime: mismatched stackless coroutine socket completion")
	}
	op.packet[stacklessCoroPollDescWord] = 0
	if pd != nil && op.ownsPollDesc {
		poll_runtime_pollUnblock(pd)
		poll_runtime_pollClose(pd)
	}
	if op.n != nil {
		if count < 0 {
			*op.n = -1
		} else {
			*op.n = int(count)
		}
	}
	if op.errno != nil {
		*op.errno = readErrno
	}
	completeStacklessCoroOperation(op)
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
		op.packet[stacklessCoroPollNextWord] = uint64(head)
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
			head = uintptr(op.packet[stacklessCoroPollNextWord])
			op.packet[stacklessCoroPollNextWord] = 0
			if getg().stacklessCoro != nil || op.id == 0 ||
				findStacklessCoroOperation(op.id) != op {
				throw("runtime: invalid stackless coroutine poll completion")
			}
			stacklessCoroSocketReadAttempt(op)
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
