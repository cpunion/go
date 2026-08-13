// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package poll

import (
	"sync/atomic"
	"syscall"
	"unsafe"
)

// CoroReadState describes how a stackless coroutine read was started.
type CoroReadState uint8

const (
	// CoroReadFallback asks the caller to perform the ordinary read on a
	// blocking worker. This is used only when another read owns the FD lock.
	CoroReadFallback CoroReadState = iota
	// CoroReadDone reports an immediate result that needs no finish call.
	CoroReadDone
	// CoroReadWait reports an operation that owns the FD read lock until
	// CoroReadFinish is called after the coroutine is resumed.
	CoroReadWait
)

const coroPollErrorTag = ^uintptr(0) ^ (^uintptr(0) >> 1)

// tryReadLock tries to add a reference to fd and lock fd for reading.
// It returns (false, nil) when another read holds the lock and
// (false, errClosing) when fd is closing.
func (fd *FD) tryReadLock() (bool, error) {
	if !fd.fdmu.rwlock(readlock, tryLock) {
		if fd.closing() {
			return false, errClosing(fd.isFile)
		}
		return false, nil
	}
	return true, nil
}

// CoroReadStart starts a read without blocking the current coroutine
// executor on FD lock contention. A CoroReadWait result retains the read lock
// and requires a matching CoroReadFinish call.
func (fd *FD) CoroReadStart(ctx unsafe.Pointer, p []byte, n *int, status *uintptr) (CoroReadState, error) {
	locked, err := fd.tryReadLock()
	if err != nil {
		return CoroReadDone, err
	}
	if !locked {
		return CoroReadFallback, nil
	}
	if len(p) == 0 {
		fd.readUnlock()
		return CoroReadDone, nil
	}
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		fd.readUnlock()
		return CoroReadDone, err
	}
	if fd.IsStream && len(p) > maxRW {
		p = p[:maxRW]
	}
	if fd.pd.pollable() && atomic.LoadUint32(&fd.isBlocking) == 0 {
		runtime_coroSocketRead(ctx, fd.pd.runtimeCtx, fd.Sysfd, p, n, status)
	} else {
		runtime_coroFileRead(ctx, fd.Sysfd, p, n, status)
	}
	return CoroReadWait, nil
}

// CoroReadFinish converts a runtime completion into the result of FD.Read and
// releases the read lock retained by CoroReadStart.
func (fd *FD) CoroReadFinish(n *int, status uintptr) error {
	defer fd.readUnlock()
	var err error
	if status&coroPollErrorTag != 0 {
		err = convertErr(int(status&^coroPollErrorTag), fd.isFile)
	} else if status != 0 {
		err = syscall.Errno(status)
	}
	if err != nil && *n < 0 {
		*n = 0
	}
	return fd.eofError(*n, err)
}

// CoroCallRead runs call on the bounded blocking-read worker pool.
func CoroCallRead(ctx unsafe.Pointer, call func()) {
	runtime_coroCallRead(ctx, call)
}

func runtime_coroFileRead(ctx unsafe.Pointer, fd int, p []byte, n *int, status *uintptr)

func runtime_coroSocketRead(ctx unsafe.Pointer, pd uintptr, fd int, p []byte, n *int, status *uintptr)

func runtime_coroCallRead(ctx unsafe.Pointer, call func())
