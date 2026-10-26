// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime_test

import (
	"internal/race"
	"runtime"
	"testing"
	"time"
	"unsafe"
)

func TestStacklessCoroTypedTaskFrames(t *testing.T) {
	if !runtime.CheckStacklessCoroTypedTaskFrameTypesForTest() {
		t.Fatal("combined task/frame pointer map or rejected-layout contract failed")
	}
	const count = 513
	valid, combined, finalizerToken := runtime.CheckStacklessCoroTypedTaskFramesForTest(count)
	t.Logf("combined task/frame allocations = %d of %d", combined, count)
	if !valid {
		t.Fatal("combined task/frame payload, completion or clearing contract failed")
	}
	if unsafe.Sizeof(uintptr(0)) == 8 && !race.Enabled && combined == 0 {
		t.Fatal("no actual combined allocation beyond the cache prefix")
	}
	if (unsafe.Sizeof(uintptr(0)) != 8 || race.Enabled) && combined != 0 {
		t.Fatal("combined allocation used on an excluded target/mode")
	}
	deadline := time.Now().Add(10 * time.Second)
	for runtime.StacklessCoroTypedTaskFrameFinalizedForTest(finalizerToken) != count && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
	if got := runtime.StacklessCoroTypedTaskFrameFinalizedForTest(finalizerToken); got != count {
		t.Fatalf("finalized payloads = %d, want %d", got, count)
	}
}
