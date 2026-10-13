// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"context"
	"internal/race"
	"internal/testenv"
	"io"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

func TestStacklessCoroSemaphore(t *testing.T) {
	if mode := os.Getenv("GO_TEST_CORO_SEMAPHORE"); mode != "" {
		switch mode {
		case "fast", "retry", "profile", "mutexprofile":
			testStacklessCoroSemaphoreRetry(t, mode)
		case "fifo", "lifo":
			testStacklessCoroSemaphoreMixed(t, mode == "lifo")
		case "contention", "handoff":
			testStacklessCoroSemaphoreContention(t, mode == "handoff")
		case "sibling":
			testStacklessCoroSemaphoreSibling(t)
		default:
			t.Fatalf("unknown semaphore case %q", mode)
		}
		return
	}
	for _, procs := range []int{1, 4} {
		for _, mode := range []string{"fast", "retry", "profile", "mutexprofile", "fifo", "lifo", "contention", "handoff", "sibling"} {
			t.Run(mode+"/P"+strconv.Itoa(procs), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := testenv.CommandContext(t, ctx, os.Args[0],
					"-test.run=^TestStacklessCoroSemaphore$")
				cmd.Env = append(cmd.Environ(), "GO_TEST_CORO_SEMAPHORE="+mode,
					"GOMAXPROCS="+strconv.Itoa(procs))
				if data, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("semaphore case: %v\n%s", err, data)
				}
			})
		}
	}
}

func testStacklessCoroSemaphoreRetry(t *testing.T, mode string) {
	var sema uint32
	var state unsafe.Pointer
	if mode == "fast" {
		sema = 1
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if runtime.SemacquireMutexStacklessCoroForTest(ctx, &sema, false, &state) {
				panic("ready semaphore blocked")
			}
			return runtime.StacklessCoroActionComplete
		})
		if sema != 0 || state != nil {
			t.Fatal("fast acquisition retained a token or waiter")
		}
		ctx := runtime.NewStacklessCoroSemaphoreContextForTest()
		if allocs := testing.AllocsPerRun(100, func() {
			sema = 1
			waiting := runtime.SemacquireMutexStacklessCoroForTest(ctx, &sema, false, &state)
			if waiting || sema != 0 || state != nil {
				t.Fatal("ready acquisition did not consume the token without state")
			}
		}); allocs != 0 {
			t.Fatalf("ready acquisition allocated %v objects, want 0", allocs)
		}
		return
	}
	if mode == "profile" {
		runtime.SetBlockProfileRate(1)
		defer runtime.SetBlockProfileRate(0)
	}
	if mode == "profile" || mode == "mutexprofile" {
		old := runtime.SetMutexProfileFraction(1)
		defer runtime.SetMutexProfileFraction(old)
	}
	step := 0
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		waiting := runtime.SemacquireMutexStacklessCoroForTest(ctx, &sema, false, &state)
		switch step {
		case 0:
			if !waiting || state == nil {
				panic("empty semaphore did not queue")
			}
			runtime.Semrelease1(&sema, false, 0)
			// A normal wakeup does not reserve ownership. Steal the token
			// before this resume returns Wait, forcing an early-ready retry.
			if !atomic.CompareAndSwapUint32(&sema, 1, 0) {
				panic("normal wakeup reserved a token")
			}
			step++
			return runtime.StacklessCoroActionWait
		case 1:
			if !waiting || state == nil {
				panic("stolen token completed the acquisition")
			}
			ordinary, logical := runtime.SemaphoreWaitersStacklessCoroForTest(&sema)
			if ordinary != 0 || logical != 1 {
				panic("retry did not rejoin the semaphore queue")
			}
			runtime.Semrelease1(&sema, true, 0)
			if atomic.CompareAndSwapUint32(&sema, 1, 0) {
				panic("handoff token was stolen")
			}
			step++
			return runtime.StacklessCoroActionWait
		case 2:
			if waiting || state != nil {
				panic("handoff did not finish the acquisition")
			}
			return runtime.StacklessCoroActionComplete
		}
		panic("unexpected semaphore resume")
	})
	if step != 2 || sema != 0 {
		t.Fatalf("retry result = (step %d, sema %d), want (2, 0)", step, sema)
	}
	ordinary, logical := runtime.SemaphoreWaitersStacklessCoroForTest(&sema)
	if ordinary != 0 || logical != 0 {
		t.Fatal("completed acquisition retained a queued waiter")
	}
	if mode == "profile" {
		if n, _ := runtime.BlockProfile(nil); n == 0 {
			t.Fatal("logical semaphore wait did not record a block event")
		}
	}
	if mode == "profile" || mode == "mutexprofile" {
		if n, _ := runtime.MutexProfile(nil); n == 0 {
			t.Fatal("logical semaphore release did not record a mutex event")
		}
	}
}

func testStacklessCoroSemaphoreMixed(t *testing.T, lifo bool) {
	runtime.SetBlockProfileRate(1)
	defer runtime.SetBlockProfileRate(0)
	old := runtime.SetMutexProfileFraction(1)
	defer runtime.SetMutexProfileFraction(old)
	var sema uint32
	done := make(chan int, 3)
	go func() {
		runtime.SemacquireMutexOrdinaryForTest(&sema, false, 0)
		done <- 1
	}()
	waitStacklessCoroSemaphoreQueue(&sema, 1, 0)
	go func() {
		var state unsafe.Pointer
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if runtime.SemacquireMutexStacklessCoroForTest(ctx, &sema, lifo, &state) {
				return runtime.StacklessCoroActionWait
			}
			done <- 2
			return runtime.StacklessCoroActionComplete
		})
	}()
	waitStacklessCoroSemaphoreQueue(&sema, 1, 1)
	go func() {
		runtime.SemacquireMutexOrdinaryForTest(&sema, false, 0)
		done <- 3
	}()
	waitStacklessCoroSemaphoreQueue(&sema, 2, 1)
	// Both kinds of waiter are retained by the same scanned semtable tree.
	runtime.GC()
	// The ordinary goroutine leak detector must tolerate a shared semaphore
	// whose treap includes logical waiters with no parked g. The semaphore is
	// still reachable here. This does not test logical-task leak detection.
	if err := pprof.Lookup("goroutineleak").WriteTo(io.Discard, 2); err != nil {
		t.Fatal(err)
	}
	want := []int{1, 2, 3}
	if lifo {
		want = []int{2, 1, 3}
	}
	for _, id := range want {
		runtime.Semrelease1(&sema, true, 0)
		if got := <-done; got != id {
			t.Fatalf("wake order = %d, want %d", got, id)
		}
	}
	waitStacklessCoroSemaphoreQueue(&sema, 0, 0)
	if n, _ := runtime.BlockProfile(nil); n == 0 {
		t.Fatal("mixed semaphore wait did not record a block event")
	}
	if n, _ := runtime.MutexProfile(nil); n == 0 {
		t.Fatal("mixed semaphore release did not record a mutex event")
	}
}

func waitStacklessCoroSemaphoreQueue(sema *uint32, ordinary, logical int) {
	for {
		o, l := runtime.SemaphoreWaitersStacklessCoroForTest(sema)
		if o == ordinary && l == logical {
			return
		}
		runtime.Gosched()
	}
}

func testStacklessCoroSemaphoreContention(t *testing.T, handoff bool) {
	const workers, iterations = 4, 200
	sema := uint32(1)
	var inside atomic.Int32
	var count int
	var wg sync.WaitGroup
	critical := func() {
		race.Acquire(unsafe.Pointer(&sema))
		if inside.Add(1) != 1 {
			panic("semaphore allowed overlapping acquisitions")
		}
		count++
		runtime.Gosched()
		if inside.Add(-1) != 0 {
			panic("semaphore critical section overlapped")
		}
		race.Release(unsafe.Pointer(&sema))
		runtime.Semrelease1(&sema, handoff, 0)
	}
	for id := 0; id < workers; id++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id%2 == 0 {
				for range iterations {
					runtime.SemacquireMutexOrdinaryForTest(&sema, false, 0)
					critical()
				}
				return
			}
			var state unsafe.Pointer
			completed := 0
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				if runtime.SemacquireMutexStacklessCoroForTest(ctx, &sema, false, &state) {
					return runtime.StacklessCoroActionWait
				}
				critical()
				completed++
				if completed == iterations {
					return runtime.StacklessCoroActionComplete
				}
				return runtime.StacklessCoroActionYield
			})
		}()
	}
	wg.Wait()
	if count != workers*iterations || sema != 1 {
		t.Fatalf("contention result = (%d, %d), want (%d, 1)", count, sema, workers*iterations)
	}
}

func testStacklessCoroSemaphoreSibling(t *testing.T) {
	var sema uint32
	var state unsafe.Pointer
	var siblingRan atomic.Bool
	var acquired atomic.Bool
	waiter := func(ctx unsafe.Pointer) uint8 {
		if runtime.SemacquireMutexStacklessCoroForTest(ctx, &sema, false, &state) {
			return runtime.StacklessCoroActionWait
		}
		if !siblingRan.Load() {
			panic("semaphore waiter acquired before its sibling ran")
		}
		acquired.Store(true)
		return runtime.StacklessCoroActionComplete
	}
	sibling := func(unsafe.Pointer) uint8 {
		siblingRan.Store(true)
		runtime.Semrelease1(&sema, true, 0)
		return runtime.StacklessCoroActionComplete
	}
	started := false
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		if !started {
			started = true
			runtime.SpawnStacklessCoroForTest(ctx, waiter)
			runtime.SpawnStacklessCoroForTest(ctx, sibling)
		}
		if !acquired.Load() {
			return runtime.StacklessCoroActionYield
		}
		return runtime.StacklessCoroActionComplete
	})
	if state != nil {
		t.Fatal("sibling progress left an acquisition pending")
	}
}
