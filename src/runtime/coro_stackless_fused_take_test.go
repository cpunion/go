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

func TestStacklessCoroLocalFusedTake(t *testing.T) {
	const helper = "GO_RUNTIME_CORO_FUSED_TAKE_LOCKED"
	locked := os.Getenv(helper) == "1"
	for mode := 0; mode < 16; mode++ {
		short, heterogeneous := mode&1 != 0, mode&2 != 0
		requested, runnable := mode&4 != 0, mode&8 != 0
		if heterogeneous && !requested {
			continue // A differing resume needs an explicit compatibility proof.
		}
		elements := 7
		if short {
			elements = 3
		}
		for element := 0; element < elements; element++ {
			t.Run(fmt.Sprintf("mode%d/element%d", mode, element), func(t *testing.T) {
				if !runtime.CheckStacklessCoroLocalFusedTakeForTest(element, short,
					heterogeneous, requested, runnable, locked, "", "") {
					t.Fatal("reservation changed chunk, task ownership, shared caches or queue state")
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
		cmd := testenv.CommandContext(t, ctx, os.Args[0], "-test.run=^TestStacklessCoroLocalFusedTake$")
		cmd.Env = append(os.Environ(), helper+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("local chunk reservation acquired the scheduler lock: %v\n%s", err, out)
		}
	})
}

func TestStacklessCoroLocalFusedTakeFallbacks(t *testing.T) {
	for mode := 0; mode < 8; mode++ {
		for _, fallback := range []string{"last", "owned", "unproved"} {
			t.Run(fmt.Sprintf("mode%d/%s", mode, fallback), func(t *testing.T) {
				if !runtime.CheckStacklessCoroLocalFusedTakeForTest(0, mode&1 != 0,
					mode&2 != 0, true, mode&4 != 0, false, "", fallback) {
					t.Fatal("shared cache reservation was bypassed at a boundary or without exclusive chunk proof")
				}
			})
		}
	}
}

func TestStacklessCoroLocalFusedTakeInvariants(t *testing.T) {
	const helper = "GO_RUNTIME_CORO_FUSED_TAKE_INVALID"
	if mode := os.Getenv(helper); mode != "" {
		runtime.CheckStacklessCoroLocalFusedTakeForTest(0, false, true,
			true, false, false, mode, "")
		t.Fatal("invalid task state unexpectedly accepted")
	}
	testenv.MustHaveExec(t)
	for _, mode := range []string{"waiting", "not-resuming", "ready", "pending", "switch", "panic", "goexit"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := testenv.CommandContext(t, ctx, os.Args[0], "-test.run=^TestStacklessCoroLocalFusedTakeInvariants$")
			cmd.Env = append(os.Environ(), helper+"="+mode)
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "fused-frame reservation outside resume") {
				t.Fatalf("wrong invalid-state outcome: %v\n%s", err, out)
			}
		})
	}
}
