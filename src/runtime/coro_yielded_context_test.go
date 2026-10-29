// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime_test

import (
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
)

func TestStacklessCoroNativeYieldedContextReuse(t *testing.T) {
	const child = "GO_CORO_YIELDED_CONTEXT_REUSE"
	if os.Getenv(child) == "" {
		for _, action := range []uint8{runtime.StacklessCoroActionYield, runtime.StacklessCoroActionHandoff} {
			t.Run(strconv.Itoa(int(action)), func(t *testing.T) {
				cmd := exec.Command(os.Args[0], "-test.run=^TestStacklessCoroNativeYieldedContextReuse$", "-test.count=1")
				cmd.Env = append(os.Environ(), child+"="+strconv.Itoa(int(action)), "GOMAXPROCS=1", "GOGC=off")
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("yielded context child: %v\n%s", err, output)
				}
			})
		}
		return
	}
	value, err := strconv.Atoi(os.Getenv(child))
	if err != nil || (uint8(value) != runtime.StacklessCoroActionYield && uint8(value) != runtime.StacklessCoroActionHandoff) {
		t.Fatal("invalid child action")
	}
	procs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(procs)
	gc := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gc)
	runtime.RunStacklessCoroForTest(func(unsafe.Pointer) uint8 { return runtime.StacklessCoroActionComplete })
	runtime.GC()
	runtime.GC()
	before := runtime.StacklessCoroNativePoolForTest()
	const roots = 64
	ready := make(chan struct{}, roots)
	release := make(chan int)
	var wg sync.WaitGroup
	var completed atomic.Int32
	for range roots {
		wg.Go(func() {
			step, total, reservedTotal, received := 0, 0, 0, 0
			var ok bool
			var held unsafe.Pointer
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				switch step {
				case 0:
					step++
					frame := &stacklessCoroYieldAwaitFrame{action: uint8(value), total: &total}
					runtime.AwaitStacklessCoroFrameForTest(ctx, unsafe.Pointer(frame), stacklessCoroYieldAwaitResume)
					return runtime.StacklessCoroActionWait
				case 1:
					step++
					held = newStacklessCoroFrameCacheTestFrame(ctx, stacklessCoroFrameCacheResume, 137, &reservedTotal)
					return uint8(value)
				case 2:
					step++
					ready <- struct{}{}
					runtime.RecvIntStacklessCoroForTest(ctx, release, &received, &ok)
					return runtime.StacklessCoroActionWait
				case 3:
					step++
					runtime.AwaitStacklessCoroFrameForTest(ctx, held, stacklessCoroFrameCacheResume)
					held = nil
					return runtime.StacklessCoroActionWait
				}
				return runtime.StacklessCoroActionComplete
			})
			if total == 2 && reservedTotal == 137 && received == 0 && !ok {
				completed.Add(1)
			}
		})
	}
	for range roots {
		<-ready
	}
	after := runtime.StacklessCoroNativePoolForTest()
	runtime.GC()
	runtime.GC()
	close(release)
	wg.Wait()
	if got := completed.Load(); got != roots {
		t.Fatalf("resumed yielded/reserved roots = %d, want %d", got, roots)
	}
	if delta := after - before; delta > 8 {
		t.Fatalf("64 concurrently yielding roots allocated %d native contexts, want a bounded parked set", delta)
	}
}
