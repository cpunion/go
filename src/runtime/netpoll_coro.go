// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

const (
	netpollCoroEnabled = true
	netpollCoroTagMask = uintptr(3)
	netpollCoroTag     = uintptr(3)
)

// netpollCoroReadArm installs op as the logical read waiter for pd. It
// reports false if readiness raced with installation and the caller should
// retry the nonblocking read.
func netpollCoroReadArm(pd *pollDesc, op *stacklessCoroOperation) (bool, int) {
	if err := netpollcheckerr(pd, 'r'); err != pollNoError {
		return false, err
	}
	pointer := uintptr(unsafe.Pointer(op))
	if pointer&netpollCoroTagMask != 0 {
		throw("runtime: invalid stackless coroutine poll token")
	}
	token := pointer | netpollCoroTag
	for {
		old := pd.rg.Load()
		if old == pdReady {
			if pd.rg.CompareAndSwap(pdReady, pdNil) {
				return false, pollNoError
			}
		} else if old != pdNil {
			throw("runtime: double stackless coroutine poll wait")
		} else {
			// Publish the waiter count before the token. Readiness can consume
			// the token immediately after the compare-and-swap and return a
			// negative delta to its caller.
			netpollAdjustWaiters(1)
			if pd.rg.CompareAndSwap(pdNil, token) {
				return true, pollNoError
			}
			netpollAdjustWaiters(-1)
		}
	}
}

// netpollCoroReadClaim removes an armed coroutine read before netpoll has
// consumed it. The caller may retry the nonblocking read after a successful
// claim.
func netpollCoroReadClaim(pd *pollDesc, op *stacklessCoroOperation) bool {
	token := uintptr(unsafe.Pointer(op)) | netpollCoroTag
	if !pd.rg.CompareAndSwap(token, pdNil) {
		return false
	}
	netpollAdjustWaiters(-1)
	return true
}

//go:nowritebarrier
func netpollCoroDispatch(token uintptr) *g {
	return stacklessCoroNetpollReady(
		(*stacklessCoroOperation)(unsafe.Pointer(token)))
}
