// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && coropullcompare && (darwin || linux)

package runtime

import "unsafe"

// RunStacklessCoroPushComparisonFrameForTest bypasses public-root reuse so
// the controlled push and pull drivers have the same root allocation and
// lifetime. It is a measurement aid, not a supported execution mode.
func RunStacklessCoroPushComparisonFrameForTest(frame unsafe.Pointer,
	resume func(unsafe.Pointer) uint8) {
	runStacklessCoroComparisonSchedulerForTest(
		newStacklessCoroScheduler(frame, resume))
}

func runStacklessCoroComparisonSchedulerForTest(s *stacklessCoroScheduler) {
	if nativeScheduler := coroRunOnNativeStack(s); nativeScheduler != nil {
		s = nativeScheduler
	} else {
		s.run(false)
	}
	s.stopReplacementExecutors()
	s.releaseWake()
	s.finish()
}

// RunStacklessCoroPullFrameForTest runs the same resume ABI, task
// representation, typed frames, and operation sources as the push driver, but
// completion only rings the root doorbell. The root then polls its current
// structured leaf. It supports one structured await lineage, not independent
// spawn or blocking foreign calls. It is a measurement aid, not a supported
// execution mode.
func RunStacklessCoroPullFrameForTest(frame unsafe.Pointer,
	resume func(unsafe.Pointer) uint8) {
	s := newStacklessCoroScheduler(frame, resume)
	s.tail = &stacklessCoroPullComparisonMarker
	s.runnableState.Store(0)
	runStacklessCoroComparisonSchedulerForTest(s)
}
