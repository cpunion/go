// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime_test

import (
	"runtime"
	"runtime/debug"
	"testing"
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
