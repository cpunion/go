// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime_test

import (
	"bytes"
	"os"
	"os/exec"
	"runtime"
	"runtime/trace"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestStacklessCoroNativeStack(t *testing.T) {
	value := new(int)
	*value = 42
	var state int
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		native, sp, lo, hi, g0lo, g0hi := runtime.StacklessCoroNativeStackForTest()
		if !native {
			t.Fatal("resume did not run on a native executor")
		}
		if sp < lo || sp >= hi {
			t.Fatalf("executor SP %#x is outside [%#x, %#x)", sp, lo, hi)
		}
		if sp >= g0lo && sp < g0hi {
			t.Fatalf("executor SP %#x is on g0 stack [%#x, %#x)", sp, g0lo, g0hi)
		}
		if n := runtime.Stack(make([]byte, 32<<10), true); n == 0 {
			t.Fatal("runtime.Stack returned an empty traceback")
		}

		switch state {
		case 0:
			state = 1
			runtime.CallReadStacklessCoroForTest(ctx, runtime.GC)
			return runtime.StacklessCoroActionWait
		case 1:
			if *value != 42 {
				t.Fatalf("value after GC = %d, want 42", *value)
			}
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
}

func TestStacklessCoroNativePoolGrowthAndReuse(t *testing.T) {
	const roots = 16
	run := func() int {
		var wg sync.WaitGroup
		started := make(chan struct{}, roots)
		gate := make(chan struct{})
		wg.Add(roots)
		for range roots {
			go func() {
				defer wg.Done()
				runtime.RunStacklessCoroForTest(func(unsafe.Pointer) uint8 {
					started <- struct{}{}
					<-gate
					return runtime.StacklessCoroActionComplete
				})
			}()
		}
		watchdog := time.NewTimer(5 * time.Second)
		for range roots {
			select {
			case <-started:
			case <-watchdog.C:
				close(gate)
				wg.Wait()
				t.Fatal("native executor pool did not grow for concurrent roots")
			}
		}
		watchdog.Stop()
		count := runtime.StacklessCoroNativePoolForTest()
		close(gate)
		wg.Wait()
		return count
	}

	grown := run()
	if grown < roots {
		t.Fatalf("native executor count = %d, want at least %d", grown, roots)
	}
	if reused := run(); reused != grown {
		t.Fatalf("native executor count after reuse = %d, want %d", reused, grown)
	}
}

func TestStacklessCoroNativeReplacementDrain(t *testing.T) {
	runs, native := runtime.StacklessCoroNativeReplacementDrainForTest()
	if runs != 2 || !native {
		t.Fatalf("replacement drain = (%d runs, native %t), want (2, true)",
			runs, native)
	}
}

func TestStacklessCoroNativeTaskReuse(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	run := func() (tasks int) {
		var state int
		child := func(unsafe.Pointer) uint8 {
			return runtime.StacklessCoroActionComplete
		}
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				tasks = runtime.StacklessCoroFreeTaskCountForTest(ctx)
				state = 1
				runtime.AwaitStacklessCoroForTest(ctx, child)
				return runtime.StacklessCoroActionWait
			case 1:
				state = 2
				return runtime.StacklessCoroActionComplete
			default:
				t.Fatalf("unexpected cache reuse state %d", state)
				return runtime.StacklessCoroActionInvalid
			}
		})
		return tasks
	}

	// A released context returns to its claimed atomic slot. Prime that slot
	// before checking that a later root receives the task cache.
	run()
	if tasks := run(); tasks == 0 {
		t.Fatal("native root task cache is empty after reuse")
	}
}

func TestStacklessCoroNativeDropsOverflowTaskSlots(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	const depth = runtime.StacklessCoroTaskCacheSize +
		2*runtime.StacklessCoroTaskChunkSize - 2
	var state int
	var bypassed atomic.Int32
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			frame := runtime.TakeStacklessCoroFrameForTest(ctx,
				stacklessCoroDeepFrameCacheResume,
				unsafe.Sizeof(stacklessCoroDeepFrameCacheTestFrame{}))
			if frame == nil {
				frame = unsafe.Pointer(new(stacklessCoroDeepFrameCacheTestFrame))
			}
			*(*stacklessCoroDeepFrameCacheTestFrame)(frame) =
				stacklessCoroDeepFrameCacheTestFrame{
					depth: depth, bypassed: &bypassed,
				}
			runtime.AwaitStacklessCoroFrameForTest(ctx, frame,
				stacklessCoroDeepFrameCacheResume)
			return runtime.StacklessCoroActionWait
		case 1:
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected overflow-task native state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})

	maxTasks := 0
	for range 2 * runtime.StacklessCoroWarmExecutorCount {
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			tasks := runtime.StacklessCoroFreeTaskCountForTest(ctx)
			if tasks > runtime.StacklessCoroTaskCacheSize {
				t.Fatalf("native context retained %d tasks, limit %d", tasks,
					runtime.StacklessCoroTaskCacheSize)
			}
			if tasks > maxTasks {
				maxTasks = tasks
			}
			free, direct := runtime.StacklessCoroOverflowTaskCountsForTest(ctx)
			if free != 0 || direct != 0 {
				t.Fatalf("reused native overflow-task counts = (%d, %d), want zero",
					free, direct)
			}
			return runtime.StacklessCoroActionComplete
		})
	}
	if maxTasks != runtime.StacklessCoroTaskCacheSize {
		t.Fatalf("largest reused native task cache = %d, want %d", maxTasks,
			runtime.StacklessCoroTaskCacheSize)
	}
}

func TestStacklessCoroNativeTaskCacheBound(t *testing.T) {
	const roots = 2 * runtime.StacklessCoroWarmExecutorCount
	started := make(chan struct{}, roots)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(roots)
	for range roots {
		go func() {
			defer wg.Done()
			var state int
			child := func(unsafe.Pointer) uint8 {
				return runtime.StacklessCoroActionComplete
			}
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				switch state {
				case 0:
					state = 1
					runtime.AwaitStacklessCoroForTest(ctx, child)
					return runtime.StacklessCoroActionWait
				case 1:
					state = 2
					if tasks := runtime.StacklessCoroFreeTaskCountForTest(ctx); tasks == 0 {
						t.Error("native root task cache is empty")
					}
					started <- struct{}{}
					<-gate
					return runtime.StacklessCoroActionComplete
				default:
					t.Errorf("unexpected task cache state %d", state)
					return runtime.StacklessCoroActionInvalid
				}
			})
		}()
	}
	watchdog := time.NewTimer(5 * time.Second)
	for range roots {
		select {
		case <-started:
		case <-watchdog.C:
			close(gate)
			wg.Wait()
			t.Fatal("native roots did not populate their task caches")
		}
	}
	watchdog.Stop()
	close(gate)
	wg.Wait()

	warm, overflow := runtime.StacklessCoroNativePoolTaskCacheForTest()
	if warm == 0 {
		t.Error("warm native pool retained no task cache")
	}
	if overflow != 0 {
		t.Errorf("overflow native pool retained %d tasks", overflow)
	}
}

func TestStacklessCoroNativeReuseDuringGC(t *testing.T) {
	const runs = 10_000
	gcDone := make(chan struct{})
	go func() {
		for range 100 {
			runtime.GC()
		}
		close(gcDone)
	}()

	for range runs {
		runtime.RunStacklessCoroForTest(func(unsafe.Pointer) uint8 {
			return runtime.StacklessCoroActionComplete
		})
	}
	<-gcDone
}

func TestStacklessCoroNativeBlockingReuseDuringGC(t *testing.T) {
	const runs = 50_000
	gcDone := make(chan struct{})
	go func() {
		for range 100 {
			runtime.GC()
		}
		close(gcDone)
	}()

	for range runs {
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			runtime.BlockingBoundaryStacklessCoroForTest(ctx)
			return runtime.StacklessCoroActionComplete
		})
	}
	<-gcDone
}

func TestStacklessCoroNativeTransitionSignals(t *testing.T) {
	const (
		signals = 256
		minRuns = 10_000
	)

	oldProcs := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(oldProcs)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	target := runtime.StacklessCoroNativeSignalTargetForTest()
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		for range signals {
			runtime.SignalStacklessCoroNativeForTest(target)
			// Avoid saturating one M with pending signals. In particular,
			// Darwin can livelock when signals arrive faster than the target
			// thread can make progress.
			runtime.Gosched()
		}
		close(done)
	}()
	<-started

	runs := 0
	signalsDone := false
	for !signalsDone || runs < minRuns {
		runtime.RunStacklessCoroForTest(func(unsafe.Pointer) uint8 {
			return runtime.StacklessCoroActionComplete
		})
		runs++
		select {
		case <-done:
			signalsDone = true
		default:
		}
	}
}

func TestStacklessCoroNativeBlockingReturnProgress(t *testing.T) {
	testStacklessCoroNativeBlockingProgress(t, 3)
}

func TestStacklessCoroNativeBlockingCapacity(t *testing.T) {
	testStacklessCoroNativeBlockingProgress(t, 8)
}

func testStacklessCoroNativeBlockingProgress(t *testing.T, workers int) {
	const maxYields = 1000
	sockets := make([][2]int, workers)
	for i := range sockets {
		pair, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
		if err != nil {
			t.Fatal(err)
		}
		sockets[i] = pair
		defer syscall.Close(pair[0])
		defer syscall.Close(pair[1])
	}

	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	var entered, done atomic.Int32
	var readFailed, writeFailed atomic.Bool
	var rescued atomic.Bool
	rescue := time.AfterFunc(5*time.Second, func() {
		rescued.Store(true)
		for _, pair := range sockets {
			syscall.Write(pair[1], []byte{'r'})
		}
	})
	defer rescue.Stop()

	workerFns := make([]func(unsafe.Pointer) uint8, workers)
	for i := range workerFns {
		fd := sockets[i][0]
		workerFns[i] = func(ctx unsafe.Pointer) uint8 {
			entered.Add(1)
			var buffer [1]byte
			if n := runtime.BlockingReadStacklessCoroForTest(ctx, fd, buffer[:]); n != 1 {
				readFailed.Store(true)
			}
			done.Add(1)
			return runtime.StacklessCoroActionComplete
		}
	}

	var state, yields int
	var stalled bool
	var executorStateFailed bool
	var returnStateFailed bool
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			for _, worker := range workerFns {
				runtime.SpawnStacklessCoroForTest(ctx, worker)
			}
			state = 1
			return runtime.StacklessCoroActionYield
		case 1:
			if rescued.Load() {
				return runtime.StacklessCoroActionComplete
			}
			if entered.Load() != int32(workers) {
				return runtime.StacklessCoroActionYield
			}
			executors, blocking := runtime.ExecutorStateStacklessCoroForTest(ctx)
			if blocking != uint32(workers) {
				return runtime.StacklessCoroActionYield
			}
			executorStateFailed = executors < blocking+1
			for _, pair := range sockets {
				if n := runtime.WriteStacklessCoroNativeForTest(pair[1], 'p'); n != 1 {
					writeFailed.Store(true)
				}
			}
			state = 2
			return runtime.StacklessCoroActionYield
		case 2:
			if rescued.Load() {
				return runtime.StacklessCoroActionComplete
			}
			if done.Load() == int32(workers) {
				returners, pending := runtime.ForeignReturnStateStacklessCoroForTest(ctx)
				_, blocking := runtime.ExecutorStateStacklessCoroForTest(ctx)
				returnStateFailed = returners != 0 || pending || blocking != 0
				return runtime.StacklessCoroActionComplete
			}
			returners, _ := runtime.ForeignReturnStateStacklessCoroForTest(ctx)
			if returners == 0 {
				yields = 0
				return runtime.StacklessCoroActionYield
			}
			yields++
			if yields >= maxYields {
				stalled = true
				return runtime.StacklessCoroActionComplete
			}
			return runtime.StacklessCoroActionYield
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
	if stalled {
		t.Fatal("blocking foreign-call returns did not acquire a P")
	}
	if executorStateFailed {
		t.Fatal("blocking foreign calls did not reserve replacement capacity")
	}
	if readFailed.Load() {
		t.Fatal("blocking foreign-call read failed")
	}
	if writeFailed.Load() {
		t.Fatal("blocking foreign-call release failed")
	}
	if rescued.Load() {
		t.Fatal("blocking foreign-call return required rescue")
	}
	if returnStateFailed {
		t.Fatal("blocking foreign-call return left scheduler state pending")
	}
}

func TestStacklessCoroNativePreemption(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	var ready atomic.Bool
	go func() {
		time.Sleep(time.Millisecond)
		ready.Store(true)
	}()

	runtime.RunStacklessCoroForTest(func(unsafe.Pointer) uint8 {
		// Keep this loop free of cooperative safe points. The large bound
		// gives sysmon enough time to preempt on a loaded builder.
		for i := 0; i < 1<<31 && !ready.Load(); i++ {
		}
		return runtime.StacklessCoroActionComplete
	})
	if !ready.Load() {
		t.Fatal("native executor did not yield to an asynchronously preempting goroutine")
	}
}

func TestStacklessCoroNativeTrace(t *testing.T) {
	var out bytes.Buffer
	if err := trace.Start(&out); err != nil {
		t.Fatal(err)
	}
	var state int
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			if !runtime.SleepStacklessCoroForTest(ctx,
				int64(time.Millisecond)) {
				t.Fatal("positive sleep did not start a timer")
			}
			return runtime.StacklessCoroActionWait
		case 1:
			return runtime.StacklessCoroActionComplete
		default:
			panic("unexpected stackless coroutine state")
		}
	})
	trace.Stop()
	if out.Len() == 0 {
		t.Fatal("execution trace is empty")
	}
}

func TestStacklessCoroNilResume(t *testing.T) {
	const helper = "GO_RUNTIME_CORO_NIL_RESUME"
	if os.Getenv(helper) == "1" {
		runtime.RunStacklessCoroForTest(nil)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestStacklessCoroNilResume$")
	cmd.Env = append(os.Environ(), helper+"=1", "GOTRACEBACK=single")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("nil resume function unexpectedly succeeded")
	}
	if !strings.Contains(string(out), "nil stackless coroutine resume function") {
		t.Fatalf("nil resume function failed with the wrong error:\n%s", out)
	}
}

func TestStacklessCoroNativeFixedStackOverflow(t *testing.T) {
	const helper = "GO_RUNTIME_CORO_FIXED_STACK_OVERFLOW"
	if os.Getenv(helper) == "1" {
		runtime.RunStacklessCoroForTest(func(unsafe.Pointer) uint8 {
			growStacklessCoroNativeStack(0)
			return runtime.StacklessCoroActionComplete
		})
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestStacklessCoroNativeFixedStackOverflow$")
	cmd.Env = append(os.Environ(), helper+"=1", "GOTRACEBACK=single")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("fixed executor stack unexpectedly grew")
	}
	if !strings.Contains(string(out), "stackless coroutine executor stack overflow") {
		t.Fatalf("fixed executor stack failed with the wrong error:\n%s", out)
	}
}

//go:noinline
func growStacklessCoroNativeStack(depth int) {
	var keep [32 << 10]byte
	keep[depth&(len(keep)-1)] = byte(depth)
	growStacklessCoroNativeStack(depth + 1)
	runtime.KeepAlive(&keep)
}
