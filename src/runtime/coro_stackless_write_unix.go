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
	op.fd = int32(fd)
	op.buffer = buffer
	op.n = n
	op.errno = status
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
	op.pollWrite = true
	op.packet[stacklessCoroPollDescWord] = uint64(descriptor)
	registerStacklessCoroOperation(op)
	stacklessCoroSocketWriteAttempt(op)
}

func stacklessCoroWriteBuffer(op *stacklessCoroOperation) []byte {
	p := op.buffer[*op.n:]
	if len(p) > stacklessCoroMaxWrite {
		p = p[:stacklessCoroMaxWrite]
	}
	return p
}

// stacklessCoroWriteResult accumulates a syscall result and reports whether
// the operation is complete. A failed syscall preserves earlier progress.
func stacklessCoroWriteResult(op *stacklessCoroOperation, count int32) bool {
	if count < 0 {
		*op.errno = uintptr(-count)
		return true
	}
	if int(count) > len(stacklessCoroWriteBuffer(op)) {
		throw("runtime: invalid stackless coroutine write count")
	}
	*op.n += int(count)
	if *op.n == len(op.buffer) {
		return true
	}
	if count == 0 {
		*op.errno = stacklessCoroWriteNoProgressTag
		return true
	}
	return false
}

func stacklessCoroSocketWriteAttempt(op *stacklessCoroOperation) {
	pd := (*pollDesc)(unsafe.Pointer(uintptr(op.packet[stacklessCoroPollDescWord])))
	if pd == nil || !op.pollWrite {
		throw("runtime: invalid stackless coroutine socket write")
	}
	if err := poll_runtime_pollReset(pd, 'w'); err != pollNoError {
		stacklessCoroSocketFinish(op, *op.n, stacklessCoroPollErrorTag|uintptr(err))
		return
	}
	for {
		p := stacklessCoroWriteBuffer(op)
		count := write1(uintptr(op.fd), unsafe.Pointer(unsafe.SliceData(p)), int32(len(p)))
		KeepAlive(p)
		if count == -_EINTR {
			continue
		}
		if count != -_EAGAIN {
			if stacklessCoroWriteResult(op, count) {
				stacklessCoroSocketFinish(op, *op.n, *op.errno)
				return
			}
			continue
		}
		waiting, err := netpollCoroArm(pd, op, 'w')
		if err != pollNoError {
			stacklessCoroSocketFinish(op, *op.n, stacklessCoroPollErrorTag|uintptr(err))
			return
		}
		if waiting {
			stacklessCoroNetpollInit()
			return
		}
	}
}
