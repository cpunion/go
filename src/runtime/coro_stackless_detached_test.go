// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"context"
	"internal/testenv"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestStacklessCoroDetachedRootLifetime(t *testing.T) {
	if mode := os.Getenv("GO_TEST_CORO_DETACHED_CASE"); mode != "" {
		testStacklessCoroDetachedRoot(t, mode)
		return
	}
	for _, procs := range []int{1, 4} {
		for _, mode := range []string{"ready", "timer", "cancel", "early", "channel", "blocking", "rootblocking", "socket", "file", "panic", "goexit", "childgoexit", "nested"} {
			t.Run(mode+"/P"+strconv.Itoa(procs), func(t *testing.T) {
				// A broken driver can remain in a foreign call after the root
				// returns logically. Bound the whole subprocess so failures
				// cannot leave a native thread or test goroutine behind.
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := testenv.CommandContext(t, ctx, os.Args[0],
					"-test.run=^TestStacklessCoroDetachedRootLifetime$")
				cmd.Env = append(cmd.Environ(), "GO_TEST_CORO_DETACHED_CASE="+mode,
					"GOMAXPROCS="+strconv.Itoa(procs))
				if data, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("detached root: %v\n%s", err, data)
				}
			})
		}
	}
}

func testStacklessCoroDetachedRoot(t *testing.T, mode string) {
	if mode == "file" {
		// The fallback worker pool is process-wide, not owned by the
		// scheduler. Start it before measuring detached executor cleanup.
		started := false
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if !started {
				started = true
				runtime.CallReadStacklessCoroForTest(ctx, func() {})
				return runtime.StacklessCoroActionWait
			}
			return runtime.StacklessCoroActionComplete
		})
	}
	runtime.GC()
	baselineG := runtime.NumGoroutine()
	baselineOperations := runtime.StacklessCoroOperationCountForTest()
	gate := make(chan int)
	done := make(chan struct{}, 1)
	returned := make(chan struct{})
	var entered atomic.Bool
	var recovered any
	var goexitReturned bool
	var cached bool
	var scheduler unsafe.Pointer
	var timer runtime.StacklessCoroTimerTokenForTest
	var timerReady atomic.Bool
	var n int
	var errno uintptr
	buffer := make([]byte, 1)
	frame := new([16]byte)
	frame[0] = 42
	var pair [2]int
	if mode == "blocking" || mode == "rootblocking" || mode == "socket" || mode == "file" {
		var err error
		pair, err = syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer syscall.Close(pair[0])
		defer syscall.Close(pair[1])
		if mode == "socket" {
			if err := syscall.SetNonblock(pair[0], true); err != nil {
				t.Fatal(err)
			}
		}
	}

	grandchild := func(unsafe.Pointer) uint8 {
		done <- struct{}{}
		return runtime.StacklessCoroActionComplete
	}
	childState, value := 0, 0
	child := func(ctx unsafe.Pointer) uint8 {
		if childState == 0 {
			childState = 1
			entered.Store(true)
			switch mode {
			case "timer":
				runtime.SleepStacklessCoroForTest(ctx, int64(10*time.Millisecond))
				return runtime.StacklessCoroActionWait
			case "cancel", "early":
				timer = runtime.StartSleepStacklessCoroForTest(ctx, int64(time.Hour))
				timerReady.Store(true)
				if mode == "early" {
					runtime.CancelSleepStacklessCoroForTest(timer)
				}
				return runtime.StacklessCoroActionWait
			case "socket":
				runtime.SocketReadStacklessCoroForTest(ctx, pair[0], buffer, &n, &errno)
				return runtime.StacklessCoroActionWait
			case "file":
				runtime.FileReadStacklessCoroForTest(ctx, pair[0], buffer, &n, &errno)
				return runtime.StacklessCoroActionWait
			case "rootblocking":
				if _, err := syscall.Write(pair[1], []byte{1}); err != nil {
					panic(err)
				}
				fallthrough
			case "channel", "panic", "goexit", "childgoexit", "nested":
				if runtime.RecvIntStacklessCoroForTest(ctx, gate, &value, nil) {
					return runtime.StacklessCoroActionWait
				}
			case "blocking":
				var buffer [1]byte
				if runtime.BlockingReadStacklessCoroForTest(ctx, pair[0], buffer[:]) != 1 {
					panic("detached blocking read failed")
				}
			}
		}
		if frame[0] != 42 {
			panic("detached root frame was reused")
		}
		if mode == "nested" {
			runtime.SpawnStacklessCoroForTest(ctx, grandchild)
		} else {
			done <- struct{}{}
		}
		if mode == "childgoexit" {
			runtime.GoexitStacklessCoroForTest(ctx)
			return runtime.StacklessCoroActionGoexit
		}
		return runtime.StacklessCoroActionComplete
	}
	rootState := 0
	root := func(ctx unsafe.Pointer) uint8 {
		if rootState == 0 {
			rootState = 1
			scheduler = runtime.SchedulerStacklessCoroForTest(ctx)
			runtime.SpawnStacklessCoroForTest(ctx, child)
			if mode == "rootblocking" {
				// Enter a foreign call before returning the first resume.
				// Handoff must retain the replacement admitted by this call.
				if runtime.BlockingReadStacklessCoroForTest(ctx, pair[0], buffer) != 1 {
					panic("root blocking read failed")
				}
			}
			// The ready case returns in the same resume that spawned the
			// child; it never reaches another queue-selection point.
			if mode != "ready" {
				return runtime.StacklessCoroActionYield
			}
		}
		if mode != "ready" && !entered.Load() {
			return runtime.StacklessCoroActionYield
		}
		if mode == "cancel" && !timerReady.Load() {
			return runtime.StacklessCoroActionYield
		}
		switch mode {
		case "panic":
			runtime.PanicStacklessCoroForTest(ctx, "detached root panic")
			return runtime.StacklessCoroActionPanic
		case "goexit":
			runtime.GoexitStacklessCoroForTest(ctx)
			return runtime.StacklessCoroActionGoexit
		}
		return runtime.StacklessCoroActionComplete
	}
	go func() {
		defer close(returned)
		defer func() { recovered = recover() }()
		cached = runtime.RunCachedStacklessCoroFrameForTest(unsafe.Pointer(frame), root, unsafe.Sizeof(*frame))
		goexitReturned = mode == "goexit"
	}()
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("public caller waited for a detached child")
	}
	if goexitReturned {
		t.Fatal("root Goexit returned to its caller")
	}
	if mode == "panic" {
		if recovered != "detached root panic" {
			t.Fatalf("root panic = %v", recovered)
		}
	} else if recovered != nil {
		t.Fatalf("unexpected root panic: %v", recovered)
	}
	if cached && mode != "ready" && mode != "early" {
		t.Fatal("root frame with live children was eligible for reuse")
	}
	// A join of all children would deadlock: only the ordinary caller can
	// release these operations, and it must have returned first.
	if mode == "blocking" || mode == "socket" || mode == "file" {
		if _, err := syscall.Write(pair[1], []byte{1}); err != nil {
			t.Fatal(err)
		}
	} else if mode == "cancel" {
		if !runtime.CancelSleepStacklessCoroForTest(timer) {
			t.Fatal("detached timer was not canceled")
		}
	} else {
		close(gate)
	}
	runtime.GC()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("detached child lost its driver after the public root returned")
	}
	if (mode == "socket" || mode == "file") && (n != 1 || errno != 0) {
		t.Fatalf("detached read = (%d, %d), want (1, 0)", n, errno)
	}
	deadline := time.Now().Add(3 * time.Second)
	// The child sends done before returning its completion action. A global
	// goroutine count cannot join this scheduler's cleanup, so also wait for
	// its drainage predicate before checking retained state.
	for runtime.NumGoroutine() > baselineG ||
		!cached && !runtime.DrainedStacklessCoroSchedulerForTest(scheduler) {
		if time.Now().After(deadline) {
			t.Fatalf("detached executors remain: goroutines = %d, baseline = %d", runtime.NumGoroutine(), baselineG)
		}
		runtime.Gosched()
	}
	if !cached && !runtime.DrainedStacklessCoroSchedulerForTest(scheduler) {
		t.Fatal("detached scheduler retained tasks or foreign calls after drainage")
	}
	if got := runtime.StacklessCoroOperationCountForTest(); got != baselineOperations {
		t.Fatalf("operation registry count = %d, baseline = %d", got, baselineOperations)
	}
}
