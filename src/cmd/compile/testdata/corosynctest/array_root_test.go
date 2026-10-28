// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package corosynctest

import (
	"runtime"
	"testing"
	"testing/synctest"
)

//go:noinline
func arrayRootRead(p *[5120]byte) int { return int(p[0]) + int(p[5119]) }

func arrayRootRecursive(depth, seed int) int {
	var data [5120]byte
	data[5119] = byte(seed)
	seed += 7
	if depth == 0 {
		runtime.Gosched()
		return arrayRootRead(&data)
	}
	got := arrayRootRecursive(depth-1, seed)
	return got + arrayRootRead(&data)
}

func TestArrayRootFallback(t *testing.T) {
	check := func(t *testing.T) {
		for _, depth := range []int{0, 1, 32, 513} {
			want := 0
			for i := 0; i <= depth; i++ {
				want += int(byte(19 + 7*i))
			}
			if got := arrayRootRecursive(depth, 19); got != want {
				t.Fatalf("depth %d: got %d, want %d", depth, got, want)
			}
		}
	}
	synctest.Test(t, check)
	t.Run("locked-thread", func(t *testing.T) {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		check(t)
	})
	t.Run("native", check)
}
