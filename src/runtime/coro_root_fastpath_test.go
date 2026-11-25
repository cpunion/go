// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"internal/race"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

func TestStacklessCoroManagedRootFastPath(t *testing.T) {
	done := make(chan struct{})
	var errMsg string
	go func() {
		defer close(done)
		// A freshly started goroutine (including recycled G descriptors from
		// gfget) must not inherit a retained stacklessCoroGState from gdestroy.
		if present, _ := runtime.StacklessCoroGStateSummaryForTest(); present {
			errMsg = "fresh goroutine started with non-nil stacklessCoroGState"
			return
		}

		yields := 0
		resume := func(unsafe.Pointer) uint8 {
			if yields < 300 {
				yields++
				return runtime.StacklessCoroActionYield
			}
			return runtime.StacklessCoroActionComplete
		}
		quiescent, wakeLen := runtime.RunManagedStacklessCoroRootForTest(resume)
		if yields != 300 {
			errMsg = "managed directInitialRoot did not complete 300 yields"
			return
		}
		if !race.Enabled {
			if !quiescent {
				errMsg = "managed directInitialRoot did not finish quiescent"
				return
			}
			if wakeLen != 0 {
				errMsg = "managed directInitialRoot signaled wake channel unexpectedly"
				return
			}
		}
		present, clean := runtime.StacklessCoroGStateSummaryForTest()
		if !present || !clean {
			errMsg = "managed root did not retain a clean stacklessCoroGState"
			return
		}
	}()
	<-done
	if errMsg != "" {
		t.Fatal(errMsg)
	}

	// Verify recycled goroutines get their stacklessCoroGState cleared by gdestroy.
	for i := range 32 {
		seen := make(chan bool, 1)
		go func() {
			present, _ := runtime.StacklessCoroGStateSummaryForTest()
			seen <- present
			runtime.RunManagedStacklessCoroRootForTest(func(unsafe.Pointer) uint8 {
				return runtime.StacklessCoroActionComplete
			})
		}()
		if <-seen {
			t.Fatalf("goroutine %d observed stale stacklessCoroGState after gdestroy", i)
		}
	}

	// Verify 0 allocations per repeated managed root call when non-race.
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
		// Warm root scheduler cache and current G's stacklessCoroGState.
		runtime.RunManagedStacklessCoroRootForTest(resume)
		if got := testing.AllocsPerRun(200, func() {
			runtime.RunManagedStacklessCoroRootForTest(resume)
		}); got != 0 {
			t.Fatalf("managed root allocations = %v, want 0", got)
		}
	}
}

func TestStacklessCoroManagedRootFairnessAndTerminal(t *testing.T) {
	// Single-P fairness: a managed directInitialRoot yielding in a loop must
	// still yield to another runnable ordinary goroutine.
	prev := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prev)

	var stop atomic.Bool
	peerRan := make(chan struct{})
	go func() {
		close(peerRan)
		stop.Store(true)
	}()

	deadline := time.Now().Add(5 * time.Second)
	runtime.RunManagedStacklessCoroRootForTest(func(unsafe.Pointer) uint8 {
		if stop.Load() || time.Now().After(deadline) {
			return runtime.StacklessCoroActionComplete
		}
		return runtime.StacklessCoroActionYield
	})
	select {
	case <-peerRan:
	default:
		t.Fatal("managed directInitialRoot starved runnable peer goroutine under GOMAXPROCS=1")
	}

	// Panic propagation through managed root + finishStacklessCoroRootScheduler slow path.
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		runtime.RunManagedStacklessCoroRootForTest(func(ctx unsafe.Pointer) uint8 {
			runtime.PanicStacklessCoroForTest(ctx, "managed-root-panic")
			return runtime.StacklessCoroActionPanic
		})
	}()
	if recovered != "managed-root-panic" {
		t.Fatalf("recovered = %v, want managed-root-panic", recovered)
	}
	if _, clean := runtime.StacklessCoroGStateSummaryForTest(); !clean {
		t.Fatal("stacklessCoroGState not clean after managed root panic")
	}
}
