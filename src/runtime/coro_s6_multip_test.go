// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime_test

import (
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

func TestStacklessCoroSchedulerSizeContract(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("64-bit size contract")
	}
	if got := runtime.StacklessCoroSchedulerSizeForTest(); got != 200 {
		t.Fatalf("sizeof(stacklessCoroScheduler) = %d, want 200", got)
	}
}

func TestStacklessCoroPerPCacheTaskAndPending(t *testing.T) {
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)
	if msg := runtime.CheckStacklessCoroPerPCacheTaskAndPendingForTest(); msg != "" {
		t.Fatal(msg)
	}
}

func TestStacklessCoroMultiPCloseBatch(t *testing.T) {
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)
	const n = 192
	for iter := range 5 {
		ch := make(chan int)
		var started, completed atomic.Int32
		var rootState int
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch rootState {
			case 0:
				rootState = 1
				for range n {
					phase := 0
					var val int
					var ok bool
					runtime.SpawnStacklessCoroForTest(ctx, func(wctx unsafe.Pointer) uint8 {
						if phase == 0 {
							phase = 1
							started.Add(1)
							if runtime.RecvIntStacklessCoroForTest(wctx, ch, &val, &ok) {
								return runtime.StacklessCoroActionWait
							}
						}
						if ok || val != 0 {
							completed.Add(100000)
						}
						completed.Add(1)
						return runtime.StacklessCoroActionComplete
					})
				}
				return runtime.StacklessCoroActionYield
			case 1:
				if int(started.Load()) < n {
					return runtime.StacklessCoroActionYield
				}
				rootState = 2
				close(ch)
				return runtime.StacklessCoroActionYield
			default:
				if int(completed.Load()) < n {
					return runtime.StacklessCoroActionYield
				}
				return runtime.StacklessCoroActionComplete
			}
		})
		if got := int(completed.Load()); got != n {
			t.Fatalf("iter %d: completed %d/%d close-batch waiters", iter, got, n)
		}
	}
}

func TestStacklessCoroPooledGoroutineGCAgingAndShrink(t *testing.T) {
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)
	oldGC := debug.SetGCPercent(1)
	defer debug.SetGCPercent(oldGC)

	const children = 256
	for iter := range 4 {
		gate := make(chan int)
		var started, done atomic.Int32
		rootState := 0
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch rootState {
			case 0:
				rootState = 1
				for range children {
					step := 0
					var val int
					var ok bool
					runtime.SpawnStacklessCoroForTest(ctx, func(wctx unsafe.Pointer) uint8 {
						if step == 0 {
							step = 1
							started.Add(1)
							if runtime.RecvIntStacklessCoroForTest(wctx, gate, &val, &ok) {
								return runtime.StacklessCoroActionWait
							}
						}
						done.Add(1)
						return runtime.StacklessCoroActionComplete
					})
				}
				return runtime.StacklessCoroActionYield
			case 1:
				if started.Load() < children {
					return runtime.StacklessCoroActionYield
				}
				rootState = 2
				close(gate)
				return runtime.StacklessCoroActionYield
			default:
				if done.Load() < children {
					return runtime.StacklessCoroActionYield
				}
				return runtime.StacklessCoroActionComplete
			}
		})
		if got := done.Load(); got != children {
			t.Fatalf("iter %d: completed %d/%d children", iter, got, children)
		}
		if iter == 1 {
			runtime.GOMAXPROCS(1)
			runtime.GOMAXPROCS(4)
		}
	}

	debug.SetGCPercent(oldGC)
	// A replacement executor may still be finishing completeTask for the last
	// child after done.Add(1) lets the root return; wait briefly for drainage.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.StacklessCoroUnhostedCountForTest() != 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if got := runtime.StacklessCoroUnhostedCountForTest(); got != 0 {
		t.Fatalf("unhosted stackless goroutines after drain = %d, want 0", got)
	}
	// Two GC cycles expire idle pooled goroutine records back to 0.
	runtime.GC()
	runtime.GC()
	runtime.GC()
	if got := runtime.StacklessCoroPooledGCountForTest(); got != 0 {
		t.Fatalf("pooled goroutine slots after 3 GCs = %d, want 0", got)
	}
}
