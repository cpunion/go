// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && !coropullcompare && ((darwin && arm64) || (linux && amd64))

package runtime_test

import (
	"runtime"
	"testing"
	"time"
	"unsafe"
)

func TestStacklessCoroSleepDeferralRequiresExecutor(t *testing.T) {
	if runtime.StacklessCoroDeferSleepOutsideExecutorForTest() {
		t.Fatal("deferred sleep accepted outside a native executor")
	}
}

func TestStacklessCoroSleepStorage(t *testing.T) {
	// The deferred deadline and signal-visible source owner must fit within
	// the driver's bounded 224B allocation class.
	if got := runtime.StacklessCoroNativeContextAllocationSizeForTest(); got > 224 {
		t.Fatalf("native context allocation = %d bytes, want at most 224", got)
	}
}

func TestStacklessCoroSleepHostProgress(t *testing.T) {
	for _, delay := range []time.Duration{time.Nanosecond, time.Microsecond} {
		t.Run(delay.String(), func(t *testing.T) {
			testStacklessCoroSleepHostProgress(t, delay)
		})
	}
}

func testStacklessCoroSleepHostProgress(t *testing.T, delay time.Duration) {
	const maxSleeps = 1000
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	start := make(chan struct{})
	progressed := make(chan struct{})
	go func() {
		<-start
		close(progressed)
	}()
	state := 0
	sleeps := 0
	observed := false
	sleepFailed := false
	invalidState := -1
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			close(start)
			sleeps++
			if !runtime.SleepStacklessCoroForTest(ctx, int64(delay)) {
				sleepFailed = true
				return runtime.StacklessCoroActionComplete
			}
			return runtime.StacklessCoroActionWait
		case 1:
			select {
			case <-progressed:
				observed = true
			default:
			}
			if !observed && sleeps < maxSleeps {
				sleeps++
				if !runtime.SleepStacklessCoroForTest(ctx, int64(delay)) {
					sleepFailed = true
					return runtime.StacklessCoroActionComplete
				}
				return runtime.StacklessCoroActionWait
			}
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			invalidState = state
			return runtime.StacklessCoroActionComplete
		}
	})
	<-progressed
	if sleepFailed {
		t.Fatal("positive sleep did not wait")
	}
	if invalidState >= 0 {
		t.Fatalf("unexpected state %d", invalidState)
	}
	if !observed {
		t.Fatalf("host goroutine did not run during %d coroutine sleeps", sleeps)
	}
}

func TestStacklessCoroSleepDueHostTimer(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	fired := make(chan struct{})
	var timer *time.Timer
	var sleeps int
	var observed, started bool
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		if timer == nil {
			timer = time.AfterFunc(time.Nanosecond, func() { close(fired) })
		} else {
			select {
			case <-fired:
				observed = true
				return runtime.StacklessCoroActionComplete
			default:
			}
		}
		if sleeps == 32 {
			return runtime.StacklessCoroActionComplete
		}
		sleeps++
		started = runtime.SleepStacklessCoroForTest(ctx, int64(time.Nanosecond))
		if !started {
			return runtime.StacklessCoroActionComplete
		}
		// Make the coroutine's deadline expire before returning Wait.
		// The ordinary timer still has to run when there is no runnable G.
		until := time.Now().Add(time.Microsecond)
		for time.Now().Before(until) {
		}
		return runtime.StacklessCoroActionWait
	})
	if timer != nil {
		timer.Stop()
	}
	if !started || !observed {
		t.Fatalf("after %d short sleeps, started = %v, host timer fired = %v", sleeps, started, observed)
	}
}

func TestStacklessCoroDeferredChannelStorage(t *testing.T) {
	if !runtime.CheckStacklessCoroDeferredChannelStorageForTest() {
		t.Fatal("deferred channel and timer storage overlapped")
	}
}
