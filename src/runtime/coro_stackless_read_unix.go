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
	lockInit(&stacklessCoroReadPool.lock, lockRankStacklessCoro)
}

func coroFileRead(ctx unsafe.Pointer, fd int, buffer []byte, n *int, errno *uintptr) {
	op := stacklessCoroStartIOOperation(ctx, "read")
	// This operation is completed directly below and is never registered.
	op.id = stacklessCoroDirectOperationID
	count, status := stacklessCoroFileRead(ctx, fd, buffer, false)
	if n != nil {
		*n = count
	}
	if errno != nil {
		*errno = status
	}
	completeStacklessCoroOperation(op)
}

//go:linkname poll_runtime_coroFileRead internal/poll.runtime_coroFileRead
func poll_runtime_coroFileRead(ctx unsafe.Pointer, fd int, buffer []byte) (int, uintptr) {
	return stacklessCoroFileRead(ctx, fd, buffer, true)
}

//go:linkname poll_runtime_coroCallRead internal/poll.runtime_coroCallRead
func poll_runtime_coroCallRead(ctx unsafe.Pointer, call func()) {
	coroCallRead(ctx, call)
}

// stacklessCoroFileRead may block in the syscall. The executor can hand off
// its P and admit replacement capacity while the calling frame stays live.
func stacklessCoroFileRead(ctx unsafe.Pointer, fd int, buffer []byte, retryEINTR bool) (int, uintptr) {
	if len(buffer) == 0 {
		return 0, 0
	}
	p := unsafe.Pointer(&buffer[0])
	length := stacklessCoroReadLength(len(buffer))
	coroEnterSyscall(ctx)
	count := stacklessCoroRead(int32(fd), p, length, retryEINTR)
	coroExitSyscall()
	KeepAlive(buffer)
	return stacklessCoroReadResult(count)
}

// The file path calls this while in syscall state.
//
//go:nosplit
func stacklessCoroRead(fd int32, p unsafe.Pointer, length int32, retryEINTR bool) int32 {
	for {
		count := read(fd, p, length)
		if count != -_EINTR || !retryEINTR {
			return count
		}
	}
}

func stacklessCoroReadResult(count int32) (int, uintptr) {
	if count < 0 {
		return -1, uintptr(-count)
	}
	return int(count), 0
}

func stacklessCoroReadLength(length int) int32 {
	if length > stacklessCoroMaxRead {
		return stacklessCoroMaxRead
	}
	return int32(length)
}

func coroSocketRead(ctx unsafe.Pointer, fd int, buffer []byte, n *int, errno *uintptr) {
	startStacklessCoroSocketRead(ctx, fd, buffer, n, errno)
}

// coroGC waits for an ordinary Go worker to complete the synchronous GC call.
// The pending operation keeps the native activation and its typed frame alive;
// GC's goroutine waits must not execute on the fixed native executor stack.
func coroGC(ctx unsafe.Pointer) {
	coroCallRead(ctx, GC)
}

func coroCallRead(ctx unsafe.Pointer, call func()) {
	if call == nil {
		throw("runtime: nil stackless coroutine read call")
	}
	op := stacklessCoroStartIOOperation(ctx, "read call")
	op.resourceState().call = call
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
	op := stacklessCoroStartIOOperation(ctx, "async foreign call")
	op.resourceState().fd = int32(readFD)
	op.resourceState().errno = errno
	op.resourceState().valueOut = result
	op.resourceState().async = true
	registerStacklessCoroOperation(op)
	return op
}

func failStacklessCoroAsync(id uint64, errno uintptr) {
	op := takeStacklessCoroOperation(id)
	if op == nil {
		return
	}
	if op.resourceState().errno != nil {
		*op.resourceState().errno = errno
	}
	completeStacklessCoroOperation(op)
}

func stacklessCoroReadEnqueue(op *stacklessCoroOperation) {
	wake := stacklessCoroReadWake()
	lock(&stacklessCoroReadPool.lock)
	op.resourceState().workNext = nil
	if stacklessCoroReadPool.tail == nil {
		stacklessCoroReadPool.head = op
	} else {
		stacklessCoroReadPool.tail.resourceState().workNext = op
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
			stacklessCoroReadPool.head = op.resourceState().workNext
			op.resourceState().workNext = nil
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
			if op.resourceState().call != nil {
				stacklessCoroCallWorker(op.id)
			} else if op.resourceState().async {
				stacklessCoroAsyncWorker(op.id)
			} else {
				throw("runtime: invalid stackless coroutine worker operation")
			}
		}
	}
}

func stacklessCoroCallWorker(id uint64) {
	op := findStacklessCoroOperation(id)
	if op == nil {
		return
	}
	op.resourceState().call()

	op = takeStacklessCoroOperation(id)
	if op == nil {
		return
	}
	completeStacklessCoroOperation(op)
}

func stacklessCoroAsyncWorker(id uint64) {
	op := findStacklessCoroOperation(id)
	if op == nil {
		return
	}
	n, errno := stacklessCoroPollRead(op.resourceState().fd,
		unsafe.Pointer(&op.resourceState().packet[0]), int32(unsafe.Sizeof(op.resourceState().packet)))

	op = takeStacklessCoroOperation(id)
	if op == nil {
		return
	}
	if n != int32(unsafe.Sizeof(op.resourceState().packet)) || op.resourceState().packet[0] != id {
		if errno == 0 {
			errno = stacklessCoroBadCompletion
		}
	} else if op.resourceState().valueOut != nil {
		*op.resourceState().valueOut = op.resourceState().packet[1]
	}
	if op.resourceState().errno != nil {
		*op.resourceState().errno = errno
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
