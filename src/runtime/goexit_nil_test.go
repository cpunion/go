// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"runtime"
	"testing"
)

//go:noinline
func growGoexitNilDeferStack(n int) int {
	var pad [1024]byte
	pad[n%len(pad)] = byte(n)
	if n == 0 {
		runtime.GC()
		return 0
	}
	result := growGoexitNilDeferStack(n - 1)
	runtime.KeepAlive(&pad)
	return result + int(pad[n%len(pad)])
}

func TestGoexitNilDeferStackCopy(t *testing.T) {
	done := make(chan struct{})
	var caught any
	returned := false
	go func() {
		defer close(done)
		defer func() {
			// Move the stack while the nil-call panic is still active.
			// Recovery must resume Goexit using its relocated panic record.
			if growGoexitNilDeferStack(32) != 32*33/2 {
				t.Error("stack contents changed")
			}
			caught = recover()
		}()
		var f func()
		defer f()
		runtime.Goexit()
		returned = true
	}()
	<-done
	if _, ok := caught.(runtime.Error); !ok {
		t.Fatalf("recovered %v (%T), want nil-call runtime.Error", caught, caught)
	}
	if returned {
		t.Fatal("Goexit returned after recovering a nil deferred call")
	}
}
