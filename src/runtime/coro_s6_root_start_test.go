// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime_test

import (
	"internal/race"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
)

func TestStacklessCoroPrivateRootFastReuseAndZeroAllocs(t *testing.T) {
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)

	// Exercise private root direct acquire/takeInitialRoot/release across Ps.
	var wg sync.WaitGroup
	var total atomic.Int64
	for g := range 4 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range 200 {
				step := 0
				runtime.RunStacklessCoroForTest(func(unsafe.Pointer) uint8 {
					if step < 2 {
						step++
						return runtime.StacklessCoroActionYield
					}
					total.Add(1)
					return runtime.StacklessCoroActionComplete
				})
				if i == 100 && id == 0 {
					runtime.GC()
				}
			}
		}(g)
	}
	wg.Wait()
	if got := total.Load(); got != 800 {
		t.Fatalf("completed private roots = %d, want 800", got)
	}

	if !race.Enabled {
		step := 0
		resume := func(unsafe.Pointer) uint8 {
			if step == 0 {
				step = 1
				return runtime.StacklessCoroActionYield
			}
			step = 0
			return runtime.StacklessCoroActionComplete
		}
		runtime.RunStacklessCoroForTest(resume)
		if got := testing.AllocsPerRun(200, func() {
			runtime.RunStacklessCoroForTest(resume)
		}); got != 0 {
			t.Fatalf("private root RunStacklessCoroForTest allocs = %v, want 0", got)
		}
	}
}
