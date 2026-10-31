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
	"sync"
	"testing"
	"time"
	"unsafe"
	"weak"
)

//go:noinline
func channelCacheWeakPopulation(t *testing.T) ([]weak.Pointer[byte], unsafe.Pointer) {
	tokens := make([]unsafe.Pointer, 4096)
	channelPopulationRound(t, tokens)
	refs := make([]weak.Pointer[byte], len(tokens))
	for i, token := range tokens {
		refs[i] = weak.Make((*byte)(token))
	}
	return refs, runtime.StacklessCoroChannelCacheHeadForTest()
}

//go:noinline
func channelCacheWeakAlive(refs []weak.Pointer[byte]) int {
	alive := 0
	for _, ref := range refs {
		if ref.Value() != nil {
			alive++
		}
	}
	return alive
}

func TestStacklessCoroNativeChannelCacheLifecycle(t *testing.T) {
	const child = "GO_CORO_CHANNEL_CACHE_LIFECYCLE"
	mode := os.Getenv(child)
	if mode == "" {
		for _, mode := range []string{"idle", "coalesced", "forced-pair", "bound", "kinds", "concurrent"} {
			t.Run(mode, func(t *testing.T) {
				cmd := exec.Command(os.Args[0], "-test.run=^TestStacklessCoroNativeChannelCacheLifecycle$", "-test.count=1", "-test.timeout=3m")
				cmd.Env = append(os.Environ(), child+"="+mode, "GOMAXPROCS=1", "GOGC=off")
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("channel cache child: %v\n%s", err, output)
				}
			})
		}
		return
	}
	procs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(procs)
	gc := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gc)
	check := func() runtime.StacklessCoroChannelCacheStateForTest {
		t.Helper()
		s := runtime.StacklessCoroChannelCacheForTest()
		if s.Invalid || s.Charge != 80 || s.Limit != 16<<20 || uintptr(s.Young+s.Old)*s.Charge+s.Metadata > s.Limit || s.Young+s.Old > s.Capacity {
			t.Fatalf("invalid actual channel cache: %+v", s)
		}
		return s
	}
	switch mode {
	case "coalesced", "forced-pair":
		// Establish a completed cycle for the real time-based automatic
		// trigger, then populate an independently rooted completed cache.
		runtime.GC()
		refs, keeper := channelCacheWeakPopulation(t)
		before := check()
		if before.Young < 3000 || before.Old != 0 {
			t.Fatalf("missing cached population before pending GC: %+v", before)
		}
		pending, cycles := runtime.StacklessCoroPendingGCForTest(mode == "coalesced")
		if !pending || cycles != 2 {
			t.Fatalf("pending mark/full request = (%t, %d cycles), want (true, 2)", pending, cycles)
		}
		first := check()
		if mode == "coalesced" {
			if first.Young != 0 || first.Old != before.Young || channelCacheWeakAlive(refs) < int(first.Old) {
				t.Fatalf("one user request aged the automatic/forced pair twice: before=%+v after=%+v", before, first)
			}
			runtime.GC()
		}
		if after := check(); after.Young != 0 || after.Old != 0 {
			t.Fatalf("consecutive user requests did not expire cache: %+v", after)
		}
		if alive := channelCacheWeakAlive(refs); alive > 512 {
			t.Fatalf("expired pending-GC population stayed rooted: %d", alive)
		}
		runtime.KeepAlive(keeper)
	case "idle":
		refs, keeper := channelCacheWeakPopulation(t)
		// Retain an interior entry as well as the newest group head. A stale
		// producer must not keep the entire expired population alive.
		interior := runtime.StacklessCoroReleasedOperationNextForTest(keeper)
		before := check()
		if before.Young < 3000 || before.Old != 0 || keeper == nil {
			t.Fatalf("completed actual channel population not cached: %+v", before)
		}
		runtime.GC()
		first := check()
		if first.Young != 0 || first.Old != before.Young || channelCacheWeakAlive(refs) < int(first.Old) {
			t.Fatalf("first idle GC lost reusable objects: %+v", first)
		}
		runtime.GC()
		second := check()
		if second.Young != 0 || second.Old != 0 || runtime.StacklessCoroReleasedOperationNextForTest(keeper) != nil {
			t.Fatalf("second idle GC retained old links: %+v", second)
		}
		if alive := channelCacheWeakAlive(refs); alive > 512 {
			t.Fatalf("one retained operation pinned expired population: %d alive", alive)
		}
		runtime.KeepAlive(keeper)
		runtime.KeepAlive(interior)
	case "bound":
		before := check()
		tokens := make([]unsafe.Pointer, int(before.Capacity)+1024)
		channelPopulationRound(t, tokens)
		after := check()
		if after.Young+after.Old != after.Capacity {
			t.Fatalf("actual population did not exercise byte cap: %+v", after)
		}
		runtime.GC()
		runtime.GC()
		if s := check(); s.Young != 0 || s.Old != 0 {
			t.Fatalf("saturated population survived idle expiry: %+v", s)
		}
		runtime.KeepAlive(tokens)
	case "kinds":
		channelPopulationRound(t, make([]unsafe.Pointer, 4096))
		before := check()
		runtime.DrainStacklessCoroSharedOperationsForTest()
		step := 0
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if step == 0 {
				step++
				timer := runtime.StartSleepStacklessCoroForTest(ctx, int64(time.Hour))
				if !runtime.CancelSleepStacklessCoroForTest(timer) {
					t.Fatal("timer cancellation failed")
				}
				return runtime.StacklessCoroActionWait
			}
			return runtime.StacklessCoroActionComplete
		})
		if after := check(); after.Young != before.Young || after.Old != before.Old {
			t.Fatalf("timer consumed compact-only overflow: before=%+v after=%+v", before, after)
		}
	case "concurrent":
		runtime.GOMAXPROCS(4)
		debug.SetGCPercent(1)
		var workers, collector sync.WaitGroup
		stop := make(chan struct{})
		collector.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					runtime.GC()
				}
			}
		})
		for range 4 {
			workers.Go(func() {
				for range 8 {
					channelPopulationRound(t, make([]unsafe.Pointer, 512))
					check()
				}
			})
		}
		workers.Wait()
		close(stop)
		collector.Wait()
		debug.SetGCPercent(-1)
		runtime.GC()
		runtime.GC()
		if s := check(); s.Young != 0 || s.Old != 0 {
			t.Fatalf("concurrent populations did not expire: %+v", s)
		}
	default:
		t.Fatal("invalid lifecycle child mode")
	}
}
