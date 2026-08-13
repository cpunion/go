// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package net

import (
	"internal/poll"
	"io"
	"runtime"
	"syscall"
	"unsafe"
)

// coroReadStart is the compiler-private entry used for a Read suspension.
// The common path borrows the connection's poll descriptor while holding its
// read lock. Lock contention alone uses the bounded blocking-read worker pool.
func (c *conn) coroReadStart(ctx unsafe.Pointer, b []byte, n *int, err *error, direct *bool, status *uintptr) bool {
	*n = 0
	*err = nil
	*direct = false
	*status = 0
	if !c.ok() {
		*err = syscall.EINVAL
		return false
	}

	state, e := c.fd.pfd.CoroReadStart(ctx, b, n, status)
	if state == poll.CoroReadFallback {
		poll.CoroCallRead(ctx, func() {
			count, readErr := c.fd.Read(b)
			*n = count
			*err = c.coroReadError(readErr)
		})
		return true
	}
	if state == poll.CoroReadWait {
		*direct = true
		return true
	}
	*err = c.coroReadError(wrapSyscallError(readSyscallName, e))
	return false
}

// coroReadFinish restores the public Read result after a direct coroutine
// operation. A fallback worker has already written the final result.
func (c *conn) coroReadFinish(n *int, err *error, direct bool, status uintptr) {
	if !direct {
		return
	}
	e := c.fd.pfd.CoroReadFinish(n, status)
	runtime.KeepAlive(c.fd)
	*err = c.coroReadError(wrapSyscallError(readSyscallName, e))
}

func (c *conn) coroReadError(err error) error {
	if err != nil && err != io.EOF {
		return &OpError{Op: "read", Net: c.fd.net, Source: c.fd.laddr, Addr: c.fd.raddr, Err: err}
	}
	return err
}
