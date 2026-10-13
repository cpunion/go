// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

// StacklessCoroPollArmRaceForTest constructs the state after an error races
// with token installation, without installing test hooks in the poller. If
// completion is nonzero, a notification has also taken ownership of the token.
func StacklessCoroPollArmRaceForTest(mode int32, pollErr, completion int) (waiting bool, errno int, tokenMatches, statePreserved bool) {
	pd := pollcache.alloc()
	lock(&pd.lock)
	pd.closing = false
	pd.rd = 0
	pd.wd = 0
	pd.rg.Store(pdNil)
	pd.wg.Store(pdNil)
	pd.setEventErr(false, 0)
	pd.publishInfo()
	unlock(&pd.lock)
	defer pollcache.free(pd)
	if err := netpollcheckerr(pd, mode); err != pollNoError {
		throw("runtime: unexpected initial coroutine arm test error")
	}

	// The error is published after the initial check but before the waiter
	// is installed. An unblock in this interval would find no waiter.
	lock(&pd.lock)
	if pollErr == pollErrClosing {
		pd.closing = true
	} else if pollErr == pollErrTimeout {
		if mode == 'r' {
			pd.rd = -1
		} else {
			pd.wd = -1
		}
	}
	pd.publishInfo()
	unlock(&pd.lock)
	op := Escape(new(stacklessCoroOperation))
	token := uintptr(unsafe.Pointer(op)) | netpollCoroTag
	gpp := &pd.rg
	if mode == 'w' {
		gpp = &pd.wg
	}
	netpollAdjustWaiters(1)
	gpp.Store(token)
	if completion != 0 {
		// Model netpollunblock consuming the token without publishing a
		// fabricated operation to the real completion queue.
		if completion == 1 {
			gpp.Store(pdNil)
		} else {
			gpp.Store(pdReady)
		}
		netpollAdjustWaiters(-1)
	}
	waiting, errno = netpollCoroArmCheck(pd, op, mode)
	state := gpp.Load()
	tokenMatches = state == token
	wantState := uintptr(pdNil)
	if completion == 2 {
		wantState = pdReady
	} else if pollErr == pollNoError && completion == 0 {
		wantState = token
	}
	statePreserved = state == wantState
	if tokenMatches {
		if !netpollCoroClaim(pd, op, mode) {
			throw("runtime: lost coroutine arm test token")
		}
	}
	KeepAlive(op)
	return
}
