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

func TestStacklessCoroNativePoolBound(t *testing.T) {
	const roots = 16
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
	for range runtime.StacklessCoroExecutorCount {
		<-started
	}
	if got := runtime.StacklessCoroNativePoolForTest(); got != runtime.StacklessCoroExecutorCount {
		t.Fatalf("native executor count = %d, want %d", got, runtime.StacklessCoroExecutorCount)
	}
	close(gate)
	wg.Wait()
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

func TestStacklessCoroNativePreemption(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	var ready atomic.Bool
	go func() {
		time.Sleep(time.Millisecond)
		ready.Store(true)
	}()

	_ = syscall.Kill(syscall.Getpid(), syscall.SIGURG)
	runtime.RunStacklessCoroForTest(func(unsafe.Pointer) uint8 {
		for i := 0; i < 1<<28 && !ready.Load(); i++ {
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
			runtime.SleepStacklessCoroForTest(ctx, int64(time.Millisecond))
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
