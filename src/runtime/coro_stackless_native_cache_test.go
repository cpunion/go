// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime_test

import (
	"runtime"
	"runtime/debug"
	"testing"
	"unsafe"
)

func TestStacklessCoroNativeIdleCacheGC(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	gcPercent := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gcPercent)
	runtime.StacklessCoroClearNativeTaskCachesForTest()
	runStacklessCoroSelfFrames(t, &stacklessCoroSelfFrameTracker{rootDepth: 4096})
	warm, overflow, _, _ := runtime.StacklessCoroNativePoolSelfFrameCacheForTest()
	if warm == 0 || overflow != 0 {
		t.Fatalf("initial cache = (%d, %d), want warm storage only", warm, overflow)
	}
	contexts := runtime.StacklessCoroNativePoolForTest()
	runtime.GC()
	first, _, _, _ := runtime.StacklessCoroNativePoolSelfFrameCacheForTest()
	if first != warm {
		t.Fatalf("first GC retained %d frame bytes, want %d", first, warm)
	}
	runtime.GC()
	second, overflow, _, _ := runtime.StacklessCoroNativePoolSelfFrameCacheForTest()
	tasks, overflowTasks := runtime.StacklessCoroNativePoolTaskCacheForTest()
	if second != 0 || overflow != 0 || tasks != 0 || overflowTasks != 0 {
		t.Fatalf("idle caches survived second GC: frame=(%d,%d), task=(%d,%d)",
			second, overflow, tasks, overflowTasks)
	}
	if got := runtime.StacklessCoroNativePoolForTest(); got != contexts {
		t.Fatalf("GC changed executor count: %d, want %d", got, contexts)
	}
	// Dropping idle payload must leave the executor safe to populate again.
	runStacklessCoroSelfFrames(t, &stacklessCoroSelfFrameTracker{rootDepth: 4096})
}

func TestStacklessCoroNativeCachePhaseReplacement(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	gcPercent := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gcPercent)
	defer func() { stacklessCoroFusedResumeDeepActive = nil }()
	runtime.StacklessCoroClearNativeTaskCachesForTest()
	self := func() {
		runStacklessCoroSelfFrames(t, &stacklessCoroSelfFrameTracker{rootDepth: 4096})
	}
	mutual := func() {
		tracker := new(stacklessCoroFusedResumeDeepTracker)
		runStacklessCoroFusedResumeDeep(512, tracker)
		if tracker.completed != 513 {
			t.Fatalf("completed %d mutual frames, want 513", tracker.completed)
		}
	}
	self()
	warm, _, _, _ := runtime.StacklessCoroNativePoolSelfFrameCacheForTest()
	// Repeated alternation must preserve the self-recursive working set.
	for range 3 {
		mutual()
		after, _, _, _ := runtime.StacklessCoroNativePoolSelfFrameCacheForTest()
		if after != warm {
			t.Fatalf("one interleaved mutual root changed self cache: %d, want %d", after, warm)
		}
		self()
	}
	// A sustained phase change can replace it, without increasing the budget.
	mutual()
	mutual()
	mutual()
	if allocations := testing.AllocsPerRun(3, mutual); allocations > 32 {
		t.Fatalf("mutual phase allocated %.0f objects after replacement, want <=32", allocations)
	}
	retained, overflow, limit, _ := runtime.StacklessCoroNativePoolSelfFrameCacheForTest()
	if retained == 0 || retained > limit || overflow != 0 {
		t.Fatalf("replacement cache=(%d,%d), want warm storage within %d bytes", retained, overflow, limit)
	}
	self()
	mutual()
	self()
}

// A burst must be reusable between roots, without retaining its peak storage
// through the first idle collection or keeping stale links after a trim.
func TestStacklessCoroNativeBurstCacheGC(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	gcPercent := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gcPercent)
	defer func() { stacklessCoroLargeSelfFrameActive = nil }()
	runtime.StacklessCoroClearNativeTaskCachesForTest()
	defer runtime.StacklessCoroClearNativeTaskCachesForTest()
	run := func() {
		tracker := new(stacklessCoroLargeSelfFrameTracker)
		runStacklessCoroLargeSelfFrames(2048, tracker)
		if tracker.total != 2049 {
			t.Fatalf("completed %d frames, want 2049", tracker.total)
		}
	}
	run()
	burst, overflow, _, ordinary := runtime.StacklessCoroNativePoolSelfFrameCacheForTest()
	direct := uintptr(runtime.StacklessCoroFrameChunkDirectCount) * unsafe.Sizeof(runtime.StacklessCoroLargeSelfFrameForTest{})
	if burst <= ordinary || burst > (4<<20)+direct || overflow != 0 {
		t.Fatalf("burst cache=(%d,%d), want >%d and <=4 MiB plus direct frames", burst, overflow, ordinary)
	}
	// A smaller whole root should reuse the retained chunks with only the
	// ordinary root/controller allocations, rather than hundreds of refills.
	small := func() {
		tracker := new(stacklessCoroLargeSelfFrameTracker)
		runStacklessCoroLargeSelfFrames(512, tracker)
		if tracker.total != 513 {
			t.Fatalf("completed %d frames", tracker.total)
		}
	}
	if n := testing.AllocsPerRun(3, small); n > 32 {
		t.Fatalf("warm burst allocated %.0f objects, want <=32", n)
	}
	contexts := runtime.StacklessCoroNativePoolForTest()
	runtime.GC()
	first, overflow, _, _ := runtime.StacklessCoroNativePoolSelfFrameCacheForTest()
	if first == 0 || first > ordinary+direct || first >= burst || overflow != 0 {
		t.Fatalf("first idle GC cache=(%d,%d), want within %d", first, overflow, ordinary+direct)
	}
	// Reuse the shortened linked list and refill it before checking expiry.
	small()
	runtime.GC()
	runtime.GC()
	second, overflow, _, _ := runtime.StacklessCoroNativePoolSelfFrameCacheForTest()
	if second != 0 || overflow != 0 {
		t.Fatalf("second idle GC cache=(%d,%d)", second, overflow)
	}
	if runtime.StacklessCoroNativePoolForTest() != contexts {
		t.Fatal("GC replaced native executors")
	}
	small()
}

func TestStacklessCoroNativeBurstCacheContexts(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	gcPercent := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gcPercent)
	for _, oversized := range []bool{false, true} {
		before, first, second, overflow, limit, active, reserved, afterTrim := runtime.StacklessCoroNativeBurstContextsForTest(oversized)
		if active > 8*(256<<10)+4*((4<<20)-(256<<10)) || reserved != 4 || afterTrim != 0 {
			t.Fatalf("active=%d reserved=%d afterTrim=%d", active, reserved, afterTrim)
		}
		if before <= limit || before > 4*(4<<20) || first > limit || second != 0 || overflow != 0 {
			t.Fatalf("oversized=%t: caches before=%d first=%d second=%d overflow=%d, idle limit=%d", oversized, before, first, second, overflow, limit)
		}
	}
}

func TestStacklessCoroNativeBurstReservationContention(t *testing.T) {
	previous := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(previous)
	gcPercent := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gcPercent)
	runtime.StacklessCoroClearNativeTaskCachesForTest()
	for range 3 {
		held, granted, remaining := runtime.StacklessCoroNativeBurstContendForTest()
		if held != 4 || granted != 4 || remaining != 0 {
			t.Fatalf("held=%d granted=%d remaining=%d", held, granted, remaining)
		}
	}
}

func TestStacklessCoroNativeBurstParkedRoot(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	gcPercent := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gcPercent)
	before, after, reserved := runtime.StacklessCoroNativeBurstParkForTest()
	if before <= 256<<10 || after > 256<<10 || reserved != 0 {
		t.Fatalf("parked cache before=%d after=%d reservations=%d", before, after, reserved)
	}
	if got := runtime.StacklessCoroNativeContextAllocationSizeForTest(); got > 208 {
		t.Fatalf("native context grew to %d allocator bytes", got)
	}
}
