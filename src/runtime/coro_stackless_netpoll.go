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

func startStacklessCoroSocketRead(ctx unsafe.Pointer, fd int, buffer []byte, n *int, errno *uintptr) {
	op := stacklessCoroStartOperation(ctx, "read")
	op.fd = int32(fd)
	op.buffer = buffer
	op.n = n
	op.errno = errno
	op.pollRead = true
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
	stacklessCoroNetpollInit()
	op.packet[stacklessCoroPollDescWord] = uint64(uintptr(unsafe.Pointer(pd)))
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
			stacklessCoroSocketReadFinish(op, -1, uintptr(resetErr))
			return
		}
		count := read(op.fd, unsafe.Pointer(&op.buffer[0]),
			stacklessCoroReadLength(len(op.buffer)))
		KeepAlive(op.buffer)
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
			stacklessCoroSocketReadFinish(op, -1, uintptr(waitErr))
			return
		}
		if waiting {
			return
		}
	}
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
	if pd != nil {
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
// makes the single safe completion G runnable through netpoll's return list.
//
//go:nowritebarrier
func stacklessCoroNetpollReady(toRun *gList, op *stacklessCoroOperation) {
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
			return
		}
		if stacklessCoroNetpoll.wake.CompareAndSwap(old, pdReady) {
			if old > pdWait {
				toRun.push((*g)(unsafe.Pointer(old)))
			}
			return
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
