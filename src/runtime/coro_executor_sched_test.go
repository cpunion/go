// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

func TestStacklessCoroExecutorAdmissionBound(t *testing.T) {
	prev := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(prev)

	var maxAdmittedP2 atomic.Uint32
	var done atomic.Int32
	const tasks = 32
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		for range tasks {
			runtime.SpawnStacklessCoroForTest(ctx, func(cctx unsafe.Pointer) uint8 {
				_, _, admitted := runtime.StacklessCoroExecutorCountsForTest(cctx)
				for {
					cur := maxAdmittedP2.Load()
					if admitted <= cur || maxAdmittedP2.CompareAndSwap(cur, admitted) {
						break
					}
				}
				done.Add(1)
				return runtime.StacklessCoroActionComplete
			})
		}
		return runtime.StacklessCoroActionComplete
	})
	deadline := time.Now().Add(5 * time.Second)
	for done.Load() < tasks && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if done.Load() != tasks {
		t.Fatalf("completed = %d, want %d", done.Load(), tasks)
	}
	if got := maxAdmittedP2.Load(); got > 1 {
		t.Fatalf("GOMAXPROCS=2 admitted warm replacements = %d, want <= 1", got)
	}
}

func TestStacklessCoroSignalIfWaiting(t *testing.T) {
	if !runtime.StacklessCoroSignalIfWaitingForTest() {
		t.Fatal("signalIfWaiting suppression/wakeup check failed")
	}
}

func TestStacklessCoroExecutorDeferredYieldFairness(t *testing.T) {
	for _, procs := range []int{1, 2} {
		t.Run("P"+string(rune('0'+procs)), func(t *testing.T) {
			prev := runtime.GOMAXPROCS(procs)
			defer runtime.GOMAXPROCS(prev)

			var peerTicks atomic.Int32
			peerStarted := make(chan struct{})
			stopPeer := make(chan struct{})
			peerDone := make(chan struct{})
			go func() {
				defer close(peerDone)
				close(peerStarted)
				for {
					select {
					case <-stopPeer:
						return
					default:
						peerTicks.Add(1)
						runtime.Gosched()
					}
				}
			}()
			<-peerStarted

			const workers = 8
			const maxYields = 200000
			var completed atomic.Int32
			var ticksBefore int32
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				ticksBefore = peerTicks.Load()
				for range workers {
					count := 0
					runtime.SpawnStacklessCoroForTest(ctx, func(unsafe.Pointer) uint8 {
						if peerTicks.Load() == ticksBefore && count < maxYields {
							count++
							return runtime.StacklessCoroActionYield
						}
						completed.Add(1)
						return runtime.StacklessCoroActionComplete
					})
				}
				return runtime.StacklessCoroActionComplete
			})
			deadline := time.Now().Add(5 * time.Second)
			for completed.Load() < workers && time.Now().Before(deadline) {
				runtime.Gosched()
			}
			close(stopPeer)
			<-peerDone
			if completed.Load() != workers {
				t.Fatalf("completed = %d, want %d", completed.Load(), workers)
			}
			if peerTicks.Load() == ticksBefore {
				t.Fatal("ordinary peer goroutine made zero progress during coroutine yields")
			}
		})
	}
}

var coroGCProgressSinks [][]byte

func TestStacklessCoroSinglePGCProgress(t *testing.T) {
	prev := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prev)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	const workers = 16
	const rounds = 128
	sinks := make([][]byte, workers)
	coroGCProgressSinks = sinks
	defer func() { coroGCProgressSinks = nil }()
	var completed atomic.Int32
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		for w := range workers {
			idx := w
			step := 0
			runtime.SpawnStacklessCoroForTest(ctx, func(unsafe.Pointer) uint8 {
				if step < rounds {
					step++
					sinks[idx] = make([]byte, 8<<10)
					return runtime.StacklessCoroActionYield
				}
				completed.Add(1)
				return runtime.StacklessCoroActionComplete
			})
		}
		return runtime.StacklessCoroActionComplete
	})
	deadline := time.Now().Add(5 * time.Second)
	for completed.Load() < workers && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if completed.Load() != workers {
		t.Fatalf("completed = %d, want %d", completed.Load(), workers)
	}
	runtime.ReadMemStats(&after)
	if after.NumGC == before.NumGC {
		t.Fatalf("expected GC to make progress during allocating coroutine yields (NumGC=%d)", after.NumGC)
	}
}
