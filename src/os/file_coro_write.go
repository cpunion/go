// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package os

import (
	"internal/poll"
	"runtime"
	"unsafe"
)

// coroWriteStart is the compiler-private entry used for a Write suspension.
// The direct path retains the file's write lock until coroWriteFinish.
func (f *File) coroWriteStart(ctx unsafe.Pointer, b []byte, n *int, err *error, direct *bool, status *uintptr) bool {
	*n = 0
	*err = nil
	*direct = false
	*status = 0
	if e := f.checkValid("write"); e != nil {
		*err = e
		return false
	}

	state, e := f.pfd.CoroWriteStart(ctx, b, n, status)
	if state == poll.CoroWriteFallback {
		poll.CoroCallWrite(ctx, func() {
			*n, *err = f.Write(b)
		})
		return true
	}
	if state == poll.CoroWriteWait {
		*direct = true
		return true
	}
	epipecheck(f, e)
	*err = f.wrapErr("write", e)
	return false
}

// coroWriteFinish restores the public Write result after a direct coroutine
// operation. Like FD.Write, the direct operation writes the whole buffer or
// returns an error. A fallback worker has already written the final result.
func (f *File) coroWriteFinish(n *int, err *error, direct bool, status uintptr) {
	if !direct {
		return
	}
	e := f.pfd.CoroWriteFinish(n, status)
	runtime.KeepAlive(f)
	epipecheck(f, e)
	*err = f.wrapErr("write", e)
}
