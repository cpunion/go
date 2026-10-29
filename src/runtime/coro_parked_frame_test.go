// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime_test

import (
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"unsafe"
)

func TestStacklessCoroNativeParkedFrameOwnership(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	gcPercent := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gcPercent)
	runtime.StacklessCoroClearNativeTaskCachesForTest()
	ready := make(chan unsafe.Pointer, 1)
	release := make(chan int)
	done := make(chan struct{})
	var total int
	go func() {
		defer close(done)
		state := 0
		var held unsafe.Pointer
		var bypassed atomic.Int32
		var value int
		var ok bool
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				child := &stacklessCoroDeepFrameCacheTestFrame{
					depth: 2 * runtime.StacklessCoroTaskCacheSize, bypassed: &bypassed,
				}
				runtime.AwaitStacklessCoroFrameForTest(ctx, unsafe.Pointer(child), stacklessCoroDeepFrameCacheResume)
				return runtime.StacklessCoroActionWait
			case 1:
				state = 2
				held = newStacklessCoroFrameCacheTestFrame(ctx, stacklessCoroFrameCacheResume, 137, &total)
				ready <- runtime.SchedulerStacklessCoroForTest(ctx)
				runtime.RecvIntStacklessCoroForTest(ctx, release, &value, &ok)
				return runtime.StacklessCoroActionWait
			case 2:
				state = 3
				runtime.AwaitStacklessCoroFrameForTest(ctx, held, stacklessCoroFrameCacheResume)
				held = nil
				return runtime.StacklessCoroActionWait
			case 3:
				return runtime.StacklessCoroActionComplete
			}
			return runtime.StacklessCoroActionInvalid
		})
	}()
	scheduler := <-ready
	defer func() {
		close(release)
		<-done
		if total != 137 {
			t.Errorf("reserved frame resumed with total %d, want 137", total)
		}
	}()
	before := runtime.StacklessCoroParkedFramesForTest(scheduler)
	warm, shared := runtime.StacklessCoroNativeIdleStackBytesForTest()
	t.Logf("parked before GC: %+v; idle stack bytes warm=%d shared=%d", before, warm, shared)
	if before.FreeFrames == 0 || before.Reserved != 1 {
		t.Fatalf("missing free/reserved ownership: %+v", before)
	}
	runtime.GC()
	first := runtime.StacklessCoroParkedFramesForTest(scheduler)
	t.Logf("parked first GC: %+v", first)
	if first != before {
		t.Fatalf("first GC changed recently parked cache: before=%+v after=%+v", before, first)
	}
	runtime.GC()
	after := runtime.StacklessCoroParkedFramesForTest(scheduler)
	warm, shared = runtime.StacklessCoroNativeIdleStackBytesForTest()
	t.Logf("parked second GC: %+v; idle stack bytes warm=%d shared=%d", after, warm, shared)
	if after.FreeFrames != 0 || after.FreeBytes != 0 || after.Reserved != before.Reserved ||
		after.CachedFrames != before.CachedFrames-before.FreeFrames ||
		after.CachedBytes != before.CachedBytes-before.FreeBytes {
		t.Fatalf("idle cache did not expire independently of reserved frames: before=%+v after=%+v", before, after)
	}
}

func TestStacklessCoroNativeIdleSchedulerStackGC(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	gcPercent := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gcPercent)
	runtime.GC()
	runtime.GC()
	run := func() {
		runtime.RunStacklessCoroForTest(func(unsafe.Pointer) uint8 {
			if active, _, _, _, _, _ := runtime.StacklessCoroNativeStackForTest(); !active {
				t.Fatal("refilled context did not enter a native episode")
			}
			return runtime.StacklessCoroActionComplete
		})
	}
	run()
	contexts := runtime.StacklessCoroNativePoolForTest()
	warm, _ := runtime.StacklessCoroNativeIdleStackBytesForTest()
	if warm == 0 {
		t.Fatal("completed context kept no warm scheduler stack")
	}
	runtime.GC()
	first, shared := runtime.StacklessCoroNativeIdleStackBytesForTest()
	if first != warm || shared != 0 {
		t.Fatalf("first GC warm=%d shared=%d, want recent warm=%d and no detached stacks", first, shared, warm)
	}
	runtime.GC()
	warm, shared = runtime.StacklessCoroNativeIdleStackBytesForTest()
	if warm != 0 || shared != 0 {
		t.Fatalf("idle native stacks survived second GC: warm=%d shared=%d", warm, shared)
	}
	if runtime.StacklessCoroNativePoolForTest() != contexts {
		t.Fatal("idle stack expiry replaced executor identities")
	}
	run()
	if warm, _ := runtime.StacklessCoroNativeIdleStackBytesForTest(); warm == 0 {
		t.Fatal("completed episode did not retain its refilled stack")
	}
}
