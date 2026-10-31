// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

// Keep the stream write limit in sync with internal/poll.maxRW. In contrast
// to a read, a public write must complete the whole buffer or return an error.
const stacklessCoroMaxWrite = 1 << 30

// The next bit below the poll error tag encodes a successful syscall that
// made no progress. internal/poll converts it to io.ErrUnexpectedEOF.
const stacklessCoroWriteNoProgressTag = stacklessCoroPollErrorTag >> 1

func newStacklessCoroWriteOperation(ctx unsafe.Pointer, fd int, buffer []byte,
	n *int, status *uintptr) *stacklessCoroOperation {
	op := stacklessCoroStartIOOperation(ctx, "write")
	op.resourceState().fd = int32(fd)
	op.resourceState().buffer = buffer
	op.resourceState().n = n
	op.resourceState().errno = status
	*n = 0
	*status = 0
	return op
}

//go:linkname poll_runtime_coroFileWrite internal/poll.runtime_coroFileWrite
func poll_runtime_coroFileWrite(ctx unsafe.Pointer, fd int, buffer []byte,
	n *int, status *uintptr) {
	op := newStacklessCoroWriteOperation(ctx, fd, buffer, n, status)
	op.id = stacklessCoroDirectOperationID
	for {
		p := stacklessCoroWriteBuffer(op)
		coroEnterSyscall(ctx)
		var count int32
		for {
			count = write1(uintptr(fd), unsafe.Pointer(unsafe.SliceData(p)), int32(len(p)))
			if count != -_EINTR {
				break
			}
		}
		coroExitSyscall()
		KeepAlive(p)
		if stacklessCoroWriteResult(op, count) {
			completeStacklessCoroOperation(op)
			return
		}
	}
}

//go:linkname poll_runtime_coroSocketWrite internal/poll.runtime_coroSocketWrite
func poll_runtime_coroSocketWrite(ctx unsafe.Pointer, descriptor uintptr,
	fd int, buffer []byte, n *int, status *uintptr) {
	if descriptor == 0 {
		throw("runtime: nil stackless coroutine write poll descriptor")
	}
	op := newStacklessCoroWriteOperation(ctx, fd, buffer, n, status)
	op.resourceState().pollWrite = true
	op.resourceState().packet[stacklessCoroPollDescWord] = uint64(descriptor)
	registerStacklessCoroOperation(op)
	stacklessCoroSocketWriteAttempt(op)
}

func stacklessCoroWriteBuffer(op *stacklessCoroOperation) []byte {
	p := op.resourceState().buffer[*op.resourceState().n:]
	if len(p) > stacklessCoroMaxWrite {
		p = p[:stacklessCoroMaxWrite]
	}
	return p
}

// stacklessCoroWriteResult accumulates a syscall result and reports whether
// the operation is complete. A failed syscall preserves earlier progress.
func stacklessCoroWriteResult(op *stacklessCoroOperation, count int32) bool {
	if count < 0 {
		*op.resourceState().errno = uintptr(-count)
		return true
	}
	if int(count) > len(stacklessCoroWriteBuffer(op)) {
		throw("runtime: invalid stackless coroutine write count")
	}
	*op.resourceState().n += int(count)
	if *op.resourceState().n == len(op.resourceState().buffer) {
		return true
	}
	if count == 0 {
		*op.resourceState().errno = stacklessCoroWriteNoProgressTag
		return true
	}
	return false
}

func stacklessCoroSocketWriteAttempt(op *stacklessCoroOperation) {
	pd := (*pollDesc)(unsafe.Pointer(uintptr(op.resourceState().packet[stacklessCoroPollDescWord])))
	if pd == nil || !op.resourceState().pollWrite {
		throw("runtime: invalid stackless coroutine socket write")
	}
	if err := poll_runtime_pollReset(pd, 'w'); err != pollNoError {
		stacklessCoroSocketFinish(op, *op.resourceState().n, stacklessCoroPollErrorTag|uintptr(err))
		return
	}
	for {
		p := stacklessCoroWriteBuffer(op)
		count := write1(uintptr(op.resourceState().fd), unsafe.Pointer(unsafe.SliceData(p)), int32(len(p)))
		KeepAlive(p)
		if count == -_EINTR {
			continue
		}
		if count != -_EAGAIN {
			if stacklessCoroWriteResult(op, count) {
				stacklessCoroSocketFinish(op, *op.resourceState().n, *op.resourceState().errno)
				return
			}
			continue
		}
		waiting, err := netpollCoroArm(pd, op, 'w')
		if err != pollNoError {
			stacklessCoroSocketFinish(op, *op.resourceState().n, stacklessCoroPollErrorTag|uintptr(err))
			return
		}
		if waiting {
			stacklessCoroNetpollInit()
			return
		}
	}
}
