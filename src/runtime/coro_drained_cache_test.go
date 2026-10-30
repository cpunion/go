// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime_test

import (
	"runtime"
	"runtime/debug"
	"testing"
	"weak"
)

func TestStacklessCoroDrainedCacheOwnership(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	gc := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gc)
	runtime.StacklessCoroClearNativeTaskCachesForTest()
	defer runtime.StacklessCoroClearNativeTaskCachesForTest()
	if _, _, valid := runtime.StacklessCoroDrainedCacheForTest(false, 0); !valid {
		t.Fatal("root completion detached a cache while children remained live")
	}
	for _, delay := range []int{0, 1, 2} {
		keeper, token, valid := runtime.StacklessCoroDrainedCacheForTest(true, delay)
		if !valid {
			t.Fatalf("delay=%d: transfer changed ownership, frame charges or wake lifetime", delay)
		}
		ref := weak.Make((*byte)(token))
		token = nil
		if delay == 0 {
			runtime.GC()
			if ref.Value() == nil {
				t.Fatal("recently used task cache did not survive its first idle GC")
			}
		}
		runtime.GC()
		if ref.Value() != nil {
			t.Fatalf("delay=%d: old scheduler or late admission pinned an expired cache", delay)
		}
		if !runtime.StacklessCoroDrainedCacheSignalForTest(keeper) {
			t.Fatal("late producer lost its original scheduler wake")
		}
		runtime.KeepAlive(keeper)
	}
}

func TestStacklessCoroDrainedCacheOperations(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	gc := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gc)
	if !runtime.CheckStacklessCoroDrainedOperationsForTest() {
		t.Fatal("operation cache retired before the last child, after drainage, or after the idle GC")
	}
}
