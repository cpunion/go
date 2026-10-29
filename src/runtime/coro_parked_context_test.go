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
	"sync/atomic"
	"testing"
	"unsafe"
)

type parkedContextRoot struct {
	scheduler unsafe.Pointer
	context   uintptr
	release   chan int
	done      chan int
	reserved  bool
}

func startParkedContextRoot(t *testing.T, reserved bool) *parkedContextRoot {
	t.Helper()
	r := &parkedContextRoot{release: make(chan int), done: make(chan int, 1), reserved: reserved}
	ready := make(chan struct{}, 1)
	go func() {
		step, total := 0, 0
		var held unsafe.Pointer
		var bypassed atomic.Int32
		var value int
		var ok bool
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch step {
			case 0:
				r.context = runtime.StacklessCoroNativeContextIdentityForTest()
				step = 1
				if reserved {
					frame := &stacklessCoroDeepFrameCacheTestFrame{
						depth: 2 * runtime.StacklessCoroTaskCacheSize, bypassed: &bypassed,
					}
					runtime.AwaitStacklessCoroFrameForTest(ctx, unsafe.Pointer(frame), stacklessCoroDeepFrameCacheResume)
					return runtime.StacklessCoroActionWait
				}
				fallthrough
			case 1:
				step = 2
				if reserved {
					held = newStacklessCoroFrameCacheTestFrame(ctx, stacklessCoroFrameCacheResume, 137, &total)
				}
				r.scheduler = runtime.SchedulerStacklessCoroForTest(ctx)
				ready <- struct{}{}
				runtime.RecvIntStacklessCoroForTest(ctx, r.release, &value, &ok)
				return runtime.StacklessCoroActionWait
			case 2:
				if reserved {
					step = 3
					runtime.AwaitStacklessCoroFrameForTest(ctx, held, stacklessCoroFrameCacheResume)
					held = nil
					return runtime.StacklessCoroActionWait
				}
				return runtime.StacklessCoroActionComplete
			case 3:
				return runtime.StacklessCoroActionComplete
			}
			return runtime.StacklessCoroActionInvalid
		})
		r.done <- total
	}()
	<-ready
	if r.context == 0 {
		t.Fatal("parked root did not enter a native context")
	}
	return r
}

func (r *parkedContextRoot) finish(t *testing.T) {
	t.Helper()
	if r.release == nil {
		return
	}
	close(r.release)
	r.release = nil
	want := 0
	if r.reserved {
		want = 137
	}
	if got := <-r.done; got != want {
		t.Errorf("resumed reserved result = %d, want %d", got, want)
	}
}

func TestStacklessCoroNativeParkedContextReuse(t *testing.T) {
	const child = "GO_CORO_PARKED_CONTEXT_REUSE"
	mode := os.Getenv(child)
	if mode == "" {
		for _, mode := range []string{"cacheless", "reserved", "lease"} {
			t.Run(mode, func(t *testing.T) {
				cmd := exec.Command(os.Args[0], "-test.run=^TestStacklessCoroNativeParkedContextReuse$", "-test.count=1")
				cmd.Env = append(os.Environ(), child+"="+mode, "GOMAXPROCS=1", "GOGC=off")
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("parked context child: %v\n%s", err, output)
				}
			})
		}
		return
	}
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	gc := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gc)
	runtime.RunStacklessCoroForTest(func(unsafe.Pointer) uint8 {
		return runtime.StacklessCoroActionComplete
	})
	runtime.GC()
	runtime.GC()
	var roots []*parkedContextRoot
	defer func() {
		for _, r := range roots {
			r.finish(t)
		}
	}()
	start := func(reserved bool) *parkedContextRoot {
		r := startParkedContextRoot(t, reserved)
		roots = append(roots, r)
		return r
	}
	if mode == "lease" {
		// Fill the four parked leases, then let an overflow root release its
		// context. A completed registered root creates a free lease slot.
		// The next root must borrow the overflow context into that slot.
		var registered [4]*parkedContextRoot
		for i := range registered {
			registered[i] = start(true)
		}
		old := start(true)
		registered[0].finish(t)
		borrower := start(true)
		if borrower.context != old.context {
			t.Fatalf("overflow context was not reused: old=%#x new=%#x", old.context, borrower.context)
		}
		if before := runtime.StacklessCoroParkedFramesForTest(borrower.scheduler); before.FreeFrames == 0 || before.Reserved != 1 {
			t.Fatalf("borrower has no parked cache/reservation: %+v", before)
		}
		// Resuming the old waiter must not run a deferred write through its
		// old context pointer and erase the borrower's parked-cache lease.
		old.finish(t)
		runtime.GC()
		runtime.GC()
		after := runtime.StacklessCoroParkedFramesForTest(borrower.scheduler)
		if after.FreeFrames != 0 || after.FreeBytes != 0 || after.Reserved != 1 || after.CachedFrames != 1 {
			t.Fatalf("old waiter corrupted borrowed context lease: %+v", after)
		}
		return
	}
	if mode != "cacheless" && mode != "reserved" {
		t.Fatal("invalid child mode")
	}
	before := runtime.StacklessCoroNativePoolForTest()
	for range 64 {
		start(mode == "reserved")
	}
	after := runtime.StacklessCoroNativePoolForTest()
	runtime.GC()
	runtime.GC()
	for _, r := range roots {
		x := runtime.StacklessCoroParkedFramesForTest(r.scheduler)
		want := 0
		if r.reserved {
			want = 1
		}
		if x.FreeFrames != 0 || x.FreeBytes != 0 || x.Reserved != want || x.CachedFrames != want {
			t.Fatalf("parked reservation/idle ownership changed: %+v, want %d reserved", x, want)
		}
	}
	if delta := after - before; delta > 8 {
		t.Fatalf("64 parked roots allocated %d native contexts, want a bounded parked set", delta)
	}
}
