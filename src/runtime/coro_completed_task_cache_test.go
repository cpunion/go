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
