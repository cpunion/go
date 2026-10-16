// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"internal/race"
	"runtime"
	"testing"
	"unsafe"
)

func TestStacklessCoroIOStorage(t *testing.T) {
	var token unsafe.Pointer
	if got := testing.AllocsPerRun(100, func() {
		token = runtime.NewStacklessCoroIOOperationForTest()
	}); got != 1 {
		t.Fatalf("new IO operation allocations = %v, want 1", got)
	}
	if !runtime.StacklessCoroIOOperationValidForTest(token) {
		t.Fatal("new IO operation does not embed its IO storage")
	}
	runtime.GC()
	if !runtime.StacklessCoroIOOperationValidForTest(token) {
		t.Fatal("embedded IO storage was not retained across GC")
	}
	runtime.KeepAlive(token)
}

func TestStacklessCoroIOStorageReuse(t *testing.T) {
	reused, cleared := runtime.CheckStacklessCoroIOReuseForTest()
	if reused != !race.Enabled {
		t.Fatalf("channel/IO/channel reuse = %t, want %t", reused, !race.Enabled)
	}
	if !cleared {
		t.Fatal("operation retained source-owned state after completion")
	}
}
