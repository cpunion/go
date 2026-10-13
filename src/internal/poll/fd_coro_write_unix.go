// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package poll

import (
	"internal/asan"
	"internal/msan"
	"internal/race"
	"io"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// CoroWriteState describes how a stackless coroutine write was started.
type CoroWriteState uint8

const (
	// CoroWriteFallback asks the caller to perform the ordinary write on a
	// blocking worker. This handles write-lock contention, non-stream FDs,
	// and builds that need syscall.Write's race or sanitizer hooks.
	CoroWriteFallback CoroWriteState = iota
	// CoroWriteDone reports an immediate result that needs no finish call.
	CoroWriteDone
	// CoroWriteWait reports an operation that owns the FD write lock until
	// CoroWriteFinish is called after the coroutine is resumed.
	CoroWriteWait
)

// coroWriteNoProgressTag denotes a successful syscall that wrote no bytes
// before the full buffer was written. FD.Write returns io.ErrUnexpectedEOF
// for this case. The runtime uses the same completion encoding.
const coroWriteNoProgressTag = coroPollErrorTag >> 1

// tryWriteLock tries to add a reference to fd and lock fd for writing.
// It returns (false, nil) when another write holds the lock and
// (false, errClosing) when fd is closing.
func (fd *FD) tryWriteLock() (bool, error) {
	if !fd.fdmu.rwlock(writeLock, tryLock) {
		if fd.closing() {
			return false, errClosing(fd.isFile)
		}
		return false, nil
	}
	return true, nil
}

// CoroWriteStart starts a write without blocking the current coroutine
// executor on FD lock contention. A CoroWriteWait result retains the write
// lock and requires a matching CoroWriteFinish call. The runtime writes the
// whole buffer, or returns the bytes written together with a nonzero status.
func (fd *FD) CoroWriteStart(ctx unsafe.Pointer, p []byte, n *int, status *uintptr) (CoroWriteState, error) {
	locked, err := fd.tryWriteLock()
	if err != nil {
		return CoroWriteDone, err
	}
	if !locked {
		return CoroWriteFallback, nil
	}
	if !fd.IsStream || race.Enabled || msan.Enabled || asan.Enabled {
		// The runtime stream operation splits large writes. Keep datagram
		// boundaries and syscall.Write's instrumentation in the ordinary path.
		fd.writeUnlock()
		return CoroWriteFallback, nil
	}
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		fd.writeUnlock()
		return CoroWriteDone, err
	}
	// Unlike Read, Write must issue a syscall even for an empty buffer.
	if fd.pd.pollable() && atomic.LoadUint32(&fd.isBlocking) == 0 {
		runtime_coroSocketWrite(ctx, fd.pd.runtimeCtx, fd.Sysfd, p, n, status)
	} else {
		runtime_coroFileWrite(ctx, fd.Sysfd, p, n, status)
	}
	return CoroWriteWait, nil
}

// CoroWriteFinish converts a runtime completion into the result of FD.Write
// and releases the write lock retained by CoroWriteStart.
func (fd *FD) CoroWriteFinish(n *int, status uintptr) error {
	defer fd.writeUnlock()
	if *n < 0 {
		*n = 0
	}
	if status&coroPollErrorTag != 0 {
		return convertErr(int(status&^coroPollErrorTag), fd.isFile)
	}
	if status == coroWriteNoProgressTag {
		return io.ErrUnexpectedEOF
	}
	if status != 0 {
		return syscall.Errno(status)
	}
	return nil
}

// CoroCallWrite runs call on the bounded blocking-I/O worker pool shared
// with reads. It preserves lock contention, non-stream, and instrumentation
// semantics through the ordinary Write implementation.
func CoroCallWrite(ctx unsafe.Pointer, call func()) {
	runtime_coroCallRead(ctx, call)
}

func runtime_coroFileWrite(ctx unsafe.Pointer, fd int, p []byte, n *int, status *uintptr)

func runtime_coroSocketWrite(ctx unsafe.Pointer, pd uintptr, fd int, p []byte, n *int, status *uintptr)
