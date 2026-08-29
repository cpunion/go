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

func TestStacklessCoroSleepHostProgress(t *testing.T) {
	const (
		delay     = time.Nanosecond
		maxSleeps = 1000
	)
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
