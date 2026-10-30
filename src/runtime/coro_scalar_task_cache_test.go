// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package runtime_test

import (
	"bytes"
	"internal/race"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"testing"
	"unsafe"
	"weak"
)

func TestStacklessCoroScalarTaskCache(t *testing.T) {
	if race.Enabled {
		t.Skip("scalar chunk identities stay distinct under race")
	}
	old := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(old)
	runtime.GC()
	runtime.GC()
	if !runtime.CheckStacklessCoroScalarTaskCacheForTest() {
		t.Fatal("scalar block membership, partial completion, clearing, group reuse or lifetime exclusion failed")
	}
}

//go:noinline
func scalarTaskCacheWeakPopulation() ([]weak.Pointer[byte], unsafe.Pointer, unsafe.Pointer) {
	tokens, head, interior := runtime.StacklessCoroScalarTaskCachePopulationForTest(48)
	refs := make([]weak.Pointer[byte], len(tokens))
	for i, p := range tokens {
		refs[i] = weak.Make((*byte)(p))
	}
	return refs, head, interior
}

//go:noinline
func scalarTaskCacheAlive(refs []weak.Pointer[byte]) int {
	n := 0
	for _, r := range refs {
		if r.Value() != nil {
			n++
		}
	}
	return n
}
func TestStacklessCoroScalarTaskCacheGC(t *testing.T) {
	if race.Enabled {
		t.Skip("scalar chunk cache excluded under race")
	}
	old := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(old)
	runtime.GC()
	runtime.GC()
	refs, head, interior := scalarTaskCacheWeakPopulation()
	runtime.GC()
	if alive := scalarTaskCacheAlive(refs); alive != len(refs) {
		t.Fatalf("firstGC alive=%d,want%d", alive, len(refs))
	}
	runtime.GC()
	if runtime.StacklessCoroScalarTaskCacheNextForTest(head) != nil {
		t.Fatal("expired head retained old group link")
	}
	if alive := scalarTaskCacheAlive(refs); alive > 64 || alive < 4 {
		t.Fatalf("stale interior retained %d tasks,want4..64", alive)
	}
	runtime.KeepAlive(head)
	runtime.KeepAlive(interior)
}
func TestStacklessCoroScalarTaskCacheBudget(t *testing.T) {
	if race.Enabled {
		t.Skip("scalar chunk cache excluded under race")
	}
	old := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(old)
	runtime.GC()
	runtime.GC()
	defer func() { runtime.GC(); runtime.GC() }()
	const cap = 2048 * 16
	count, bytes := runtime.StacklessCoroScalarTaskCacheFillForTest(cap + 1024)
	if count != cap || bytes > 16<<20 {
		t.Fatalf("scalar group cap count=%d bytes=%d", count, bytes)
	}
	runtime.GC()
	bytes = runtime.StacklessCoroScalarTaskCacheFillChannelsForTest()
	if bytes > 16<<20 || bytes+112 <= 16<<20 {
		t.Fatalf("shared byte cap=%d", bytes)
	}
	after, got := runtime.StacklessCoroScalarTaskCacheFillForTest(1024)
	if after != count || got != bytes {
		t.Fatalf("scalar bypassed shared byte cap count=%d bytes=%d,want%d/%d", after, got, count, bytes)
	}
}
func TestStacklessCoroScalarTaskCacheConcurrent(t *testing.T) {
	if race.Enabled {
		t.Skip("scalar chunk cache excluded under race")
	}
	if !runtime.CheckStacklessCoroScalarTaskCacheConcurrentForTest() {
		t.Fatal("concurrent chunk ownership or GC/budget failed")
	}
}
func TestStacklessCoroScalarTaskCacheRaceNoop(t *testing.T) {
	if !race.Enabled {
		t.Skip("race fallback probe")
	}
	if !runtime.CheckStacklessCoroScalarTaskCacheRaceNoopForTest() {
		t.Fatal("scalar cache changed race fallback")
	}
}
func TestStacklessCoroScalarTaskCacheFatal(t *testing.T) {
	if race.Enabled {
		t.Skip("scalar chunk cache excluded under race")
	}
	if mode := os.Getenv("GO_CORO_SCALAR_TASK_CACHE_FATAL"); mode != "" {
		runtime.StacklessCoroScalarTaskCacheFatalForTest(mode)
		return
	}
	for _, tc := range []struct{ mode, message string }{{"origin", "origin"}, {"admission", "admission"}, {"index", "group"}, {"head", "cache"}} {
		t.Run(tc.mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestStacklessCoroScalarTaskCacheFatal$", "-test.count=1")
			cmd.Env = append(os.Environ(), "GO_CORO_SCALAR_TASK_CACHE_FATAL="+tc.mode, "GOGC=off")
			out, err := cmd.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 2 || !bytes.Contains(out, []byte("runtime: invalid stackless coroutine scalar-task "+tc.message)) {
				t.Fatalf("scalar %s: err=%v output=%s", tc.mode, err, out)
			}
		})
	}
}

func TestStacklessCoroScalarTaskCacheMixed(t *testing.T) {
	if race.Enabled {
		t.Skip("scalar chunk cache excluded under race")
	}
	old := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(old)
	runtime.GC()
	runtime.GC()
	if !runtime.CheckStacklessCoroMixedScalarSlotsForTest() {
		t.Fatal("mixed scalar boundary, slot isolation, independent typed frame or mode preservation failed")
	}
}
