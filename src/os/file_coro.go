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

// coroReadStart is the compiler-private entry used for a Read suspension.
// The common path retains the file's read lock until coroReadFinish. Lock
// contention alone falls back to the bounded blocking-read worker pool.
func (f *File) coroReadStart(ctx unsafe.Pointer, b []byte, n *int, err *error, direct *bool, status *uintptr) bool {
	*n = 0
	*err = nil
	*direct = false
	*status = 0
	if e := f.checkValid("read"); e != nil {
		*err = e
		return false
	}

	state, e := f.pfd.CoroReadStart(ctx, b, n, status)
	if state == poll.CoroReadFallback {
		poll.CoroCallRead(ctx, func() {
			count, readErr := f.read(b)
			*n = count
			*err = f.wrapErr("read", readErr)
		})
		return true
	}
	if state == poll.CoroReadWait {
		*direct = true
		return true
	}
	*err = f.wrapErr("read", e)
	return false
}

// coroReadFinish restores the public Read result after a direct coroutine
// operation. A fallback worker has already written the final result.
func (f *File) coroReadFinish(n *int, err *error, direct bool, status uintptr) {
	if !direct {
		return
	}
	e := f.pfd.CoroReadFinish(n, status)
	runtime.KeepAlive(f)
	*err = f.wrapErr("read", e)
}
