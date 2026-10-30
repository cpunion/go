// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package runtime_test

import (
	"internal/race"
	"runtime"
	"runtime/debug"
	"testing"
	"weak"
)

func TestStacklessCoroCompletedTaskCache(t *testing.T) {
	if race.Enabled {
		t.Skip("task identities must remain distinct under race")
	}
	runtime.GC()
	runtime.GC()
	old := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(old)
	if !runtime.CheckStacklessCoroCompletedTaskCacheForTest() {
		t.Fatal("bitmap isolation, group reuse, payload clearing, or await/reservation/Goexit exclusion failed")
	}
	if got := runtime.StacklessCoroSharedPayloadCacheBytesForTest(); got > 16<<20 {
		t.Fatalf("shared cache uses %d bytes, exceeds16MiB", got)
	}
}

//go:noinline
func completedTaskCacheWeakToken() weak.Pointer[byte] {
	return weak.Make((*byte)(runtime.StacklessCoroCompletedTaskCacheTokenForTest()))
}

func TestStacklessCoroCompletedTaskCacheGC(t *testing.T) {
	if race.Enabled {
		t.Skip("task cache is excluded under race")
	}
	runtime.GC()
	runtime.GC()
	token := completedTaskCacheWeakToken()
	runtime.GC()
	if token.Value() == nil {
		t.Fatal("recently completed allocation did not survive first GC")
	}
	runtime.GC()
	if token.Value() != nil {
		t.Fatal("idle allocation survived second GC")
	}
}

//go:noinline
func completedTaskCacheWeakPayload() weak.Pointer[byte] {
	return weak.Make((*byte)(runtime.StacklessCoroCompletedTaskCachePayloadForTest()))
}

func TestStacklessCoroCompletedTaskCachePayload(t *testing.T) {
	if race.Enabled {
		t.Skip("task cache is excluded under race")
	}
	runtime.GC()
	runtime.GC()
	payload := completedTaskCacheWeakPayload()
	runtime.GC()
	if payload.Value() != nil {
		t.Fatal("cached completed frame retained its payload")
	}
}

func TestStacklessCoroCompletedTaskCacheBudget(t *testing.T) {
	if race.Enabled {
		t.Skip("task cache is excluded under race")
	}
	runtime.GC()
	runtime.GC()
	old := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(old)
	defer func() { runtime.GC(); runtime.GC() }()
	const perGeneration = 2048 * 64
	first, bytes := runtime.StacklessCoroCompletedTaskCacheFillForTest(perGeneration + 1024)
	if first != perGeneration || bytes > 16<<20 {
		t.Fatalf("first generation count=%d bytes=%d", first, bytes)
	}
	runtime.GC()
	total, bytes := runtime.StacklessCoroCompletedTaskCacheFillForTest(perGeneration + 1024)
	if total <= first || total >= 2*perGeneration || bytes > 16<<20 || bytes+64 <= 16<<20 {
		t.Fatalf("two generations count=%d bytes=%d, want budget-limited admission", total, bytes)
	}
}

func TestStacklessCoroCompletedTaskCacheConcurrent(t *testing.T) {
	if race.Enabled {
		t.Skip("task cache is excluded under race")
	}
	if !runtime.CheckStacklessCoroCompletedTaskCacheConcurrentForTest() {
		t.Fatal("concurrent task leases, clearing, or budget failed across GC")
	}
}
