// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package net

import (
	"internal/poll"
	"runtime"
	"syscall"
	"unsafe"
)

// coroWriteStart is the compiler-private entry used for a Write suspension.
// The direct path borrows the connection's poll descriptor while holding its
// write lock. Contended and non-stream writes use a blocking-I/O worker.
func (c *conn) coroWriteStart(ctx unsafe.Pointer, b []byte, n *int, err *error, direct *bool, status *uintptr) bool {
	*n = 0
	*err = nil
	*direct = false
	*status = 0
	if !c.ok() {
		*err = syscall.EINVAL
		return false
	}

	state, e := c.fd.pfd.CoroWriteStart(ctx, b, n, status)
	if state == poll.CoroWriteFallback {
		poll.CoroCallWrite(ctx, func() {
			count, writeErr := c.fd.Write(b)
			*n = count
			*err = c.coroWriteError(writeErr)
		})
		return true
	}
	if state == poll.CoroWriteWait {
		*direct = true
		return true
	}
	*err = c.coroWriteError(wrapSyscallError(writeSyscallName, e))
	return false
}

// coroWriteFinish restores the public Write result after a direct coroutine
// operation. A fallback worker has already written the final result.
func (c *conn) coroWriteFinish(n *int, err *error, direct bool, status uintptr) {
	if !direct {
		return
	}
	e := c.fd.pfd.CoroWriteFinish(n, status)
	runtime.KeepAlive(c.fd)
	*err = c.coroWriteError(wrapSyscallError(writeSyscallName, e))
}

func (c *conn) coroWriteError(err error) error {
	if err != nil {
		return &OpError{Op: "write", Net: c.fd.net, Source: c.fd.laddr, Addr: c.fd.raddr, Err: err}
	}
	return nil
}
