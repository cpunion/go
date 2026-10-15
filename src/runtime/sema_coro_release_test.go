// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"context"
	"internal/race"
	"internal/testenv"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

func TestStacklessCoroSemaphoreRelease(t *testing.T) {
	if mode := os.Getenv("GO_TEST_CORO_SEMAPHORE_RELEASE"); mode != "" {
		switch mode {
		case "fast":
			testStacklessCoroSemaphoreReleaseFast(t)
		case "collision":
			testStacklessCoroSemaphoreReleaseCollision(t)
		case "ordinary", "ordinary-queued":
			testStacklessCoroSemaphoreReleaseOrdinary(t, mode == "ordinary-queued")
		case "mixed", "mixed-handoff":
			testStacklessCoroSemaphoreReleaseMixed(t, mode == "mixed-handoff")
		case "publication":
			testStacklessCoroSemaphoreReleasePublication(t)
		case "sibling":
			testStacklessCoroSemaphoreReleaseSibling(t)
		default:
			t.Fatalf("unknown semaphore release case %q", mode)
		}
		return
	}
	for _, procs := range []int{1, 4} {
		for _, mode := range []string{"fast", "collision", "ordinary", "ordinary-queued", "mixed", "mixed-handoff", "publication", "sibling"} {
			t.Run(mode+"/P"+strconv.Itoa(procs), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				cmd := testenv.CommandContext(t, ctx, os.Args[0],
					"-test.run=^TestStacklessCoroSemaphoreRelease$")
				cmd.Env = append(cmd.Environ(), "GO_TEST_CORO_SEMAPHORE_RELEASE="+mode,
					"GOMAXPROCS="+strconv.Itoa(procs))
				if data, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("semaphore release case: %v\n%s", err, data)
				}
			})
		}
	}
}

func testStacklessCoroSemaphoreReleaseFast(t *testing.T) {
	var sema uint32
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		for _, handoff := range []bool{false, true} {
			if runtime.SemreleaseStacklessCoroForTest(ctx, &sema, handoff) {
				panic("semaphore release without waiters reported a handoff")
			}
		}
		return runtime.StacklessCoroActionComplete
	})
	if sema != 2 {
		t.Fatalf("released tokens = %d, want 2", sema)
	}
}

func testStacklessCoroSemaphoreReleaseCollision(t *testing.T) {
	// Semaphore buckets use the address shifted by three bits, modulo the
	// table size. These addresses share a bucket but not their waiters.
	var semas [2*runtime.SemTableSize + 1]uint32
	blocked, other := &semas[0], &semas[2*runtime.SemTableSize]
	if runtime.SemRootLock(blocked) != runtime.SemRootLock(other) {
		t.Fatal("semaphore addresses do not share a bucket")
	}
	done := make(chan struct{})
	go func() {
		runtime.SemacquireMutexOrdinaryForTest(blocked, false, 0)
		close(done)
	}()
	waitStacklessCoroSemaphoreQueue(blocked, 1, 0)
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		if runtime.SemreleaseStacklessCoroForTest(ctx, other, true) {
			panic("semaphore handoff selected another address's waiter")
		}
		return runtime.StacklessCoroActionComplete
	})
	waitStacklessCoroSemaphoreQueue(blocked, 1, 0)
	runtime.Semrelease1(blocked, false, 0)
	<-done
	if *blocked != 0 || *other != 1 {
		t.Fatalf("semaphore tokens = (%d, %d), want (0, 1)", *blocked, *other)
	}
}

func testStacklessCoroSemaphoreReleaseOrdinary(t *testing.T, queued bool) {
	var sema uint32
	var ran, finished atomic.Bool
	done := make(chan struct{})
	go func() {
		runtime.SemacquireMutexOrdinaryForTest(&sema, false, 0)
		ran.Store(true)
		close(done)
	}()
	waitStacklessCoroSemaphoreQueue(&sema, 1, 0)
	step := 0
	sawOrdinary := false
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch step {
		case 0:
			step++
			// Consume the first host fairness check while the ordinary
			// waiter is still blocked. Handoff must not wait 256 yields.
			return runtime.StacklessCoroActionYield
		case 1:
			if queued {
				runtime.SpawnStacklessCoroForTest(ctx, func(unsafe.Pointer) uint8 {
					if finished.Load() {
						return runtime.StacklessCoroActionComplete
					}
					return runtime.StacklessCoroActionYield
				})
			}
			if !runtime.SemreleaseStacklessCoroForTest(ctx, &sema, true) {
				panic("ordinary semaphore waiter did not receive a ticket")
			}
			if runtime.GOMAXPROCS(0) == 1 && ran.Load() {
				panic("semaphore release yielded inside the native resume")
			}
			step++
			return runtime.StacklessCoroActionHandoff
		case 2:
			sawOrdinary = ran.Load()
			finished.Store(true)
			return runtime.StacklessCoroActionComplete
		}
		panic("unexpected semaphore release continuation")
	})
	<-done
	if runtime.GOMAXPROCS(0) == 1 && !sawOrdinary {
		t.Fatal("ordinary waiter did not run at the handoff boundary")
	}
	if sema != 0 {
		t.Fatalf("handoff retained %d tokens", sema)
	}
}

func testStacklessCoroSemaphoreReleaseMixed(t *testing.T, handoff bool) {
	const workers, iterations = 4, 200
	sema := uint32(1)
	var inside atomic.Int32
	var count int
	enter := func() {
		race.Acquire(unsafe.Pointer(&sema))
		if inside.Add(1) != 1 {
			panic("mixed semaphore acquisitions overlap")
		}
		count++
	}
	leave := func() {
		if inside.Add(-1) != 0 {
			panic("mixed semaphore critical sections overlap")
		}
		race.Release(unsafe.Pointer(&sema))
	}
	var wg sync.WaitGroup
	for id := 0; id < workers; id++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id%2 == 0 {
				for range iterations {
					runtime.SemacquireMutexOrdinaryForTest(&sema, false, 0)
					enter()
					runtime.Gosched()
					leave()
					runtime.Semrelease1(&sema, handoff, 0)
				}
				return
			}
			var state unsafe.Pointer
			completed := 0
			acquired := false
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				if completed == iterations {
					return runtime.StacklessCoroActionComplete
				}
				if !acquired {
					if runtime.SemacquireMutexStacklessCoroForTest(ctx, &sema, false, &state) {
						return runtime.StacklessCoroActionWait
					}
					enter()
					acquired = true
					return runtime.StacklessCoroActionYield
				}
				leave()
				acquired = false
				completed++
				if runtime.SemreleaseStacklessCoroForTest(ctx, &sema, handoff) {
					return runtime.StacklessCoroActionHandoff
				}
				return runtime.StacklessCoroActionYield
			})
		}()
	}
	wg.Wait()
	if count != workers*iterations || sema != 1 {
		t.Fatalf("mixed release result = (%d, %d), want (%d, 1)", count, sema, workers*iterations)
	}
	waitStacklessCoroSemaphoreQueue(&sema, 0, 0)
}

func testStacklessCoroSemaphoreReleasePublication(t *testing.T) {
	const iterations = 1000
	var sema uint32
	var acquired atomic.Int32
	done := make(chan struct{})
	go func() {
		var state unsafe.Pointer
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if acquired.Load() == iterations {
				return runtime.StacklessCoroActionComplete
			}
			if runtime.SemacquireMutexStacklessCoroForTest(ctx, &sema, false, &state) {
				return runtime.StacklessCoroActionWait
			}
			acquired.Add(1)
			return runtime.StacklessCoroActionYield
		})
		close(done)
	}()
	released := 0
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		if released == iterations {
			return runtime.StacklessCoroActionComplete
		}
		_, logical := runtime.SemaphoreWaitersStacklessCoroForTest(&sema)
		if logical != 1 {
			return runtime.StacklessCoroActionYield
		}
		// The waiter runs on another scheduler and can finish and clear
		// its ticket before this release returns. Release must use the
		// snapshot taken before publishing readiness, not reread it.
		if !runtime.SemreleaseStacklessCoroForTest(ctx, &sema, true) {
			panic("published logical semaphore ticket was lost")
		}
		released++
		return runtime.StacklessCoroActionHandoff
	})
	<-done
	if acquired.Load() != iterations || sema != 0 {
		t.Fatalf("published handoffs = (%d, %d), want (%d, 0)", acquired.Load(), sema, iterations)
	}
	waitStacklessCoroSemaphoreQueue(&sema, 0, 0)
}

func testStacklessCoroSemaphoreReleaseSibling(t *testing.T) {
	var sema uint32
	var state unsafe.Pointer
	var acquired, finished atomic.Bool
	waiter := func(ctx unsafe.Pointer) uint8 {
		if runtime.SemacquireMutexStacklessCoroForTest(ctx, &sema, false, &state) {
			return runtime.StacklessCoroActionWait
		}
		acquired.Store(true)
		return runtime.StacklessCoroActionComplete
	}
	released := false
	releaser := func(ctx unsafe.Pointer) uint8 {
		if !released {
			_, logical := runtime.SemaphoreWaitersStacklessCoroForTest(&sema)
			if logical != 1 {
				return runtime.StacklessCoroActionYield
			}
			if !runtime.SemreleaseStacklessCoroForTest(ctx, &sema, true) {
				panic("logical sibling did not receive a semaphore ticket")
			}
			released = true
			return runtime.StacklessCoroActionHandoff
		}
		if !acquired.Load() {
			// Another executor may have selected the waiter without yet
			// executing its continuation. Require progress, not a global
			// execution order across host Ps.
			return runtime.StacklessCoroActionYield
		}
		finished.Store(true)
		return runtime.StacklessCoroActionComplete
	}
	started := false
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		if !started {
			started = true
			runtime.SpawnStacklessCoroForTest(ctx, waiter)
			runtime.SpawnStacklessCoroForTest(ctx, releaser)
		}
		if !finished.Load() {
			return runtime.StacklessCoroActionYield
		}
		return runtime.StacklessCoroActionComplete
	})
	if state != nil || sema != 0 {
		t.Fatal("sibling handoff retained a token or acquisition state")
	}
}
