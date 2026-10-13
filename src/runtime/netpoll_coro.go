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
	return netpollCoroArm(pd, op, 'r')
}

func netpollCoroArm(pd *pollDesc, op *stacklessCoroOperation, mode int32) (bool, int) {
	if err := netpollcheckerr(pd, mode); err != pollNoError {
		return false, err
	}
	gpp := &pd.rg
	if mode == 'w' {
		gpp = &pd.wg
	}
	pointer := uintptr(unsafe.Pointer(op))
	if pointer&netpollCoroTagMask != 0 {
		throw("runtime: invalid stackless coroutine poll token")
	}
	token := pointer | netpollCoroTag
	for {
		old := gpp.Load()
		if old == pdReady {
			if gpp.CompareAndSwap(pdReady, pdNil) {
				return false, pollNoError
			}
		} else if old != pdNil {
			throw("runtime: double stackless coroutine poll wait")
		} else {
			// Publish the waiter count before the token. Readiness can consume
			// the token immediately after the compare-and-swap and return a
			// negative delta to its caller.
			netpollAdjustWaiters(1)
			if gpp.CompareAndSwap(pdNil, token) {
				return netpollCoroArmCheck(pd, op, mode)
			}
			netpollAdjustWaiters(-1)
		}
	}
}

// netpollCoroArmCheck rechecks errors after publishing the waiter. Close and
// deadline updates publish their error before looking for a waiter, so the
// initial check alone can miss an update that precedes token installation.
func netpollCoroArmCheck(pd *pollDesc, op *stacklessCoroOperation, mode int32) (bool, int) {
	if err := netpollcheckerr(pd, mode); err != pollNoError {
		if netpollCoroClaim(pd, op, mode) {
			return false, err
		}
		// Readiness or an error notification already claimed the token.
		// Its queued completion owns op, even if the error is now visible.
	}
	return true, pollNoError
}

// netpollCoroClaim removes an armed operation before netpoll has consumed it.
// The caller may retry the nonblocking I/O after a successful claim.
func netpollCoroClaim(pd *pollDesc, op *stacklessCoroOperation, mode int32) bool {
	gpp := &pd.rg
	if mode == 'w' {
		gpp = &pd.wg
	}
	token := uintptr(unsafe.Pointer(op)) | netpollCoroTag
	if !gpp.CompareAndSwap(token, pdNil) {
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
