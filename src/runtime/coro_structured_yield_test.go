// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime_test

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
)

type stacklessCoroYieldAwaitFrame struct {
	step   int
	action uint8
	total  *int
}

func stacklessCoroYieldAwaitResume(ctx unsafe.Pointer) uint8 {
	f := (*stacklessCoroYieldAwaitFrame)(runtime.FrameStacklessCoroForTest(ctx))
	*f.total++
	if f.step == 0 {
		f.step++
		return f.action
	}
	return runtime.StacklessCoroActionComplete
}

func TestStacklessCoroNativeStructuredYieldThreads(t *testing.T) {
	const child = "GO_CORO_STRUCTURED_YIELD_THREADS"
	if os.Getenv(child) == "" {
		for _, action := range []uint8{runtime.StacklessCoroActionYield, runtime.StacklessCoroActionHandoff} {
			cmd := exec.Command(os.Args[0], "-test.run=^TestStacklessCoroNativeStructuredYieldThreads$", "-test.count=1")
			cmd.Env = append(os.Environ(), child+"="+strconv.Itoa(int(action)), "GOMAXPROCS=1", "GOGC=100")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("structured yield child action=%d: %v\n%s", action, err, output)
			}
		}
		return
	}
	value, err := strconv.Atoi(os.Getenv(child))
	if err != nil || (uint8(value) != runtime.StacklessCoroActionYield && uint8(value) != runtime.StacklessCoroActionHandoff) {
		t.Fatal("invalid child action")
	}
	const roots = 64
	before, _ := runtime.ThreadCreateProfile(nil)
	var wg sync.WaitGroup
	var native, completed atomic.Int32
	for range roots {
		wg.Go(func() {
			step, total := 0, 0
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				if step == 0 {
					if active, _, _, _, _, _ := runtime.StacklessCoroNativeStackForTest(); active {
						native.Add(1)
					}
					step++
					frame := &stacklessCoroYieldAwaitFrame{action: uint8(value), total: &total}
					runtime.AwaitStacklessCoroFrameForTest(ctx, unsafe.Pointer(frame), stacklessCoroYieldAwaitResume)
					return runtime.StacklessCoroActionWait
				}
				return runtime.StacklessCoroActionComplete
			})
			if total == 2 {
				completed.Add(1)
			}
		})
	}
	wg.Wait()
	after, _ := runtime.ThreadCreateProfile(nil)
	if got := native.Load(); got != roots {
		t.Fatalf("native roots = %d, want %d", got, roots)
	}
	if got := completed.Load(); got != roots {
		t.Fatalf("completed await frames = %d, want %d", got, roots)
	}
	if delta := after - before; delta > 8 {
		t.Fatalf("structured yields created %d thread records for %d roots", delta, roots)
	}
}
