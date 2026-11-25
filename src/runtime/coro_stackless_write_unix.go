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
	n *int, status *uintptr, written int) *stacklessCoroOperation {
	op := stacklessCoroStartIOOperation(ctx, "write")
	op.resourceState().fd = int32(fd)
	op.resourceState().buffer = buffer
	op.resourceState().n = n
	op.resourceState().errno = status
	*n = written
	*status = 0
	return op
}

// A file write returns in its calling activation. Syscall entry still
// reserves replacement capacity when the write could block other tasks.
//
//go:linkname poll_runtime_coroFileWrite internal/poll.runtime_coroFileWrite
func poll_runtime_coroFileWrite(ctx unsafe.Pointer, fd int, buffer []byte) (int, uintptr) {
	written := 0
	for {
		p := buffer[written:]
		if len(p) > stacklessCoroMaxWrite {
			p = p[:stacklessCoroMaxWrite]
		}
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
		if count < 0 {
			return written, uintptr(-count)
		}
		if int(count) > len(p) {
			throw("runtime: invalid stackless coroutine write count")
		}
		written += int(count)
		if written == len(buffer) {
			return written, 0
		}
		if count == 0 {
			return written, stacklessCoroWriteNoProgressTag
		}
	}
}

//go:linkname poll_runtime_coroSocketWrite internal/poll.runtime_coroSocketWrite
func poll_runtime_coroSocketWrite(ctx unsafe.Pointer, descriptor uintptr,
	fd int, buffer []byte, n *int, status *uintptr) bool {
	pd := (*pollDesc)(unsafe.Pointer(descriptor))
	if pd == nil {
		throw("runtime: nil stackless coroutine write poll descriptor")
	}
	if err := poll_runtime_pollReset(pd, 'w'); err != pollNoError {
		*n, *status = 0, stacklessCoroPollErrorTag|uintptr(err)
		return false
	}
	written, statusCode, blocked := stacklessCoroSocketWriteBuffer(fd, buffer, 0)
	if !blocked {
		*n, *status = written, statusCode
		return false
	}

	// The write lock keeps the descriptor borrowed while the caller is
	// suspended. Retain the operation only when the syscall would block.
	op := newStacklessCoroWriteOperation(ctx, fd, buffer, n, status, written)
	op.resourceState().pollWrite = true
	op.resourceState().packet[stacklessCoroPollDescWord] = uint64(descriptor)
	registerStacklessCoroOperation(op)
	if !stacklessCoroSocketWriteArm(op, pd) {
		stacklessCoroSocketWriteAttempt(op)
	}
	return true
}

// stacklessCoroWriteResult accumulates a syscall result and reports whether
// the whole write completed or failed. Failures preserve earlier progress.
func stacklessCoroWriteResult(size, written int, count int32) (int, uintptr, bool) {
	if count < 0 {
		return written, uintptr(-count), true
	}
	if int(count) > min(size-written, stacklessCoroMaxWrite) {
		throw("runtime: invalid stackless coroutine write count")
	}
	written += int(count)
	if written == size {
		return written, 0, true
	}
	if count == 0 {
		return written, stacklessCoroWriteNoProgressTag, true
	}
	return written, 0, false
}

// stacklessCoroSocketWriteBuffer advances a nonblocking write. Only EAGAIN
// leaves a pending operation. The caller retries or arms the poll descriptor.
func stacklessCoroSocketWriteBuffer(fd int, buffer []byte, written int) (int, uintptr, bool) {
	for {
		p := buffer[written:]
		if len(p) > stacklessCoroMaxWrite {
			p = p[:stacklessCoroMaxWrite]
		}
		count := write1(uintptr(fd), unsafe.Pointer(unsafe.SliceData(p)), int32(len(p)))
		KeepAlive(p)
		if int(count) == len(p) && len(p) == len(buffer)-written {
			return len(buffer), 0, false
		}
		switch count {
		case -_EINTR:
			continue
		case -_EAGAIN:
			return written, 0, true
		}
		var status uintptr
		var complete bool
		written, status, complete = stacklessCoroWriteResult(len(buffer), written, count)
		if complete {
			return written, status, false
		}
	}
}

func stacklessCoroSocketWriteAttempt(op *stacklessCoroOperation) {
	res := op.resourceState()
	pd := (*pollDesc)(unsafe.Pointer(uintptr(res.packet[stacklessCoroPollDescWord])))
	if pd == nil || !res.pollWrite {
		throw("runtime: invalid stackless coroutine socket write")
	}
	if err := poll_runtime_pollReset(pd, 'w'); err != pollNoError {
		stacklessCoroSocketFinish(op, *res.n, stacklessCoroPollErrorTag|uintptr(err))
		return
	}
	for {
		written, status, blocked := stacklessCoroSocketWriteBuffer(int(res.fd), res.buffer, *res.n)
		if !blocked {
			stacklessCoroSocketFinish(op, written, status)
			return
		}
		*res.n = written
		if stacklessCoroSocketWriteArm(op, pd) {
			return
		}
	}
}

// stacklessCoroSocketWriteArm reports whether the operation was armed or
// completed with an error. The caller retries when readiness raced with arming.
func stacklessCoroSocketWriteArm(op *stacklessCoroOperation, pd *pollDesc) bool {
	waiting, err := netpollCoroArm(pd, op, 'w')
	if err != pollNoError {
		stacklessCoroSocketFinish(op, *op.resourceState().n, stacklessCoroPollErrorTag|uintptr(err))
		return true
	}
	if waiting {
		stacklessCoroNetpollInit()
	}
	return waiting
}
