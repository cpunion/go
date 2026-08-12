// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

const (
	stacklessCoroMaxRead           = 1<<31 - 1
	stacklessCoroDirectOperationID = ^uint64(0)
	stacklessCoroBadCompletion     = ^uintptr(0)
)

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
	op := stacklessCoroStartOperation(ctx, "read")
	// This operation is completed directly below and is never registered.
	// It still needs a nonzero identity for the common completion checks.
	op.id = stacklessCoroDirectOperationID
	op.buffer = buffer
	op.n = n
	op.errno = errno

	var count int32
	var readErrno uintptr
	if len(buffer) != 0 {
		length := len(buffer)
		if length > stacklessCoroMaxRead {
			length = stacklessCoroMaxRead
		}
		coroEnterSyscall(ctx)
		count = read(int32(fd), unsafe.Pointer(&buffer[0]), int32(length))
		coroExitSyscall()
		if count < 0 {
			readErrno = uintptr(-count)
		}
	}
	KeepAlive(buffer)

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

func coroSocketRead(ctx unsafe.Pointer, fd int, buffer []byte, n *int, errno *uintptr) {
	startStacklessCoroSocketRead(ctx, fd, buffer, n, errno)
}

func coroCallRead(ctx unsafe.Pointer, call func()) {
	if call == nil {
		throw("runtime: nil stackless coroutine read call")
	}
	op := stacklessCoroStartOperation(ctx, "read call")
	op.call = call
	registerStacklessCoroOperation(op)
	stacklessCoroReadEnqueue(op)
}

func coroAsyncDouble(ctx unsafe.Pointer, readFD, writeFD int, value uint64, result *uint64, errno *uintptr) {
	op := startStacklessCoroAsync(ctx, readFD, result, errno)
	id := op.id
	coroEnterBlocking(ctx)
	submitErr := coroSubmit(id, value, int32(writeFD))
	coroExitBlocking()
	if submitErr != 0 {
		failStacklessCoroAsync(id, uintptr(submitErr))
		return
	}
	stacklessCoroReadEnqueue(op)
}

func coroSubmit(id, value uint64, fd int32) int32

func startStacklessCoroAsync(ctx unsafe.Pointer, readFD int, result *uint64, errno *uintptr) *stacklessCoroOperation {
	op := stacklessCoroStartOperation(ctx, "async foreign call")
	op.fd = int32(readFD)
	op.errno = errno
	op.valueOut = result
	op.async = true
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
	completeStacklessCoroOperation(op)
}

func startStacklessCoroSocketRead(ctx unsafe.Pointer, fd int, buffer []byte, n *int, errno *uintptr) {
	op := stacklessCoroStartOperation(ctx, "read")
	op.fd = int32(fd)
	op.buffer = buffer
	op.n = n
	op.errno = errno
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
				stacklessCoroSocketReadWorker(op.id)
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
	completeStacklessCoroOperation(op)
}

func stacklessCoroSocketReadWorker(id uint64) {
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
		n, errno = stacklessCoroPollRead(op.fd,
			unsafe.Pointer(&op.buffer[0]), int32(length))
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
	completeStacklessCoroOperation(op)
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
	completeStacklessCoroOperation(op)
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
