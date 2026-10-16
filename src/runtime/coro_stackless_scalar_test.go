// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"internal/race"
	"runtime"
	"strconv"
	"testing"
	"unsafe"
)

func TestStacklessCoroScalarFrames(t *testing.T) {
	for _, size := range []uintptr{1, 4, 8, 16} {
		t.Run(strconv.Itoa(int(size)), func(t *testing.T) {
			for _, count := range []int{0, 1, 7, 8, 9, 10, 11, 12, 13, 16, 64} {
				paired, valid := runtime.CheckStacklessCoroScalarFramesForTest(size, count)
				want := 0
				if !race.Enabled && size == 4 && size < unsafe.Sizeof(uintptr(0)) && count > 8 {
					want = count - 8
				}
				if !valid || paired != want {
					t.Fatalf("size=%d count=%d: paired=%d valid=%t, want %d valid frames",
						size, count, paired, valid, want)
				}
			}
		})
	}
}

func TestStacklessCoroScalarFrameMix(t *testing.T) {
	if race.Enabled || unsafe.Sizeof(uintptr(0)) == 4 {
		t.Skip("scalar pairing is deliberately disabled on these paths")
	}
	if !runtime.CheckStacklessCoroScalarFrameMixForTest() {
		t.Fatal("scalar task slots leaked into an unrelated frame or lost pointer roots")
	}
	if !runtime.CheckStacklessCoroDiscardScalarFramesForTest() {
		t.Fatal("discarded scalar task slots retained their frames or list links")
	}
}
