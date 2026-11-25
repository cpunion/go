// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

// RunManagedStacklessCoroRootForTest runs a private root directly through the
// managed runInitial path (bypassing native fixed-stack dispatch) so managed
// run-scope reuse and directInitialRoot yield/completion invariants are tested
// on every platform.
func RunManagedStacklessCoroRootForTest(resume func(unsafe.Pointer) uint8) (quiescent bool, wakeLen int) {
	s := acquireStacklessCoroRootScheduler(nil, resume, 0)
	wake := s.wake
	s.runInitial()
	wakeLen = len(wake)
	quiescent = finishStacklessCoroRootScheduler(s)
	return quiescent, wakeLen
}

// StacklessCoroGStateSummaryForTest reports whether the current goroutine has
// a retained stacklessCoroGState and whether all of its transient fields are clear.
func StacklessCoroGStateSummaryForTest() (present, clean bool) {
	state := getg().stacklessCoro
	if state == nil {
		return false, true
	}
	return true, state.native == nil && state.deferState == nil &&
		state.blockingScheduler == nil && state.activeSource == nil &&
		!state.sourceTransition
}
