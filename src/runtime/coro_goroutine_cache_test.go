// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime_test

import (
	"internal/testenv"
	"os"
	"runtime"
	"runtime/debug"
	"testing"
	"weak"
)

func TestStacklessCoroRecordCacheLifecycle(t *testing.T) {
	const child = "GO_CORO_RECORD_CACHE_LIFECYCLE"
	if os.Getenv(child) == "" {
		testenv.MustHaveExec(t)
		cmd := testenv.Command(t, os.Args[0], "-test.run=^TestStacklessCoroRecordCacheLifecycle$")
		cmd.Env = append(cmd.Environ(), child+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("record cache subprocess: %v\n%s", err, out)
		}
		return
	}
	oldGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(oldGC)
	runtime.GC()
	runtime.GC()
	ptr, capacity := runtime.StacklessCoroFillRecordCacheForTest()
	ref := weak.Make((*byte)(ptr))
	ptr = nil
	if young, old := runtime.StacklessCoroRecordCacheCountsForTest(); capacity == 0 || young != capacity || old != 0 {
		t.Fatalf("filled record cache = (%d, %d), want (%d, 0)", young, old, capacity)
	}
	runtime.GC()
	if young, old := runtime.StacklessCoroRecordCacheCountsForTest(); young != 0 || old != capacity || ref.Value() == nil {
		t.Fatalf("record cache after first GC = (%d, %d), want (0, %d) with live records", young, old, capacity)
	}
	runtime.GC()
	if young, old := runtime.StacklessCoroRecordCacheCountsForTest(); young != 0 || old != 0 || ref.Value() != nil {
		t.Fatalf("record cache after second GC = (%d, %d), want (0, 0) with released records", young, old)
	}
}

func TestStacklessCoroGoroutineBucketResize(t *testing.T) {
	if !runtime.StacklessCoroCheckBucketResizeForTest() {
		t.Fatal("in-place registry resize lost records or left links in unused buckets")
	}
}
