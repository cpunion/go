// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

const stacklessCoroMaxRead = 1<<31 - 1
const stacklessCoroBadCompletion = ^uintptr(0)

var stacklessCoroReadPool struct {
	lock mutex
	wake chan struct{}
	head *stacklessCoroOperation
	tail *stacklessCoroOperation
}

func init() {
	lockInit(&stacklessCoroReadPool.lock, lockRankLeafRank)
}

func coroFileRead(ctx unsafe.Pointer, fd int, buffer []byte, n *int, errno *uintptr) {
	startStacklessCoroRead(ctx, fd, buffer, n, errno, false)
}

func coroSocketRead(ctx unsafe.Pointer, fd int, buffer []byte, n *int, errno *uintptr) {
	startStacklessCoroRead(ctx, fd, buffer, n, errno, true)
}

func coroCallRead(ctx unsafe.Pointer, call func()) {
	if call == nil {
		throw("runtime: nil stackless coroutine read call")
	}
	s, task := stacklessCoroStartOperation(ctx, "read call")
	op := &stacklessCoroOperation{
		scheduler: s,
		task:      task,
		call:      call,
	}
	registerStacklessCoroOperation(op)
	stacklessCoroReadEnqueue(op)
}

func coroAsyncDouble(ctx unsafe.Pointer, readFD, writeFD int, value uint64, result *uint64, errno *uintptr) {
	op := startStacklessCoroAsync(ctx, readFD, result, errno)
	coroEnterBlocking(ctx)
	submitErr := coroSubmit(op.id, value, int32(writeFD))
	coroExitBlocking()
	if submitErr != 0 {
		failStacklessCoroAsync(op.id, uintptr(submitErr))
		return
	}
	stacklessCoroReadEnqueue(op)
}

func coroSubmit(id, value uint64, fd int32) int32

func startStacklessCoroAsync(ctx unsafe.Pointer, readFD int, result *uint64, errno *uintptr) *stacklessCoroOperation {
	s, task := stacklessCoroStartOperation(ctx, "async foreign call")
	op := &stacklessCoroOperation{
		scheduler: s,
		task:      task,
		fd:        int32(readFD),
		errno:     errno,
		valueOut:  result,
		poll:      true,
		async:     true,
	}
	registerStacklessCoroOperation(op)
	return op
}

func failStacklessCoroAsync(id uint64, errno uintptr) {
	op := takeStacklessCoroOperation(id)
	if op == nil {
		return
	}
	if op.errno != nil {
		*op.errno = errno
	}
	op.valueOut = nil
	op.errno = nil
	op.scheduler.ready(op.task, true)
}

func startStacklessCoroRead(ctx unsafe.Pointer, fd int, buffer []byte, n *int, errno *uintptr, poll bool) {
	s, task := stacklessCoroStartOperation(ctx, "read")
	op := &stacklessCoroOperation{
		scheduler: s,
		task:      task,
		fd:        int32(fd),
		buffer:    buffer,
		n:         n,
		errno:     errno,
		poll:      poll,
	}
	registerStacklessCoroOperation(op)
	stacklessCoroReadEnqueue(op)
}

func stacklessCoroReadEnqueue(op *stacklessCoroOperation) {
	wake := stacklessCoroReadWake()
	lock(&stacklessCoroReadPool.lock)
	op.workNext = nil
	if stacklessCoroReadPool.tail == nil {
		stacklessCoroReadPool.head = op
	} else {
		stacklessCoroReadPool.tail.workNext = op
	}
	stacklessCoroReadPool.tail = op
	unlock(&stacklessCoroReadPool.lock)
	stacklessCoroReadSignal(wake)
}

func stacklessCoroReadWake() chan struct{} {
	lock(&stacklessCoroReadPool.lock)
	wake := stacklessCoroReadPool.wake
	unlock(&stacklessCoroReadPool.lock)
	if wake != nil {
		return wake
	}

	candidate := make(chan struct{}, 1)
	start := false
	lock(&stacklessCoroReadPool.lock)
	if stacklessCoroReadPool.wake == nil {
		stacklessCoroReadPool.wake = candidate
		start = true
	} else {
		candidate = stacklessCoroReadPool.wake
	}
	unlock(&stacklessCoroReadPool.lock)
	if start {
		for range 4 {
			go stacklessCoroReadWorkerLoop()
		}
	}
	return candidate
}

func stacklessCoroReadSignal(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

func stacklessCoroReadWorkerLoop() {
	wake := stacklessCoroReadWake()
	for {
		<-wake
		lock(&stacklessCoroReadPool.lock)
		op := stacklessCoroReadPool.head
		if op != nil {
			stacklessCoroReadPool.head = op.workNext
			op.workNext = nil
			if stacklessCoroReadPool.head == nil {
				stacklessCoroReadPool.tail = nil
			}
		}
		more := stacklessCoroReadPool.head != nil
		unlock(&stacklessCoroReadPool.lock)
		if more {
			stacklessCoroReadSignal(wake)
		}
		if op != nil {
			if op.call != nil {
				stacklessCoroCallWorker(op.id)
			} else if op.async {
				stacklessCoroAsyncWorker(op.id)
			} else {
				stacklessCoroReadWorker(op.id)
			}
		}
	}
}

func stacklessCoroCallWorker(id uint64) {
	op := findStacklessCoroOperation(id)
	if op == nil {
		return
	}
	op.call()

	op = takeStacklessCoroOperation(id)
	if op == nil {
		return
	}
	op.call = nil
	op.scheduler.ready(op.task, true)
}

func stacklessCoroReadWorker(id uint64) {
	op := findStacklessCoroOperation(id)
	if op == nil {
		return
	}

	var n int32
	var errno uintptr
	if len(op.buffer) != 0 {
		length := len(op.buffer)
		if length > stacklessCoroMaxRead {
			length = stacklessCoroMaxRead
		}
		if op.poll {
			n, errno = stacklessCoroPollRead(op.fd,
				unsafe.Pointer(&op.buffer[0]), int32(length))
		} else {
			entersyscallblock()
			n = read(op.fd, unsafe.Pointer(&op.buffer[0]), int32(length))
			exitsyscall()
			if n < 0 {
				errno = uintptr(-n)
			}
		}
	}

	op = takeStacklessCoroOperation(id)
	if op == nil {
		return
	}
	if op.n != nil {
		if n < 0 {
			*op.n = -1
		} else {
			*op.n = int(n)
		}
	}
	if op.errno != nil {
		*op.errno = errno
	}
	op.buffer = nil
	op.n = nil
	op.errno = nil
	op.scheduler.ready(op.task, true)
}

func stacklessCoroAsyncWorker(id uint64) {
	op := findStacklessCoroOperation(id)
	if op == nil {
		return
	}
	n, errno := stacklessCoroPollRead(op.fd,
		unsafe.Pointer(&op.packet[0]), int32(unsafe.Sizeof(op.packet)))

	op = takeStacklessCoroOperation(id)
	if op == nil {
		return
	}
	if n != int32(unsafe.Sizeof(op.packet)) || op.packet[0] != id {
		if errno == 0 {
			errno = stacklessCoroBadCompletion
		}
	} else if op.valueOut != nil {
		*op.valueOut = op.packet[1]
	}
	if op.errno != nil {
		*op.errno = errno
	}
	op.valueOut = nil
	op.errno = nil
	op.scheduler.ready(op.task, true)
}

func stacklessCoroPollRead(fd int32, p unsafe.Pointer, length int32) (int32, uintptr) {
	netpollGenericInit()
	pd, openErr := poll_runtime_pollOpen(uintptr(fd))
	if openErr != 0 {
		return -1, uintptr(openErr)
	}
	defer func() {
		poll_runtime_pollUnblock(pd)
		poll_runtime_pollClose(pd)
	}()

	for {
		if resetErr := poll_runtime_pollReset(pd, 'r'); resetErr != pollNoError {
			return -1, uintptr(resetErr)
		}
		n := read(fd, p, length)
		if n != -_EAGAIN {
			if n < 0 {
				return -1, uintptr(-n)
			}
			return n, 0
		}
		if waitErr := poll_runtime_pollWait(pd, 'r'); waitErr != pollNoError {
			return -1, uintptr(waitErr)
		}
	}
}
