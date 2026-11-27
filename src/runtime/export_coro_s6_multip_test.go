// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "unsafe"

func StacklessCoroPooledGCountForTest() uint32 {
	return stacklessCoroPooledG.count.Load()
}

func StacklessCoroUnhostedCountForTest() int64 {
	return stacklessCoroUnhostedGoroutines()
}

func s6MultipDummyResume(unsafe.Pointer) uint8 {
	return stacklessCoroActionComplete
}

// CheckStacklessCoroPerPCacheTaskAndPendingForTest deterministically verifies:
// 1. Per-P pendingTypedTask put/take/takeAnyP/cancel across Ps.
// 2. Per-P cacheTask ring put/get/flush preserving s.freeFrameBytes accounting.
// 3. Capped executor wake signaling (stacklessCoroSendWakeCap).
func CheckStacklessCoroPerPCacheTaskAndPendingForTest() string {
	s := new(stacklessCoroScheduler)
	s.wake = make(chan struct{}, stacklessCoroWarmExecutorCount)
	lockInit(&s.lock, lockRankStacklessCoro)
	parent := &s.root
	parent.context.scheduler = s
	frameBuf := new([64]byte)
	frame := unsafe.Pointer(&frameBuf[0])

	t1 := new(stacklessCoroTask)
	t1.resume = s6MultipDummyResume
	t1.frameSize = 64
	t1.state = stacklessCoroTaskNew
	t1.context.frame = frame
	t1.setParent(parent)

	if !coroPTryPutPendingTypedTask(t1) {
		return "coroPTryPutPendingTypedTask failed on empty slot"
	}
	if coroPTryPutPendingTypedTask(t1) {
		return "coroPTryPutPendingTypedTask unexpectedly succeeded on occupied slot"
	}
	if got := coroPTakePendingTypedTask(parent, frame, s6MultipDummyResume); got != t1 {
		return "coroPTakePendingTypedTask did not return stored task"
	}
	if got := coroPTakePendingTypedTask(parent, frame, s6MultipDummyResume); got != nil {
		return "coroPTakePendingTypedTask returned task twice"
	}

	if !coroPTryPutPendingTypedTask(t1) {
		return "coroPTryPutPendingTypedTask failed for TakeAnyP check"
	}
	if got := coroPTakePendingTypedTaskAnyP(parent, frame, s6MultipDummyResume); got != t1 {
		if gomaxprocs > 1 {
			return "coroPTakePendingTypedTaskAnyP failed under GOMAXPROCS > 1"
		}
		if got2 := coroPTakePendingTypedTask(parent, frame, s6MultipDummyResume); got2 != t1 {
			return "local pendingTypedTask drain failed"
		}
	}
	if !coroPTryPutPendingTypedTask(t1) {
		return "coroPTryPutPendingTypedTask failed for Cancel check"
	}
	if got := coroPCancelPendingTypedTask(parent); got != t1 {
		return "coroPCancelPendingTypedTask did not return pending task"
	}

	const nTasks = 80
	s.cachedFrameBytes = uint16(nTasks * 64)
	s.cachedFrameTasks = nTasks
	for range nTasks {
		t := new(stacklessCoroTask)
		t.resume = s6MultipDummyResume
		t.frameSize = 64
		t.state = stacklessCoroTaskNew
		t.flags = stacklessCoroTaskCacheFrame
		t.context.frame = frame
		coroPCacheTaskPut(s, t)
	}
	if !coroPHasCacheTasks(s) {
		return "coroPHasCacheTasks reported false after puts"
	}
	gotCount := 0
	for range 16 {
		if got := coroPCacheTaskGet(s, s6MultipDummyResume, 64); got != nil {
			gotCount++
		}
	}
	if gotCount != 16 {
		return "coroPCacheTaskGet did not return expected 16 tasks"
	}
	coroPFlushCacheTasks(s)
	if s.freeFrameBytes != uint16((nTasks-16)*64) {
		return "coroPFlushCacheTasks did not flush remaining tasks to s.freeTasks"
	}
	s.freeTasks = nil
	s.freeFrameBytes = 0

	if !raceenabled {
		wake := make(chan struct{}, 4)
		for range 8 {
			stacklessCoroSendWakeCap(wake, 1, false)
		}
		if len(wake) != 1 {
			return "stacklessCoroSendWakeCap(1) buffered more than 1 token"
		}
		<-wake
		for range 8 {
			stacklessCoroSendWakeCap(wake, 2, true)
		}
		if len(wake) != 2 {
			return "stacklessCoroSendWakeCap(2) did not cap buffered tokens at 2"
		}
	}
	return ""
}
