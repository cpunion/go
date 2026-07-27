// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

// Package coro exposes temporary operation declarations for the coroutine
// experiment. The compiler replaces supported calls with scheduler
// operations. Calls left on the ordinary path use their synchronous fallback.
package coro

import "syscall"

// DirectAdd calls the MVP scalar System ABI fixture.
func DirectAdd(a, b uint64) uint64

// DirectBlock calls the MVP blocking System ABI fixture.
func DirectBlock(gate *uint32)

// AsyncDouble asks the MVP foreign fixture to publish twice value through a
// descriptor. The compiler turns the call into a suspending operation.
func AsyncDouble(readFD, writeFD int, value uint64, result *uint64, errno *uintptr) {
	if result != nil {
		*result = value * 2
	}
	if errno != nil {
		*errno = 0
	}
}

// FileRead reads from a regular file descriptor and stores its result.
func FileRead(fd int, buffer []byte, n *int, errno *uintptr) {
	read(fd, buffer, n, errno)
}

// SocketRead waits for and reads from a nonblocking socket descriptor.
func SocketRead(fd int, buffer []byte, n *int, errno *uintptr) {
	read(fd, buffer, n, errno)
}

func read(fd int, buffer []byte, n *int, errno *uintptr) {
	count, err := syscall.Read(fd, buffer)
	if n != nil {
		*n = count
	}
	if errno == nil {
		return
	}
	*errno = 0
	if err == nil {
		return
	}
	if value, ok := err.(syscall.Errno); ok {
		*errno = uintptr(value)
		return
	}
	*errno = ^uintptr(0)
}
