// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && !coropullcompare && (darwin || linux)

package runtime_test

import (
	"context"
	"fmt"
	"internal/testenv"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStacklessCoroLocalFusedAwait(t *testing.T) {
	const helper = "GO_RUNTIME_CORO_FUSED_AWAIT_LOCKED"
	locked := os.Getenv(helper) == "1"
	for mode := 0; mode < 4; mode++ {
		short, runnable := mode&1 != 0, mode&2 != 0
		last := 14 // Six direct frames followed by eight chunk elements.
		if short {
			last = 10
		}
		for allocation := 1; allocation <= last; allocation++ {
			t.Run(fmt.Sprintf("mode%d/allocation%d", mode, allocation), func(t *testing.T) {
				if !runtime.CheckStacklessCoroLocalFusedAwaitForTest(uint8(allocation), short, runnable, locked, "") {
					t.Fatal("await changed frame ancestry, resume identity, shared caches or fairness")
				}
			})
		}
	}
	if locked {
		return
	}
	t.Run("scheduler-locked", func(t *testing.T) {
		testenv.MustHaveExec(t)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := testenv.CommandContext(t, ctx, os.Args[0], "-test.run=^TestStacklessCoroLocalFusedAwait$")
		cmd.Env = append(os.Environ(), helper+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("uncached await acquired the scheduler lock: %v\n%s", err, out)
		}
	})
}

func TestStacklessCoroLocalFusedAwaitFallbacks(t *testing.T) {
	for mode := 0; mode < 4; mode++ {
		for _, fallback := range []string{"pending-only", "cached-child", "owned-parent",
			"zero-allocation", "unlinked-parent", "unvalidated-parent", "same-resume"} {
			t.Run(fmt.Sprintf("mode%d/%s", mode, fallback), func(t *testing.T) {
				if !runtime.CheckStacklessCoroLocalFusedAwaitForTest(7, mode&1 != 0, mode&2 != 0, false, fallback) {
					t.Fatal("fallback lost ownership, reservation or resume state")
				}
			})
		}
	}
}

func TestStacklessCoroLocalFusedAwaitInvariants(t *testing.T) {
	const helper = "GO_RUNTIME_CORO_FUSED_AWAIT_INVALID"
	if mode := os.Getenv(helper); mode != "" {
		runtime.CheckStacklessCoroLocalFusedAwaitForTest(7, true, false, false, mode)
		t.Fatal("invalid await unexpectedly accepted")
	}
	testenv.MustHaveExec(t)
	for mode, diagnostic := range map[string]string{
		"requested":       "unconsumed stackless coroutine fused-frame request",
		"missing-pending": "unconsumed stackless coroutine fused frame chunk",
		"waiting":         "fused-frame await outside resume",
		"not-resuming":    "fused-frame await outside resume",
		"ready":           "fused-frame await outside resume",
		"switch":          "fused-frame await outside resume",
		"panic":           "fused-frame await outside resume",
		"goexit":          "fused-frame await outside resume",
		"nil-resume":      "fused-frame await outside resume",
		"child-parent":    "invalid initialized stackless coroutine fused frame",
		"child-owner":     "invalid initialized stackless coroutine fused frame",
		"child-marker":    "invalid initialized stackless coroutine fused frame",
		"child-resume":    "initialized stackless coroutine fused resume",
		"invalid-marker":  "invalid stackless coroutine parent fused-frame marker",
	} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := testenv.CommandContext(t, ctx, os.Args[0], "-test.run=^TestStacklessCoroLocalFusedAwaitInvariants$")
			cmd.Env = append(os.Environ(), helper+"="+mode)
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), diagnostic) {
				t.Fatalf("wrong invalid-state outcome: %v\n%s", err, out)
			}
		})
	}
}
